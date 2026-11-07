package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/scheduler"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/spore-host/lagotto/pkg/arm"
	"github.com/spore-host/lagotto/pkg/watcher"
)

// Default lagotto DynamoDB table names — the same defaults the lagotto CLI uses
// (cmd/root.go: --watches-table / --history-table). The MCP server has no flag
// layer, so it reads/writes the conventional tables directly.
const (
	lagottoWatchesTable = "lagotto-watches"
	lagottoHistoryTable = "lagotto-match-history"
)

func registerLagottoTools(s *server.MCPServer) {
	s.AddTool(mcp.NewTool("lagotto_list",
		mcp.WithDescription("List your lagotto capacity watches. Returns each watch's project, owner, status, instance-type pattern, regions, action, its expiry (for an until-landed watch, its acknowledgement deadline, or 'paused (unacked)' once it paused), and (when it matched or gave up) the derived wait-to-acquire / time-to-give-up. Read-only. Only watches created by the calling AWS identity are shown."),
		mcp.WithString("project",
			mcp.Description("Only show watches with this project label."),
		),
		mcp.WithBoolean("all",
			mcp.Description("Show watches in all states. When false (default) only active watches are listed."),
			mcp.DefaultBool(false),
		),
	), handleLagottoList)

	s.AddTool(mcp.NewTool("lagotto_status",
		mcp.WithDescription("Show the full details of one lagotto watch by its ID, including project, owner, pattern, regions, action, spot/max-price, cost exposure, status, its expiry (for an until-landed watch, the acknowledgement deadline and how to move it with lagotto_ack, or why it paused), the heartbeat (last poll, attempts, last error), and the derived wait-to-acquire / time-to-give-up timings. Read-only. You must be the watch's owner."),
		mcp.WithString("watch_id",
			mcp.Description("The watch ID (e.g. w-1a2b3c4d)."),
			mcp.Required(),
		),
	), handleLagottoStatus)

	s.AddTool(mcp.NewTool("lagotto_watch",
		mcp.WithDescription("Create a lagotto capacity watch: lagotto polls for the requested instance-type capacity across regions and takes an action when it's found. Like `lagotto watch`, it switches the hosted poller's schedule back on if the poller turned it off while idle (needs scheduler:GetSchedule, scheduler:UpdateSchedule and iam:PassRole on the scheduler-invoke role), and says so when no hosted poller is deployed in the account and region, since then nothing polls the watch until `lagotto deploy` or `lagotto poll --daemon`. Supports action=notify (send a notification only — nothing billable) and action=hold (place an On-Demand Capacity Reservation to HOLD the capacity — this reserves billable EC2 capacity for as long as the reservation is held). action=spawn is not supported here (it needs a full spawn launch-config); use the lagotto CLI for that. The watch stops when it matches or its TTL elapses. With until_landed=true (action=hold only) it has no TTL: it keeps hunting until it lands, reminds you on its notify channels, and pauses itself unless acknowledged with lagotto_ack within 3 × the remind interval (21 days by default). Such a watch may reserve billable capacity at any point in the next weeks, so it needs a notify channel and an explicit confirm=true; without confirm it is refused with its cost exposure and nothing is created. A hold watch is refused if no AZ that offers the type has a default subnet in the regions it names, as `lagotto watch` does."),
		mcp.WithString("pattern",
			mcp.Description("Instance-type pattern to watch: an exact type (g5.xlarge), a wildcard (p5.*), or a comma-separated list (g6.4xlarge,g6.2xlarge) matching ANY listed rung."),
			mcp.Required(),
		),
		mcp.WithString("regions",
			mcp.Description("Comma-separated AWS regions to watch (e.g. us-east-1,us-west-2). Omit to watch all enabled regions."),
		),
		mcp.WithString("action",
			mcp.Description("Action on match: notify (default) or hold. hold reserves billable capacity via an On-Demand Capacity Reservation."),
			mcp.DefaultString("notify"),
		),
		mcp.WithString("project",
			mcp.Description("Optional project label for scoping (a shared-account poller can service only its own project's watches)."),
		),
		mcp.WithString("ttl",
			mcp.Description("How long to keep watching before giving up, e.g. 24h, 7d, 1w (default 24h when omitted). Units w/d/h/m/s. Must be greater than zero. Omit it with until_landed, which has no TTL."),
		),
		mcp.WithBoolean("until_landed",
			mcp.Description("Keep hunting with no TTL until the hold lands (action=hold only; refused for notify). Needs a notify channel and confirm=true. The watch pauses itself unless acknowledged with lagotto_ack within 3 × remind (21 days by default); lagotto_ack resumes a paused one. Mutually exclusive with ttl."),
			mcp.DefaultBool(false),
		),
		mcp.WithString("remind",
			mcp.Description("until_landed only: the reminder interval, e.g. 7d (default), between 1h and 30d. The watch pauses after 2 unacknowledged reminders, i.e. 3 × remind after creation or the last lagotto_ack."),
		),
		mcp.WithBoolean("confirm",
			mcp.Description("Required true to create an until_landed hold watch, which may reserve billable capacity at any time until it lands (the MCP equivalent of the CLI's confirmation prompt / --yes). Call once without it to see the refusal with the watch's cost exposure, then again with confirm=true once the user agrees. Ignored for other watches."),
			mcp.DefaultBool(false),
		),
		mcp.WithBoolean("spot",
			mcp.Description("Watch for Spot capacity instead of On-Demand."),
			mcp.DefaultBool(false),
		),
		mcp.WithNumber("max_price",
			mcp.Description("Maximum acceptable price per hour (0 = any)."),
			mcp.DefaultNumber(0),
		),
		mcp.WithString("notify",
			mcp.Description("Comma-separated notification channels (required for until_landed), each type:target — email:user@example.com, sns:arn:aws:sns:..., or webhook:https://... An email address is subscribed to the account's lagotto alerts topic (created by `lagotto deploy`; the watch is refused if there is none) and receives nothing until it clicks the confirmation link AWS emails it."),
		),
	), handleLagottoWatch)

	s.AddTool(mcp.NewTool("lagotto_ack",
		mcp.WithDescription("Acknowledge an until-landed lagotto watch so it keeps hunting, like `lagotto ack`. An until-landed watch has no TTL; it pauses itself unless acknowledged within 3 × its remind interval (21 days by default) of creation or the last ack. This moves the deadline a full window ahead from now. A paused watch (status expired, reason unacknowledged) is resumed: like the CLI, it switches the hosted poller's schedule back on if the poller turned it off (needs scheduler:GetSchedule, scheduler:UpdateSchedule and iam:PassRole on the scheduler-invoke role) and re-subscribes its email addresses; if the schedule cannot be switched on the watch is put back to paused and the call refused. Refused for a watch with a TTL (use `lagotto extend`) and for a watch that is not active or paused. Reserves nothing itself, but a resumed hold watch may again reserve billable capacity when it lands. You must be the watch's owner."),
		mcp.WithString("watch_id",
			mcp.Description("The watch ID (e.g. w-1a2b3c4d)."),
			mcp.Required(),
		),
	), handleLagottoAck)
}

// lagottoContext resolves an AWS config (via the same spawn client the rest of
// the server uses, so credential resolution is identical), a lagotto Store bound
// to the conventional tables, and the caller's ARN (watches are owner-scoped).
//
// It goes through the spawnClient seam rather than calling spawnclient.NewClient
// itself (#79): the seam is what the tests replace with a fake AWS, so a lagotto
// handler test that gets past argument validation talks to the fake, not to
// whatever account the developer's credentials name.
func lagottoContext(ctx context.Context) (*watcher.Store, awssdk.Config, string, error) {
	client, err := spawnClient(ctx)
	if err != nil {
		return nil, awssdk.Config{}, "", err
	}
	cfg := client.Config()
	identity, err := sts.NewFromConfig(cfg).GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return nil, awssdk.Config{}, "", fmt.Errorf("get caller identity: %w", err)
	}
	store := watcher.NewStore(cfg, lagottoWatchesTable, lagottoHistoryTable)
	return store, cfg, awssdk.ToString(identity.Arn), nil
}

// shortOwner renders a watch's UserID (the creator's caller ARN) as the compact
// trailing resource segment — e.g. "user/alice" — mirroring the lagotto CLI's
// list output. Returns "-" for an empty owner (legacy watches).
func shortOwner(arn string) string {
	if arn == "" {
		return "-"
	}
	if i := strings.LastIndex(arn, ":"); i >= 0 && i+1 < len(arn) {
		return arn[i+1:]
	}
	return arn
}

func displayRegions(regions []string) string {
	if len(regions) == 0 {
		return "(all enabled)"
	}
	return strings.Join(regions, ", ")
}

func handleLagottoList(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	project, _ := args["project"].(string)
	all, _ := args["all"].(bool)

	store, _, caller, err := lagottoContext(ctx)
	if err != nil {
		return mcp.NewToolResultError("failed to connect to AWS: " + err.Error()), nil
	}

	var statusFilter watcher.WatchStatus
	if !all {
		statusFilter = watcher.StatusActive
	}

	watches, err := store.ListWatchesByUser(ctx, caller, statusFilter)
	if err != nil {
		return mcp.NewToolResultError("list watches: " + err.Error()), nil
	}

	// Reuse the poller's filter logic for --project (the same code the CLI uses),
	// rather than re-implementing the label match.
	filter := &watcher.WatchFilter{Project: project}
	if !filter.Empty() {
		kept := watches[:0]
		for _, w := range watches {
			if filter.Matches(&w) {
				kept = append(kept, w)
			}
		}
		watches = kept
	}

	if len(watches) == 0 {
		scope := "active"
		if all {
			scope = "any-state"
		}
		if project != "" {
			return withSummary(fmt.Sprintf("No %s watches found for project %q.", scope, project), ""), nil
		}
		return withSummary(fmt.Sprintf("No %s watches found.", scope), ""), nil
	}

	now := time.Now().UTC()
	var sb strings.Builder
	fmt.Fprintf(&sb, "Found %d watch(es):\n\n", len(watches))
	for i := range watches {
		w := &watches[i]
		w.ComputeDurations()
		action := string(w.Action)
		if w.DesiredCount > 0 {
			action = fmt.Sprintf("%s×%d", w.Action, w.DesiredCount)
		}
		fmt.Fprintf(&sb, "• %s  [%s]\n", w.WatchID, w.Status)
		fmt.Fprintf(&sb, "  Project: %s  Owner: %s\n", dashIfEmpty(w.Project), shortOwner(w.UserID))
		fmt.Fprintf(&sb, "  Pattern: %s  Regions: %s\n", w.InstanceTypePattern, displayRegions(w.Regions))
		fmt.Fprintf(&sb, "  Action: %s  Spot: %v", action, w.Spot)
		if w.MaxPrice > 0 {
			fmt.Fprintf(&sb, "  Max price: $%.4f/hr", w.MaxPrice)
		}
		sb.WriteString("\n")
		if d, ok := w.WaitToAcquire(); ok {
			fmt.Fprintf(&sb, "  Wait to acquire: %s\n", watcher.FormatWait(d))
		}
		if d, ok := w.TimeToGiveUp(); ok {
			fmt.Fprintf(&sb, "  Time to give up: %s\n", watcher.FormatWait(d))
		}
		if w.Status == watcher.StatusExpired && w.ExpiredReason != "" {
			fmt.Fprintf(&sb, "  Expired reason: %s\n", w.ExpiredReason)
		}
		fmt.Fprintf(&sb, "  Expires: %s\n\n", expiresColumn(w, now))
	}
	return withSummary(watchListSummary(watches, all, project), sb.String()), nil
}

// summaryRegions is a watch's regions as a summary line words them.
func summaryRegions(regions []string) string {
	if len(regions) == 0 {
		return "all enabled regions"
	}
	return strings.Join(regions, ", ")
}

// watchListSummary is lagotto_list's summary line (#75): the count, then each
// watch's id, pattern and status, as many as fit.
func watchListSummary(watches []watcher.Watch, all bool, project string) string {
	head := fmt.Sprintf("%d active watch(es)", len(watches))
	if all {
		head = fmt.Sprintf("%d watch(es)", len(watches))
	}
	if project != "" {
		head += fmt.Sprintf(" in project %q", project)
	}
	head += ": "
	items := make([]string, 0, len(watches))
	for i := range watches {
		w := &watches[i]
		items = append(items, fmt.Sprintf("%s %s [%s]", w.WatchID, w.InstanceTypePattern, summaryStatus(w)))
	}
	return head + joinWithin(items, maxSummaryRunes-len([]rune(head)))
}

// watchSummary is lagotto_status's summary line (#75): the status, what it
// watches and where, then its last match (with the price) or how many
// matches and when it expires.
func watchSummary(w *watcher.Watch, now time.Time) string {
	purchase := "On-Demand"
	if w.Spot {
		purchase = "Spot"
	}
	line := fmt.Sprintf("Watch %s %s: %s %s in %s, action %s", w.WatchID, summaryStatus(w), w.InstanceTypePattern, purchase, summaryRegions(w.Regions), w.Action)
	if m := w.LastMatch; m != nil {
		where := m.AvailabilityZone
		if where == "" {
			where = m.Region
		}
		line += fmt.Sprintf("; last match %s in %s at $%.4f/hr", m.InstanceType, where, m.Price)
		return line
	}
	line += fmt.Sprintf("; %d match(es)", w.MatchCount)
	switch {
	case w.Paused():
		return line + ", lagotto_ack resumes it"
	case w.UntilLanded && w.Status == watcher.StatusActive:
		if left := w.ExpiresAt.Sub(now); left > 0 {
			return line + ", until landed, ack due in " + formatDuration(left)
		}
		return line + ", until landed, ack due now"
	}
	if w.Status == watcher.StatusActive && !w.ExpiresAt.IsZero() {
		if left := w.ExpiresAt.Sub(now); left > 0 {
			line += ", expires in " + formatDuration(left)
		}
	}
	return line
}

// summaryStatus is a watch's status as a summary line words it: a paused
// until-landed watch reads "paused (unacked)" rather than "expired".
func summaryStatus(w *watcher.Watch) string {
	if w.Paused() {
		return "paused (unacked)"
	}
	return string(w.Status)
}

// createdWatchSummary is lagotto_watch's summary line (#75): the watch, what
// it watches, and whether anything will poll it, which is the outcome that
// decides whether the watch can ever fire.
func createdWatchSummary(w *watcher.Watch, ttl time.Duration, pollerFound, pending bool) string {
	purchase := "On-Demand"
	if w.Spot {
		purchase = "Spot"
	}
	life := "expires in " + formatDuration(ttl)
	if w.UntilLanded {
		life = "until landed, pauses in " + formatDuration(w.AckWindow()) + " unless acked"
	}
	line := fmt.Sprintf("✅ Created watch %s: %s %s in %s, action %s, %s", w.WatchID, w.InstanceTypePattern, purchase, summaryRegions(w.Regions), w.Action, life)
	if pollerFound {
		line += "; poller on"
	} else {
		line += "; no hosted poller, so nothing polls it yet"
	}
	if pending {
		line += "; email confirmation pending"
	}
	return line
}

func dashIfEmpty(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func handleLagottoStatus(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	watchID, _ := args["watch_id"].(string)
	if strings.TrimSpace(watchID) == "" {
		return mcp.NewToolResultError("watch_id is required"), nil
	}

	store, _, caller, err := lagottoContext(ctx)
	if err != nil {
		return mcp.NewToolResultError("failed to connect to AWS: " + err.Error()), nil
	}

	w, err := store.GetWatch(ctx, watchID)
	if err != nil {
		return mcp.NewToolResultError("get watch: " + err.Error()), nil
	}
	// Owner-scope: a watch ID is guessable and the tables can be shared, so return
	// the SAME "not found" for a watch the caller doesn't own (no existence oracle),
	// mirroring the lagotto CLI's getWatchOwned.
	if w == nil || (w.UserID != "" && caller != "" && w.UserID != caller) {
		return mcp.NewToolResultError(fmt.Sprintf("watch %s not found", watchID)), nil
	}

	w.ComputeDurations()

	var sb strings.Builder
	fmt.Fprintf(&sb, "Watch:    %s\n", w.WatchID)
	fmt.Fprintf(&sb, "Status:   %s\n", w.Status)
	if w.Project != "" {
		fmt.Fprintf(&sb, "Project:  %s\n", w.Project)
	}
	if w.UserID != "" {
		fmt.Fprintf(&sb, "Owner:    %s\n", w.UserID)
	}
	fmt.Fprintf(&sb, "Pattern:  %s\n", w.InstanceTypePattern)
	fmt.Fprintf(&sb, "Regions:  %s\n", displayRegions(w.Regions))
	if len(w.AvailabilityZones) > 0 {
		fmt.Fprintf(&sb, "AZs:      %s\n", strings.Join(w.AvailabilityZones, ", "))
	}
	fmt.Fprintf(&sb, "Spot:     %v\n", w.Spot)
	if w.MaxPrice > 0 {
		fmt.Fprintf(&sb, "Max price: $%.4f/hr\n", w.MaxPrice)
	}
	fmt.Fprintf(&sb, "Action:   %s\n", w.Action)
	writeExposure(&sb, "Exposure: ", watcher.StaticExposure(w, mcpStaticPriceReason))
	if w.DesiredCount > 0 {
		fmt.Fprintf(&sb, "Fleet:    maintain %d worker(s)\n", w.DesiredCount)
		if w.CompletionCondition != "" {
			fmt.Fprintf(&sb, "Until:    %s\n", w.CompletionCondition)
		}
	}
	now := time.Now().UTC()
	fmt.Fprintf(&sb, "Created:  %s\n", w.CreatedAt.Format(time.RFC3339))
	writeExpires(&sb, w, now)
	writeHeartbeat(&sb, w, now)
	fmt.Fprintf(&sb, "Matches:  %d\n", w.MatchCount)
	if d, ok := w.WaitToAcquire(); ok {
		fmt.Fprintf(&sb, "Wait to acquire: %s\n", watcher.FormatWait(d))
	}
	if d, ok := w.TimeToGiveUp(); ok {
		fmt.Fprintf(&sb, "Time to give up: %s\n", watcher.FormatWait(d))
	}
	if m := w.LastMatch; m != nil {
		sb.WriteString("\nLast match:\n")
		fmt.Fprintf(&sb, "  Instance: %s\n", m.InstanceType)
		fmt.Fprintf(&sb, "  Region:   %s\n", m.Region)
		fmt.Fprintf(&sb, "  AZ:       %s\n", m.AvailabilityZone)
		fmt.Fprintf(&sb, "  Price:    $%.4f/hr\n", m.Price)
		fmt.Fprintf(&sb, "  Spot:     %v\n", m.IsSpot)
		fmt.Fprintf(&sb, "  Action:   %s\n", m.ActionTaken)
		if m.InstanceID != "" {
			fmt.Fprintf(&sb, "  Instance ID: %s\n", m.InstanceID)
		}
	}
	return withSummary(watchSummary(w, now), sb.String()), nil
}

// parseNotifyChannels parses "type:target" notification specs, mirroring the
// lagotto CLI (cmd/watch.go). Returns an error naming the bad entry.
func parseNotifyChannels(raw string) ([]watcher.NotifyChannel, error) {
	var channels []watcher.NotifyChannel
	for _, s := range strings.Split(raw, ",") {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		i := strings.IndexByte(s, ':')
		if i < 0 {
			return nil, fmt.Errorf("invalid notify format %q: expected type:target (e.g. email:user@example.com)", s)
		}
		ch := watcher.NotifyChannel{Type: s[:i], Target: s[i+1:]}
		switch ch.Type {
		case "email":
			// Stored normalized, as `lagotto watch` does: the address is the
			// subscription's filter value and the publish attribute
			// (lagotto#240), so one spelling throughout.
			if err := watcher.ValidateEmail(ch.Target); err != nil {
				return nil, err
			}
			ch.Target = watcher.NormalizeEmail(ch.Target)
		case "sns":
		case "webhook":
			if err := watcher.ValidateWebhookURL(ch.Target); err != nil {
				return nil, fmt.Errorf("invalid webhook URL: %w", err)
			}
		default:
			return nil, fmt.Errorf("invalid notify type %q: must be email, webhook, or sns", ch.Type)
		}
		channels = append(channels, ch)
	}
	return channels, nil
}

func parseRegionList(s string) []string {
	var regions []string
	for _, r := range strings.Split(s, ",") {
		if r = strings.TrimSpace(r); r != "" {
			regions = append(regions, r)
		}
	}
	return regions
}

func handleLagottoWatch(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	pattern, _ := args["pattern"].(string)
	regionsStr, _ := args["regions"].(string)
	actionStr, _ := args["action"].(string)
	project, _ := args["project"].(string)
	ttlStr, _ := args["ttl"].(string)
	spot, _ := args["spot"].(bool)
	maxPrice, _ := args["max_price"].(float64)
	notifyStr, _ := args["notify"].(string)
	untilLanded, _ := args["until_landed"].(bool)
	remindStr, _ := args["remind"].(string)
	confirm, _ := args["confirm"].(bool)

	if strings.TrimSpace(pattern) == "" {
		return mcp.NewToolResultError("pattern is required"), nil
	}
	if actionStr == "" {
		actionStr = "notify"
	}

	// EC2-only via MCP: SageMaker watches need a job definition file, which is out
	// of scope for this tool.
	service := watcher.ServiceEC2
	if err := watcher.ValidateWatchPattern(service, pattern); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	// Only notify + hold are supported here. spawn needs a full launch-config
	// (file/S3/stdin parsing + self-contained resolution) — defer that to the CLI.
	action := watcher.ActionMode(actionStr)
	switch action {
	case watcher.ActionNotify, watcher.ActionHold:
	case watcher.ActionSpawn:
		return mcp.NewToolResultError("action=spawn is not supported via MCP (it requires a spawn launch-config); create it with the lagotto CLI: lagotto watch <pattern> --action spawn --spawn-config <file>"), nil
	default:
		return mcp.NewToolResultError(fmt.Sprintf("invalid action %q: must be notify or hold", actionStr)), nil
	}

	// A positive ttl, or until_landed with its remind interval (#92).
	life, err := watchLifetime(strings.TrimSpace(ttlStr), untilLanded, strings.TrimSpace(remindStr))
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	channels, err := parseNotifyChannels(notifyStr)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	regions := parseRegionList(regionsStr)

	// Until-landed (lagotto#239): lagotto's own rules (not notify, a notify
	// channel required, the remind bounds), then the confirmation the CLI asks
	// for with a prompt or --yes, here an explicit confirm=true. Both before any
	// AWS call, so a refusal costs nothing and creates nothing.
	if life.UntilLanded {
		if err := watcher.ValidateUntilLanded(action, 0, channels, life.RemindEvery, life.RemindPauseAfter); err != nil {
			return mcp.NewToolResultError(lagottoArgWording.Replace(err.Error())), nil
		}
		if action == watcher.ActionHold && !confirm {
			exp := watcher.StaticExposure(&watcher.Watch{Service: service, Action: action, InstanceTypePattern: pattern,
				Regions: regions, Spot: spot, MaxPrice: maxPrice}, mcpStaticPriceReason)
			return mcp.NewToolResultError(untilLandedConfirmRefusal(pattern, life, exp).Error()), nil
		}
	}

	// The ID, timestamps, expiry and the record's retention horizon come from
	// watcher.NewWatch, as in `lagotto watch` (lagotto#266). It is the one place
	// expires_at and ttl_timestamp are set together: ttl_timestamp is when
	// DynamoDB deletes the record, expires_at when lagotto stops watching, and
	// setting them equal deleted the item at the moment the poller should write
	// its status=expired tombstone (#77, the MCP repeat of lagotto#161). It also
	// refuses a TTL that is not positive (#92), and sets an until-landed
	// watch's first acknowledgement deadline. Built before any AWS call, so a
	// refusal costs nothing.
	w, err := watcher.NewWatch(time.Now(), life)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	store, cfg, caller, err := lagottoContext(ctx)
	if err != nil {
		return mcp.NewToolResultError("failed to connect to AWS: " + err.Error()), nil
	}

	w.UserID = caller
	w.Project = project
	w.Service = service
	w.InstanceTypePattern = pattern
	w.Regions = regions
	w.Spot = spot
	w.MaxPrice = maxPrice
	w.Action = action
	w.NotifyChannels = channels

	// Default-subnet coverage (lagotto#256), as `lagotto watch` checks it: a
	// hold watch that can never land in a region it names (no AZ there both
	// offers the type and has a default subnet) is refused before anything is
	// created. A lookup that can't be made is a warning, never a refusal. A
	// notify watch is not checked.
	cov := newSubnetCoverageChecker(cfg).Check(ctx, w)
	if err := cov.Refusal(); err != nil {
		return mcp.NewToolResultError(strings.ReplaceAll(err.Error(), "--regions", "regions") + "\nNo watch was created."), nil
	}
	coverageWarnings := cov.Warnings()

	// Auto-create the backing tables on first use (zero-setup), like the CLI.
	if _, err := store.EnsureTables(ctx); err != nil {
		return mcp.NewToolResultError("ensure tables: " + err.Error()), nil
	}

	// Store the watch and make sure the hosted poller will poll it (#78), in
	// `lagotto watch`'s order, with the same lagotto code (pkg/arm,
	// lagotto#266): read the schedule first, refusing if it can't be read;
	// store; then switch a DISABLED schedule on, rolling the watch back if that
	// fails.
	//
	// The hosted poller switches its own schedule OFF when there are no active
	// watches, so a watch is only polled if whatever creates it switches the
	// schedule back on. Storing first and enabling second means a poller run
	// that starts after the enable always sees the watch; the other order lets
	// a run that starts in between find no watch and switch the schedule
	// straight back off. A schedule that is not found means no hosted poller is
	// deployed: the watch is stored (for `lagotto poll --daemon`) and the result
	// says so. Any other read error, AccessDenied included, refuses the watch.
	sched := scheduler.NewFromConfig(cfg)
	armAction := "create watch " + w.WatchID
	pollSched, err := arm.PreflightPollSchedule(ctx, sched, armAction)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	// Subscribe each email address to the alerts topic (#81, lagotto#240)
	// before the watch is stored, so an address that cannot be subscribed
	// refuses the watch instead of arming one whose emails reach no one.
	emailWarnings, err := subscribeForNewWatch(ctx, cfg, caller, w)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if warn := undeliverableUntilLandedWarning(w); warn != "" {
		emailWarnings = append(emailWarnings, warn)
	}
	if err := store.PutWatch(ctx, w); err != nil {
		return mcp.NewToolResultError("create watch: " + err.Error()), nil
	}
	enabled, err := arm.EnablePollScheduleIfOff(ctx, sched, pollSched)
	if err != nil {
		// Roll back through PutWatch, the call (and dynamodb:PutItem permission)
		// that just created it, so the rollback needs nothing the caller lacks.
		w.Status = watcher.StatusCancelled
		w.UpdatedAt = time.Now().UTC()
		return mcp.NewToolResultError(arm.PollScheduleUpdateRefusal(armAction, "no watch was created", store.PutWatch(ctx, w), err).Error()), nil
	}

	auditLog("lagotto_watch", w.WatchID, "", "")

	var sb strings.Builder
	fmt.Fprintf(&sb, "✅ Created watch %s\n", w.WatchID)
	fmt.Fprintf(&sb, "  Pattern: %s\n", w.InstanceTypePattern)
	fmt.Fprintf(&sb, "  Regions: %s\n", displayRegions(w.Regions))
	if w.Project != "" {
		fmt.Fprintf(&sb, "  Project: %s\n", w.Project)
	}
	fmt.Fprintf(&sb, "  Spot:    %v\n", w.Spot)
	fmt.Fprintf(&sb, "  Action:  %s\n", w.Action)
	if w.Action == watcher.ActionHold {
		sb.WriteString("  Note:    hold reserves billable On-Demand capacity on match.\n")
	}
	w.Exposure = watcher.StaticExposure(w, mcpStaticPriceReason)
	writeExposure(&sb, "  Exposure: ", w.Exposure)
	if w.UntilLanded {
		fmt.Fprintf(&sb, "  Expires: until landed; pauses %s unless acknowledged (lagotto_ack watch_id %s; each ack moves it %s ahead)\n",
			w.ExpiresAt.Format(time.RFC3339), w.WatchID, watcher.FormatSpan(w.AckWindow()))
	} else {
		fmt.Fprintf(&sb, "  Expires: %s\n", w.ExpiresAt.Format(time.RFC3339))
	}
	pending := false
	for _, ch := range w.NotifyChannels {
		if ch.Type == "email" && ch.Subscription != "" {
			fmt.Fprintf(&sb, "  Notify:  email:%s  %s\n", ch.Target, ch.Subscription.Describe())
			pending = pending || ch.Subscription == watcher.SubPending
		}
	}
	if pending {
		sb.WriteString("  Note:    AWS has emailed a confirmation link to each pending address; nothing is delivered to it until the link is clicked.\n")
	}
	for _, warn := range append(coverageWarnings, emailWarnings...) {
		fmt.Fprintf(&sb, "  Warning: %s\n", warn)
	}
	switch {
	case pollSched == nil:
		fmt.Fprintf(&sb, "  Poller:  no hosted poller in this account and region (schedule %s not found), so nothing will poll this watch until you run `lagotto deploy` here, or `lagotto poll --daemon` somewhere that stays up.\n", arm.PollScheduleName())
	case enabled:
		fmt.Fprintf(&sb, "  Poller:  switched the hosted poller's schedule %s back on (it turns itself off when no watches are active).\n", awssdk.ToString(pollSched.Name))
	default:
		fmt.Fprintf(&sb, "  Poller:  the hosted poller's schedule %s is on.\n", awssdk.ToString(pollSched.Name))
	}
	return withSummary(createdWatchSummary(w, life.TTL, pollSched != nil, pending), sb.String()), nil
}
