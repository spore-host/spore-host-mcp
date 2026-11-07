package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/scheduler"
	"github.com/aws/aws-sdk-go-v2/service/sns"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/spore-host/lagotto/pkg/arm"
	"github.com/spore-host/lagotto/pkg/deploy"
	"github.com/spore-host/lagotto/pkg/watcher"
)

// Until-landed watches and their acknowledgement (#92, lagotto#239).
//
// An until-landed watch has no TTL. It keeps hunting behind a rolling
// acknowledgement deadline of (pause-after+1) × its remind interval (3 × 7d =
// 21d by default), stored in expires_at. The hosted poller reminds its owner on
// its notify channels, and once the deadline passes unacknowledged it pauses the
// watch: status expired, expired_reason "unacknowledged". lagotto_ack (like
// `lagotto ack`) moves the deadline a full window ahead from now, and resumes a
// paused watch, re-arming the poll schedule the poller may have switched off.
//
// Everything that decides what is allowed or stored is lagotto's exported code:
// watcher.NewWatch (the deadline and retention horizon), ValidateUntilLanded
// (not notify, a channel required, the remind bounds), Watch.AckDeadline, the
// Store's ExtendWatch / ExpireWatch, and pkg/arm's poll-schedule and email
// steps. What this file adds is the MCP equivalent of the CLI's confirmation
// prompt (an explicit confirm argument) and the rendering.

// mcpStaticPriceReason is why this server's cost-exposure lines use the static
// price list (truffle's, labelled with its collection date).
const mcpStaticPriceReason = "spore-host-mcp does not query the live AWS Price List for watch exposure (that would need pricing:GetProducts, which this server does not ask you for)"

// lagottoArgWording rewrites the CLI flag names in lagotto's until-landed
// refusals to this tool's argument names, so the assistant is told which
// argument to change.
var lagottoArgWording = strings.NewReplacer(
	"--until-landed", "until_landed",
	"--action ", "action=",
	"--notify", "notify",
	"--remind", "remind",
	"--ttl", "ttl",
)

// watchLifetime parses lagotto_watch's lifetime arguments: a ttl, or
// until_landed with an optional remind interval. It mirrors the CLI's
// parseTTLFlag and parseUntilLandedFlags: a ttl must be positive, until_landed
// and ttl are mutually exclusive, and remind means nothing without
// until_landed. The remind bounds are checked by ValidateUntilLanded.
func watchLifetime(ttlStr string, untilLanded bool, remindStr string) (watcher.Lifetime, error) {
	if !untilLanded {
		if remindStr != "" {
			return watcher.Lifetime{}, errors.New("remind only applies to until_landed watches")
		}
		if ttlStr == "" {
			ttlStr = "24h"
		}
		ttl, err := parseLagottoDuration(ttlStr)
		if err != nil {
			return watcher.Lifetime{}, fmt.Errorf("invalid ttl %q: %v", ttlStr, err)
		}
		if ttl <= 0 {
			return watcher.Lifetime{}, fmt.Errorf("invalid ttl %q: it must be greater than zero (to keep hunting until a launch lands, use until_landed: true instead of ttl)", ttlStr)
		}
		return watcher.Lifetime{TTL: ttl}, nil
	}
	if ttlStr != "" {
		return watcher.Lifetime{}, errors.New("until_landed and ttl are mutually exclusive: an until-landed watch has no TTL, it pauses unless acknowledged (see remind)")
	}
	remind := watcher.DefaultRemindEvery
	if remindStr != "" {
		d, err := parseLagottoDuration(remindStr)
		if err != nil {
			return watcher.Lifetime{}, fmt.Errorf("invalid remind %q: %v", remindStr, err)
		}
		remind = d
	}
	return watcher.Lifetime{UntilLanded: true, RemindEvery: remind, RemindPauseAfter: watcher.DefaultRemindPauseAfter}, nil
}

// parseLagottoDuration accepts both Go durations (24h) and lagotto's short
// form (7d, 1w).
func parseLagottoDuration(s string) (time.Duration, error) {
	d, err := time.ParseDuration(s)
	if err == nil {
		return d, nil
	}
	return watcher.ParseDuration(s)
}

// ackWindow is how far one acknowledgement moves a deadline of life.
func ackWindow(life watcher.Lifetime) time.Duration {
	return time.Duration(life.RemindPauseAfter+1) * life.RemindEvery
}

// untilLandedConfirmRefusal is the refusal for an until-landed hold watch
// created without confirm: true, the MCP equivalent of the CLI's prompt
// (lagotto#239, owner decision 9). With no TTL it may reserve billable capacity
// at any point in the next weeks, so the caller must say so explicitly. exp is
// the watch's cost exposure.
func untilLandedConfirmRefusal(pattern string, life watcher.Lifetime, exp *watcher.Exposure) error {
	what := fmt.Sprintf("create a billable capacity reservation for %s (billed for %d minutes whether used or not)",
		pattern, int(watcher.HoldDuration/time.Minute))
	msg := fmt.Sprintf("until_landed with action=hold needs confirmation: it may %s at any time until it lands", what)
	if exp != nil {
		msg += " (" + exp.Summary + ")"
	}
	return fmt.Errorf("%s. It pauses itself unless acknowledged with lagotto_ack within %s (%d × the %s remind interval). "+
		"Pass confirm: true to create it; no watch was created",
		msg, watcher.FormatSpan(ackWindow(life)), life.RemindPauseAfter+1, watcher.FormatSpan(life.RemindEvery))
}

// undeliverableUntilLandedWarning is the CLI's warning for an until-landed
// watch whose only channels are email addresses still awaiting confirmation:
// until one confirms, its reminders reach nobody and it will simply pause at
// its deadline. "" when there is nothing to warn about.
func undeliverableUntilLandedWarning(w *watcher.Watch) string {
	if !w.UntilLanded || len(w.NotifyChannels) == 0 {
		return ""
	}
	for _, ch := range w.NotifyChannels {
		if ch.Type != "email" || ch.Subscription == watcher.SubConfirmed {
			return ""
		}
	}
	return fmt.Sprintf("none of this watch's email addresses has confirmed its subscription yet. Until one does, "+
		"nothing it sends reaches you, and it pauses at %s unless you acknowledge it with lagotto_ack.",
		w.ExpiresAt.Format(time.RFC3339))
}

// newSubnetCoverageChecker builds lagotto_watch's default-subnet coverage
// check (lagotto#256), as `lagotto watch` does. A test seam, so the fake-AWS
// tests needn't serve EC2.
var newSubnetCoverageChecker = watcher.NewSubnetCoverageChecker

// expiresColumn is lagotto_list's expiry for a watch: its RFC 3339 expiry, or
// for an until-landed watch the CLI's list wording, "until landed (ack in 4d
// 0h 0m)" while active and "paused (unacked)" once its deadline passed.
func expiresColumn(w *watcher.Watch, now time.Time) string {
	if !w.UntilLanded {
		return w.ExpiresAt.Format(time.RFC3339)
	}
	if w.Paused() {
		return "paused (unacked): resume it with lagotto_ack"
	}
	if w.Status != watcher.StatusActive {
		return "until landed"
	}
	left := w.ExpiresAt.Sub(now)
	if left <= 0 {
		return "until landed (ack due)"
	}
	return fmt.Sprintf("until landed (ack in %s, by %s)", formatDuration(left), w.ExpiresAt.Format(time.RFC3339))
}

// writeExpires writes lagotto_status's expiry lines, as `lagotto status` does:
// an until-landed watch shows its acknowledgement deadline and how to move it,
// a paused one why it stopped and how to resume it, and any other expired
// watch with a recorded reason shows the reason.
func writeExpires(sb *strings.Builder, w *watcher.Watch, now time.Time) {
	if !w.UntilLanded {
		fmt.Fprintf(sb, "Expires:  %s\n", w.ExpiresAt.Format(time.RFC3339))
		if w.Status == watcher.StatusExpired && w.ExpiredReason != "" {
			fmt.Fprintf(sb, "Expired reason: %s\n", w.ExpiredReason)
		}
		return
	}
	remind := watcher.FormatSpan(time.Duration(w.RemindEverySeconds) * time.Second)
	switch {
	case w.Paused():
		fmt.Fprintf(sb, "Expires:  until landed; PAUSED at %s, not acknowledged in time (expired_reason: %s)\n",
			w.ExpiresAt.Format(time.RFC3339), w.ExpiredReason)
		fmt.Fprintf(sb, "  Resume the hunt with lagotto_ack (watch_id %s), or 'lagotto ack %s'.\n", w.WatchID, w.WatchID)
	case w.Status == watcher.StatusActive:
		left := "ack due"
		if d := w.ExpiresAt.Sub(now); d > 0 {
			left = "in " + formatDuration(d)
		}
		fmt.Fprintf(sb, "Expires:  until landed; pauses %s (%s) unless acknowledged\n", w.ExpiresAt.Format(time.RFC3339), left)
		fmt.Fprintf(sb, "  Acknowledge with lagotto_ack (watch_id %s): each ack moves the deadline %s ahead (%d × the %s remind interval).\n",
			w.WatchID, watcher.FormatSpan(w.AckWindow()), w.RemindPauseAfter+1, remind)
	default:
		fmt.Fprintf(sb, "Expires:  until landed (deadline was %s)\n", w.ExpiresAt.Format(time.RFC3339))
	}
	if w.RemindersSent > 0 {
		fmt.Fprintf(sb, "Reminders sent: %d this window\n", w.RemindersSent)
	}
}

// writeHeartbeat writes lagotto_status's heartbeat (lagotto#239 phase 1): when
// the watch was last polled, how many attempts it has made, and its last error
// and last search error.
func writeHeartbeat(sb *strings.Builder, w *watcher.Watch, now time.Time) {
	if !w.LastPolledAt.IsZero() {
		fmt.Fprintf(sb, "Last polled: %s (%s ago)\n", w.LastPolledAt.Format(time.RFC3339), formatDuration(now.Sub(w.LastPolledAt)))
	}
	if w.Attempts > 0 || !w.LastAttemptAt.IsZero() {
		fmt.Fprintf(sb, "Attempts: %d", w.Attempts)
		if !w.LastAttemptAt.IsZero() {
			fmt.Fprintf(sb, " (last %s)", w.LastAttemptAt.Format(time.RFC3339))
		}
		sb.WriteString("\n")
	}
	if w.LastError != "" {
		fmt.Fprintf(sb, "Last error: %s", w.LastError)
		if !w.LastErrorAt.IsZero() {
			fmt.Fprintf(sb, " (at %s)", w.LastErrorAt.Format(time.RFC3339))
		}
		sb.WriteString("\n")
	}
	if w.LastPollError != "" {
		fmt.Fprintf(sb, "Last search error: %s", w.LastPollError)
		if !w.LastPollErrorAt.IsZero() {
			fmt.Fprintf(sb, " (at %s", w.LastPollErrorAt.Format(time.RFC3339))
			if w.LastPolledAt.After(w.LastPollErrorAt) {
				sb.WriteString("; the search has succeeded since")
			}
			sb.WriteString(")")
		}
		sb.WriteString("\n")
	}
}

// writeExposure writes an exposure's summary after label and its details
// indented beneath it. A nil exposure writes nothing.
func writeExposure(sb *strings.Builder, label string, e *watcher.Exposure) {
	if e == nil {
		return
	}
	pad := strings.Repeat(" ", len([]rune(label)))
	fmt.Fprintf(sb, "%s%s\n", label, e.Summary)
	for _, d := range e.Details {
		fmt.Fprintf(sb, "%s%s\n", pad, d)
	}
}

// handleLagottoAck is lagotto_ack, the MCP equivalent of `lagotto ack`
// (lagotto cmd/ack.go, cmd/extend.go extendOrAck): resolve the caller's watch,
// move its acknowledgement deadline a full window ahead, and for a paused
// (expired) watch resume it, re-arming the poll schedule and its email
// subscriptions as the CLI does.
func handleLagottoAck(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	watchID, _ := args["watch_id"].(string)
	watchID = strings.TrimSpace(watchID)
	if watchID == "" {
		return mcp.NewToolResultError("watch_id is required"), nil
	}

	store, cfg, caller, err := lagottoContext(ctx)
	if err != nil {
		return mcp.NewToolResultError("failed to connect to AWS: " + err.Error()), nil
	}
	w, err := store.GetWatch(ctx, watchID)
	if err != nil {
		return mcp.NewToolResultError("get watch: " + err.Error()), nil
	}
	// The same owner scope, and the same not-found for a watch the caller does
	// not own, as lagotto_status and the CLI's getWatchOwned.
	if w == nil || (w.UserID != "" && caller != "" && w.UserID != caller) {
		return mcp.NewToolResultError(fmt.Sprintf("watch %s not found", watchID)), nil
	}
	if w.Status != watcher.StatusActive && w.Status != watcher.StatusExpired {
		return mcp.NewToolResultError(fmt.Sprintf("watch %s is not active (status: %s)", watchID, w.Status)), nil
	}
	now := time.Now().UTC()
	newExpiry, ok := w.AckDeadline(now)
	if !ok {
		return mcp.NewToolResultError(fmt.Sprintf("watch %s is not an until-landed watch, so there is nothing to acknowledge; "+
			"use 'lagotto extend %s --ttl <duration>' to move its expiry", watchID, watchID)), nil
	}
	reactivate := w.Status == watcher.StatusExpired

	// A resumed watch is only polled if the poller's schedule, which the poller
	// switched off when it had nothing to poll, is switched back on (lagotto#231).
	// The CLI's order: preflight before the write, refusing if the schedule
	// can't be read; ExtendWatch (which also moves ttl_timestamp with the
	// deadline); then enable, putting the watch back as it was if that fails.
	sched := scheduler.NewFromConfig(cfg)
	armAction := "reactivate watch " + watchID
	var pollSched *scheduler.GetScheduleOutput
	if reactivate {
		if pollSched, err = arm.PreflightPollSchedule(ctx, sched, armAction); err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
	}
	if err := store.ExtendWatch(ctx, watchID, newExpiry, reactivate); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	enabled := false
	if reactivate {
		if enabled, err = arm.EnablePollScheduleIfOff(ctx, sched, pollSched); err != nil {
			restore := store.ExpireWatch(ctx, watchID, w.ExpiredReason)
			return mcp.NewToolResultError(arm.PollScheduleUpdateRefusal(armAction, "the watch is still expired", restore, err).Error()), nil
		}
	}

	auditLog("lagotto_ack", watchID, "", "")

	var sb strings.Builder
	if reactivate {
		fmt.Fprintf(&sb, "Reactivated expired watch %s\n", watchID)
	}
	fmt.Fprintf(&sb, "Acknowledged watch %s: it keeps hunting until it lands, and pauses at %s unless acknowledged again\n",
		watchID, newExpiry.Format(time.RFC3339))
	fmt.Fprintf(&sb, "  Window:  each ack moves the deadline %s ahead (%d × the %s remind interval).\n",
		watcher.FormatSpan(w.AckWindow()), w.RemindPauseAfter+1, watcher.FormatSpan(time.Duration(w.RemindEverySeconds)*time.Second))
	if reactivate {
		switch {
		case pollSched == nil:
			fmt.Fprintf(&sb, "  Poller:  no hosted poller in this account and region (schedule %s not found), so nothing will poll this watch until you run `lagotto deploy` here, or `lagotto poll --daemon` somewhere that stays up.\n", arm.PollScheduleName())
		case enabled:
			fmt.Fprintf(&sb, "  Poller:  switched the hosted poller's schedule %s back on (it turns itself off when no watches are active).\n", awssdk.ToString(pollSched.Name))
		default:
			fmt.Fprintf(&sb, "  Poller:  the hosted poller's schedule %s is on.\n", awssdk.ToString(pollSched.Name))
		}
		// A resumed watch's addresses may have been unsubscribed when it
		// paused (lagotto#240): subscribe them again. Best effort, as in the
		// CLI: the watch is resumed either way, and this says so if its emails
		// will not be delivered.
		for _, line := range resubscribeOnReactivate(ctx, cfg, w) {
			fmt.Fprintf(&sb, "  %s\n", line)
		}
	}
	return withSummary(ackSummary(w, newExpiry, now, reactivate, pollSched != nil), sb.String()), nil
}

// resubscribeOnReactivate subscribes a resumed watch's email addresses again,
// reusing any subscription they still have, and returns the lines to show:
// each address's state, any warning, or why its emails will not be delivered.
func resubscribeOnReactivate(ctx context.Context, cfg awssdk.Config, w *watcher.Watch) []string {
	if len(watcher.EmailAddresses(w)) == 0 {
		return nil
	}
	resumed := *w
	resumed.Status = watcher.StatusActive
	resumed.NotifyChannels = append([]watcher.NotifyChannel(nil), w.NotifyChannels...)
	topic := arm.ResolveAlertsTopic(ctx, os.Getenv, cfg.Region, accountOf(w.UserID), deploy.New(cfg))
	var subs arm.EmailSubscriber
	if topic.ARN != "" {
		subs = watcher.NewEmailSubscriptions(sns.NewFromConfig(cfg), topic.ARN)
	}
	warnings, err := arm.SubscribeWatchEmails(ctx, topic, subs, &resumed)
	var lines []string
	for _, warn := range warnings {
		lines = append(lines, "Warning: "+warn)
	}
	if err != nil {
		return append(lines, fmt.Sprintf("Warning: email notifications for %s will not be delivered: %s", w.WatchID,
			strings.TrimSuffix(err.Error(), "; no watch was created")))
	}
	for _, ch := range resumed.NotifyChannels {
		if ch.Type == "email" && ch.Subscription != "" {
			lines = append(lines, fmt.Sprintf("Notify:  email:%s  %s", ch.Target, ch.Subscription.Describe()))
		}
	}
	return lines
}

// ackSummary is lagotto_ack's summary line (#75).
func ackSummary(w *watcher.Watch, newExpiry, now time.Time, reactivate, pollerFound bool) string {
	line := fmt.Sprintf("✅ Acknowledged watch %s: %s hunts until it lands, pauses in %s unless acked again",
		w.WatchID, w.InstanceTypePattern, formatDuration(newExpiry.Sub(now)))
	if reactivate {
		line += "; resumed"
		if !pollerFound {
			line += ", but no hosted poller, so nothing polls it yet"
		}
	}
	return line
}
