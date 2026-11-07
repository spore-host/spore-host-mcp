package main

import (
	"context"
	"fmt"
	"os"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/arn"
	"github.com/aws/aws-sdk-go-v2/service/sns"
	"github.com/spore-host/lagotto/pkg/arm"
	"github.com/spore-host/lagotto/pkg/deploy"
	"github.com/spore-host/lagotto/pkg/watcher"
)

// Email channels and their subscriptions (#81, lagotto#240).
//
// Since lagotto#240 the poller publishes each email event to the alerts topic
// with the message attribute lagotto_email=<address>, and only a subscription
// whose filter policy names that address receives it. So creating a watch with
// an email channel must subscribe the address before the watch is stored.
//
// The steps are lagotto's own, exported in pkg/arm (lagotto#266) and called by
// `lagotto watch` too: arm.ResolveAlertsTopic finds the topic, and
// arm.SubscribeWatchEmails subscribes each address (reusing an existing
// subscription) and records each channel's state for the output, refusing
// before anything is stored if an address cannot be subscribed.
//
// Cleanup needs nothing here: the hosted poller unsubscribes an address at the
// start of the sweep after the last active watch naming it ends, and
// `lagotto cancel` / `lagotto notify-gc` do it from the CLI. If this server
// gains a cancel tool, it should call arm.CleanupWatchEmails after the status
// write, as lagotto's cancel does.

// snsSubscribePermissions is the IAM the subscribe step needs. It is added to
// arm.SubscribeWatchEmails's refusal when a subscription could not be made,
// since an MCP user cannot see lagotto's CLI IAM reference from a tool result.
const snsSubscribePermissions = "this needs sns:Subscribe, sns:ListSubscriptionsByTopic and sns:GetSubscriptionAttributes on %s"

// subscribeForNewWatch is lagotto_watch's subscribe step: resolve the alerts
// topic, then arm.SubscribeWatchEmails. A no-op for a watch with no email
// channel. It returns a warning per address that already has an UNFILTERED
// subscription.
func subscribeForNewWatch(ctx context.Context, cfg awssdk.Config, callerArn string, w *watcher.Watch) ([]string, error) {
	if !watcher.NeedsAlertsTopic(w) {
		return nil, nil
	}
	topic := arm.ResolveAlertsTopic(ctx, os.Getenv, cfg.Region, accountOf(callerArn), deploy.New(cfg))
	var subs arm.EmailSubscriber
	if topic.ARN != "" {
		subs = watcher.NewEmailSubscriptions(sns.NewFromConfig(cfg), topic.ARN)
	}
	warnings, err := arm.SubscribeWatchEmails(ctx, topic, subs, w)
	if err != nil && subs != nil {
		err = fmt.Errorf("%w ("+snsSubscribePermissions+")", err, subs.TopicArn())
	}
	return warnings, err
}

// accountOf is the account ID in an ARN, or "" when it does not parse.
func accountOf(a string) string {
	if parsed, err := arn.Parse(a); err == nil {
		return parsed.AccountID
	}
	return ""
}
