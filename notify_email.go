package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/arn"
	"github.com/aws/aws-sdk-go-v2/service/sns"
	"github.com/spore-host/lagotto/pkg/deploy"
	"github.com/spore-host/lagotto/pkg/watcher"
)

// Email channels and their subscriptions (#81, lagotto#240).
//
// Since lagotto#240 the poller publishes each email event to the alerts topic
// with the message attribute lagotto_email=<address>, and only a subscription
// whose filter policy names that address receives it. `lagotto watch` creates
// that subscription; lagotto_watch did not, so an email channel created here
// stored the address and delivered nothing.
//
// The subscription itself is lagotto's exported code
// (watcher.NewEmailSubscriptions(...).Ensure: reuse an existing subscription,
// else sns:Subscribe with FilterPolicy={"lagotto_email":[addr]} and
// FilterPolicyScope=MessageAttributes, every call made in the topic ARN's
// region). Resolving the topic and the refuse-before-store sequencing are
// small equivalents of lagotto v0.67.0's cmd/poll_notify.go resolveAlertsTopic
// and cmd/notify_email.go subscribeWatchEmails, which are unexported.
//
// Cleanup needs nothing here: the hosted poller unsubscribes an address at the
// start of the sweep after the last active watch naming it ends, and
// `lagotto cancel` / `lagotto notify-gc` do it from the CLI. If this server
// gains a cancel tool, it should call Reconcile(ctx, activeWatches,
// addrsOfCancelledWatch) after the status write, excluding the cancelled watch
// (the status index is eventually consistent), as lagotto's cancel does.

// lagottoTopicEnvVar overrides the alerts topic, as in the lagotto CLI.
const lagottoTopicEnvVar = "LAGOTTO_SNS_TOPIC_ARN"

// alertsTopic is the resolved alerts topic. ARN "" means none resolved, and Why
// says why.
type alertsTopic struct {
	ARN string
	Why string
}

// topicProber checks whether the capacity-alerts topic exists in an account
// and region. *deploy.Deployer satisfies it (sns:GetTopicAttributes).
type topicProber interface {
	AlertsTopicPresent(ctx context.Context, region, accountID string) (bool, error)
}

// resolveAlertsTopic finds the alerts topic the way `lagotto watch` does:
// LAGOTTO_SNS_TOPIC_ARN when set; else lagotto-capacity-alerts in the caller's
// account and region when it exists (or when its existence can't be checked,
// since a caller may subscribe to a topic it cannot describe); else nothing.
func resolveAlertsTopic(ctx context.Context, getenv func(string) string, region, accountID string, probe topicProber) alertsTopic {
	if v := strings.TrimSpace(getenv(lagottoTopicEnvVar)); v != "" {
		return alertsTopic{ARN: v}
	}
	if region == "" {
		return alertsTopic{Why: "no AWS region is set, so lagotto cannot look for " + deploy.AlertsTopicName}
	}
	if accountID == "" {
		return alertsTopic{Why: "your AWS account could not be resolved, so lagotto cannot look for " + deploy.AlertsTopicName}
	}
	topicArn := deploy.AlertsTopicARN(region, accountID)
	present, err := probe.AlertsTopicPresent(ctx, region, accountID)
	switch {
	case err != nil:
		return alertsTopic{ARN: topicArn}
	case !present:
		return alertsTopic{Why: fmt.Sprintf("%s does not exist in account %s, region %s (`lagotto deploy` creates it)", deploy.AlertsTopicName, accountID, region)}
	default:
		return alertsTopic{ARN: topicArn}
	}
}

// emailSubscriber is the slice of watcher.EmailSubscriptions lagotto_watch uses.
type emailSubscriber interface {
	TopicArn() string
	Ensure(ctx context.Context, addr string) (watcher.EnsureResult, error)
}

// subscribeWatchEmails subscribes each email address on w to the alerts topic,
// reusing an address's existing subscription, and records each channel's state
// on w for the output (NotifyChannel.Subscription is never stored). It runs
// before the watch is stored, so a refusal leaves no watch behind: an email
// channel that cannot be subscribed would deliver nothing. It returns a warning
// per address that already has an UNFILTERED subscription.
func subscribeWatchEmails(ctx context.Context, topic alertsTopic, subs emailSubscriber, w *watcher.Watch) ([]string, error) {
	addrs := watcher.EmailAddresses(w)
	if len(addrs) == 0 {
		return nil, nil
	}
	if topic.ARN == "" || subs == nil {
		return nil, fmt.Errorf("email notifications need the lagotto alerts topic, and %s. Run `lagotto deploy` in this account and region "+
			"(or set %s on the MCP server), or drop the email channel; no watch was created", topic.Why, lagottoTopicEnvVar)
	}
	states := map[string]watcher.SubscriptionState{}
	var warnings []string
	for _, a := range addrs {
		res, err := subs.Ensure(ctx, a)
		if err != nil {
			return nil, fmt.Errorf("subscribe %s to email notifications: %w; no watch was created "+
				"(this needs sns:Subscribe, sns:ListSubscriptionsByTopic and sns:GetSubscriptionAttributes on %s)", a, err, subs.TopicArn())
		}
		states[a] = res.State
		if res.Unfiltered {
			warnings = append(warnings, fmt.Sprintf("%s already has a subscription on %s with NO filter policy: it receives every email "+
				"event on the topic, other users' watches included. lagotto did not create it and leaves it alone; `lagotto doctor` names it.",
				a, subs.TopicArn()))
		}
	}
	for i := range w.NotifyChannels {
		ch := &w.NotifyChannels[i]
		if ch.Type != "email" {
			continue
		}
		if s, ok := states[watcher.NormalizeEmail(ch.Target)]; ok {
			ch.Subscription = s
		} else {
			ch.Subscription = watcher.SubNone
		}
	}
	return warnings, nil
}

// subscribeForNewWatch is lagotto_watch's subscribe step: resolve the alerts
// topic, then subscribeWatchEmails. A no-op for a watch with no email channel.
func subscribeForNewWatch(ctx context.Context, cfg awssdk.Config, callerArn string, w *watcher.Watch) ([]string, error) {
	if !watcher.NeedsAlertsTopic(w) {
		return nil, nil
	}
	account := ""
	if parsed, err := arn.Parse(callerArn); err == nil {
		account = parsed.AccountID
	}
	topic := resolveAlertsTopic(ctx, os.Getenv, cfg.Region, account, deploy.New(cfg))
	var subs emailSubscriber
	if topic.ARN != "" {
		subs = watcher.NewEmailSubscriptions(sns.NewFromConfig(cfg), topic.ARN)
	}
	return subscribeWatchEmails(ctx, topic, subs, w)
}
