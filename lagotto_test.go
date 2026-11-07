package main

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/spore-host/lagotto/pkg/watcher"
)

// TestLagottoStatus_RequiresID rejects an empty watch_id before any AWS call.
func TestLagottoStatus_RequiresID(t *testing.T) {
	res, err := handleLagottoStatus(context.Background(), newRequest(map[string]any{"watch_id": ""}))
	if err != nil {
		t.Fatalf("transport error: %v", err)
	}
	if res == nil || !res.IsError {
		t.Errorf("expected an error result for an empty watch_id, got %+v", res)
	}
}

// TestLagottoWatch_ValidationBeforeAWS covers the argument-validation layer of
// the create tool: all of these are rejected before any AWS/DynamoDB call.
func TestLagottoWatch_ValidationBeforeAWS(t *testing.T) {
	cases := []struct {
		name string
		args map[string]any
		want string
	}{
		{"empty pattern", map[string]any{"pattern": ""}, "pattern is required"},
		{"spawn action deferred", map[string]any{"pattern": "g5.xlarge", "action": "spawn"}, "action=spawn is not supported"},
		{"invalid action", map[string]any{"pattern": "g5.xlarge", "action": "bogus"}, "invalid action"},
		{"invalid ttl", map[string]any{"pattern": "g5.xlarge", "ttl": "banana"}, "invalid ttl"},
		{"bad notify", map[string]any{"pattern": "g5.xlarge", "notify": "no-colon-here"}, "invalid notify"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := handleLagottoWatch(context.Background(), newRequest(tc.args))
			if err != nil {
				t.Fatalf("transport error: %v", err)
			}
			if res == nil || !res.IsError {
				t.Fatalf("expected an error result, got %+v", res)
			}
			if !strings.Contains(resultText(res), tc.want) {
				t.Errorf("error %q missing %q", resultText(res), tc.want)
			}
		})
	}
}

func TestParseNotifyChannels(t *testing.T) {
	chs, err := parseNotifyChannels("email:a@b.com, sns:arn:aws:sns:us-east-1:1:topic")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(chs) != 2 {
		t.Fatalf("expected 2 channels, got %d", len(chs))
	}
	if chs[0].Type != "email" || chs[0].Target != "a@b.com" {
		t.Errorf("channel 0 wrong: %+v", chs[0])
	}
	// SNS target keeps its colons intact (split on the FIRST colon only).
	if chs[1].Type != "sns" || !strings.HasPrefix(chs[1].Target, "arn:aws:sns") {
		t.Errorf("channel 1 wrong: %+v", chs[1])
	}
	// An empty spec yields no channels and no error.
	if chs, err := parseNotifyChannels(""); err != nil || len(chs) != 0 {
		t.Errorf("empty notify: got %v, err %v", chs, err)
	}
	// A bad type is rejected.
	if _, err := parseNotifyChannels("carrier-pigeon:home"); err == nil {
		t.Error("expected an error for an unknown notify type")
	}
}

func TestShortOwner(t *testing.T) {
	cases := map[string]string{
		"arn:aws:iam::123456789012:user/alice":                     "user/alice",
		"arn:aws:sts::123456789012:assumed-role/Dev/alice-session": "assumed-role/Dev/alice-session",
		"":           "-",
		"plain-name": "plain-name",
	}
	for in, want := range cases {
		if got := shortOwner(in); got != want {
			t.Errorf("shortOwner(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseRegionList(t *testing.T) {
	got := parseRegionList(" us-east-1 , ,us-west-2 ")
	if len(got) != 2 || got[0] != "us-east-1" || got[1] != "us-west-2" {
		t.Errorf("parseRegionList = %v", got)
	}
	if r := parseRegionList(""); len(r) != 0 {
		t.Errorf("empty region list should be nil/empty, got %v", r)
	}
}

// Guard: the actions the create tool accepts map to real watcher ActionModes.
func TestWatchActionsExist(t *testing.T) {
	if watcher.ActionNotify != "notify" || watcher.ActionHold != "hold" {
		t.Errorf("unexpected action constants: notify=%q hold=%q", watcher.ActionNotify, watcher.ActionHold)
	}
}

// putWatchItem is the one watch record a lagotto_watch call wrote to the fake.
func putWatchItem(t *testing.T, f *fakeAWS) map[string]map[string]any {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.ddbPuts) != 1 {
		t.Fatalf("PutItem calls = %d, want 1 (actions: %v)", len(f.ddbPuts), f.actions)
	}
	return f.ddbPuts[0]
}

// TestLagottoWatch_TTLTimestampIsTheRetentionHorizon is #77's regression test,
// the MCP repeat of lagotto#161: ttl_timestamp (when DynamoDB deletes the
// record) must be watcher.RetentionTTL(expires_at), not expires_at (when
// lagotto stops watching), or the item is deleted out from under the poller's
// status=expired tombstone.
func TestLagottoWatch_TTLTimestampIsTheRetentionHorizon(t *testing.T) {
	f := newFakeAWS(t)
	f.install(t)

	res, err := handleLagottoWatch(context.Background(), newRequest(map[string]any{
		"pattern": "g5.xlarge", "action": "notify", "ttl": "24h",
	}))
	if err != nil {
		t.Fatalf("transport error: %v", err)
	}
	if res.IsError {
		t.Fatalf("lagotto_watch failed: %s", resultText(res))
	}

	item := putWatchItem(t, f)
	expiresAt, err := time.Parse(time.RFC3339Nano, item["expires_at"]["S"].(string))
	if err != nil {
		t.Fatalf("expires_at %v: %v", item["expires_at"], err)
	}
	ttl, err := strconv.ParseInt(item["ttl_timestamp"]["N"].(string), 10, 64)
	if err != nil {
		t.Fatalf("ttl_timestamp %v: %v", item["ttl_timestamp"], err)
	}
	if want := watcher.RetentionTTL(expiresAt); ttl != want {
		t.Errorf("ttl_timestamp = %d (%s), want RetentionTTL(expires_at) = %d (%s)",
			ttl, time.Unix(ttl, 0).UTC(), want, time.Unix(want, 0).UTC())
	}
	if ttl <= expiresAt.Unix() {
		t.Errorf("ttl_timestamp %s is not after expires_at %s: DynamoDB would delete the record at expiry",
			time.Unix(ttl, 0).UTC(), expiresAt)
	}
}

// pollerSchedule is a deployed hosted-poller schedule in state.
func pollerSchedule(state string) *fakeSchedule {
	return &fakeSchedule{
		Name:       "lagotto-capacity-poller",
		State:      state,
		Expression: "rate(5 minutes)",
		TargetArn:  "arn:aws:lambda:us-west-2:123456789012:function:lagotto-capacity-poller",
		RoleArn:    "arn:aws:iam::123456789012:role/lagotto-capacity-poller-scheduler-invoke",
	}
}

func createNotifyWatch(t *testing.T) (string, bool) {
	t.Helper()
	res, err := handleLagottoWatch(context.Background(), newRequest(map[string]any{
		"pattern": "g5.xlarge", "action": "notify", "ttl": "24h",
	}))
	if err != nil {
		t.Fatalf("transport error: %v", err)
	}
	return resultText(res), res.IsError
}

// indexOf is the position of the first action with prefix, or -1.
func indexOf(actions []string, prefix string) int {
	for i, a := range actions {
		if strings.HasPrefix(a, prefix) {
			return i
		}
	}
	return -1
}

// TestLagottoWatch_EnablesADisabledPollSchedule is #78: the hosted poller turns
// its schedule off when idle, so lagotto_watch must turn it back on, after
// storing the watch, round-tripping the schedule's fields (UpdateSchedule is a
// full PUT), as `lagotto watch` does.
func TestLagottoWatch_EnablesADisabledPollSchedule(t *testing.T) {
	f := newFakeAWS(t)
	f.schedule = pollerSchedule("DISABLED")
	f.install(t)

	text, isErr := createNotifyWatch(t)
	if isErr {
		t.Fatalf("lagotto_watch failed: %s", text)
	}
	if len(f.scheduleUpdates) != 1 {
		t.Fatalf("UpdateSchedule calls = %d, want 1 (actions %v)", len(f.scheduleUpdates), f.actions)
	}
	up := f.scheduleUpdates[0]
	if up["State"] != "ENABLED" {
		t.Errorf("UpdateSchedule State = %v, want ENABLED", up["State"])
	}
	if up["ScheduleExpression"] != "rate(5 minutes)" {
		t.Errorf("UpdateSchedule dropped the expression: %v", up["ScheduleExpression"])
	}
	target, _ := up["Target"].(map[string]any)
	if target["Arn"] != f.schedule.TargetArn || target["RoleArn"] != f.schedule.RoleArn {
		t.Errorf("UpdateSchedule Target = %v, want the schedule's own target and role", target)
	}
	get, put, update := indexOf(f.actions, "scheduler:GetSchedule"), indexOf(f.actions, "dynamodb:PutItem"), indexOf(f.actions, "scheduler:UpdateSchedule")
	if get < 0 || get >= put || put >= update {
		t.Errorf("order = %v, want GetSchedule < PutItem < UpdateSchedule", f.actions)
	}
	if !strings.Contains(text, "switched the hosted poller's schedule lagotto-capacity-poller back on") {
		t.Errorf("result does not say the schedule was enabled:\n%s", text)
	}
}

// TestLagottoWatch_LeavesAnEnabledPollScheduleAlone: no UpdateSchedule when the
// schedule is already on.
func TestLagottoWatch_LeavesAnEnabledPollScheduleAlone(t *testing.T) {
	f := newFakeAWS(t)
	f.schedule = pollerSchedule("ENABLED")
	f.install(t)

	text, isErr := createNotifyWatch(t)
	if isErr {
		t.Fatalf("lagotto_watch failed: %s", text)
	}
	if len(f.scheduleUpdates) != 0 {
		t.Errorf("UpdateSchedule called on an ENABLED schedule: %v", f.scheduleUpdates)
	}
	if !strings.Contains(text, "schedule lagotto-capacity-poller is on") {
		t.Errorf("result does not report the schedule:\n%s", text)
	}
}

// TestLagottoWatch_NoHostedPollerSaysSo: with no schedule deployed the watch is
// still stored (it is for `lagotto poll --daemon`), and the result says nothing
// will poll it until one runs.
func TestLagottoWatch_NoHostedPollerSaysSo(t *testing.T) {
	f := newFakeAWS(t)
	f.install(t)

	text, isErr := createNotifyWatch(t)
	if isErr {
		t.Fatalf("lagotto_watch failed: %s", text)
	}
	putWatchItem(t, f)
	for _, want := range []string{"no hosted poller", "lagotto deploy", "lagotto poll --daemon"} {
		if !strings.Contains(text, want) {
			t.Errorf("result missing %q:\n%s", want, text)
		}
	}
}

// TestLagottoWatch_UnreadablePollScheduleRefuses: an error other than
// ResourceNotFound (AccessDenied here) means we cannot know the watch will be
// polled, so nothing is stored and the refusal names the missing permissions.
func TestLagottoWatch_UnreadablePollScheduleRefuses(t *testing.T) {
	f := newFakeAWS(t)
	f.schedGetErr = "AccessDeniedException"
	f.install(t)

	text, isErr := createNotifyWatch(t)
	if !isErr {
		t.Fatalf("lagotto_watch succeeded with an unreadable schedule:\n%s", text)
	}
	if len(f.ddbPuts) != 0 {
		t.Errorf("stored %d item(s) after the preflight refused", len(f.ddbPuts))
	}
	for _, want := range []string{"scheduler:GetSchedule", "was denied", "nothing was changed", "iam:PassRole", "lagotto-capacity-poller-scheduler-invoke"} {
		if !strings.Contains(text, want) {
			t.Errorf("refusal missing %q:\n%s", want, text)
		}
	}
}

// TestLagottoWatch_EnableFailureRollsBack: if switching the schedule on fails,
// the stored watch is rewritten as cancelled, so no active watch is left for
// nothing to poll, and the call is refused.
func TestLagottoWatch_EnableFailureRollsBack(t *testing.T) {
	f := newFakeAWS(t)
	f.schedule = pollerSchedule("DISABLED")
	f.schedUpdateErr = "AccessDeniedException"
	f.install(t)

	text, isErr := createNotifyWatch(t)
	if !isErr {
		t.Fatalf("lagotto_watch succeeded although the schedule could not be enabled:\n%s", text)
	}
	if len(f.ddbPuts) != 2 {
		t.Fatalf("PutItem calls = %d, want 2 (create, then roll back)", len(f.ddbPuts))
	}
	if got := f.ddbPuts[1]["status"]["S"]; got != string(watcher.StatusCancelled) {
		t.Errorf("rollback wrote status %v, want %s", got, watcher.StatusCancelled)
	}
	if f.ddbPuts[0]["watch_id"]["S"] != f.ddbPuts[1]["watch_id"]["S"] {
		t.Errorf("rollback wrote a different watch: %v vs %v", f.ddbPuts[0]["watch_id"], f.ddbPuts[1]["watch_id"])
	}
	for _, want := range []string{"scheduler:UpdateSchedule", "no watch was created"} {
		if !strings.Contains(text, want) {
			t.Errorf("refusal missing %q:\n%s", want, text)
		}
	}
}

const fakeAlertsTopic = "arn:aws:sns:us-west-2:123456789012:lagotto-capacity-alerts"

func createEmailWatch(t *testing.T, notify string) (string, bool) {
	t.Helper()
	res, err := handleLagottoWatch(context.Background(), newRequest(map[string]any{
		"pattern": "g5.xlarge", "action": "notify", "ttl": "24h", "notify": notify,
	}))
	if err != nil {
		t.Fatalf("transport error: %v", err)
	}
	return resultText(res), res.IsError
}

// storedNotifyTargets is the notify_channels the stored watch carries.
func storedNotifyTargets(t *testing.T, item map[string]map[string]any) []string {
	t.Helper()
	var out []string
	list, _ := item["notify_channels"]["L"].([]any)
	for _, e := range list {
		m, _ := e.(map[string]any)["M"].(map[string]any)
		typ, _ := m["type"].(map[string]any)["S"].(string)
		target, _ := m["target"].(map[string]any)["S"].(string)
		if _, stored := m["subscription"]; stored {
			t.Errorf("channel %s:%s stored its subscription state; it is display-only", typ, target)
		}
		out = append(out, typ+":"+target)
	}
	return out
}

// TestLagottoWatch_EmailSubscribesTheAddress is #81: an email channel only
// delivers once the address has its own filtered subscription on the alerts
// topic (lagotto#240). lagotto_watch creates it before storing the watch, with
// the address normalized and lagotto's filter policy, and reports it pending.
func TestLagottoWatch_EmailSubscribesTheAddress(t *testing.T) {
	f := newFakeAWS(t)
	f.snsTopic = fakeAlertsTopic
	f.install(t)

	text, isErr := createEmailWatch(t, "email: Alice@Example.COM ")
	if isErr {
		t.Fatalf("lagotto_watch failed: %s", text)
	}
	if len(f.snsSubscribes) != 1 {
		t.Fatalf("Subscribe calls = %d, want 1 (actions %v)", len(f.snsSubscribes), f.actions)
	}
	sub := f.snsSubscribes[0]
	if sub.Get("TopicArn") != fakeAlertsTopic || sub.Get("Protocol") != "email" || sub.Get("Endpoint") != "alice@example.com" {
		t.Errorf("Subscribe = %v, want email alice@example.com on %s", sub, fakeAlertsTopic)
	}
	attrs := snsSubscribeAttrs(sub)
	if want := watcher.EmailFilterPolicy("alice@example.com"); attrs["FilterPolicy"] != want {
		t.Errorf("FilterPolicy = %q, want %q", attrs["FilterPolicy"], want)
	}
	if attrs["FilterPolicyScope"] != "MessageAttributes" {
		t.Errorf("FilterPolicyScope = %q, want MessageAttributes", attrs["FilterPolicyScope"])
	}
	if s, p := indexOf(f.actions, "sns:Subscribe"), indexOf(f.actions, "dynamodb:PutItem"); s < 0 || s > p {
		t.Errorf("order = %v, want Subscribe before PutItem", f.actions)
	}
	if got := storedNotifyTargets(t, putWatchItem(t, f)); len(got) != 1 || got[0] != "email:alice@example.com" {
		t.Errorf("stored channels = %v, want [email:alice@example.com]", got)
	}
	for _, want := range []string{"email:alice@example.com", "pending confirmation", "confirmation link"} {
		if !strings.Contains(text, want) {
			t.Errorf("result missing %q:\n%s", want, text)
		}
	}
}

// TestLagottoWatch_EmailReusesAConfirmedSubscription: an address that already
// has its subscription is not subscribed again, and shows as confirmed.
func TestLagottoWatch_EmailReusesAConfirmedSubscription(t *testing.T) {
	f := newFakeAWS(t)
	f.snsTopic = fakeAlertsTopic
	f.snsSubs = []fakeSNSSub{{endpoint: "bob@example.com", confirmed: true, filterPolicy: watcher.EmailFilterPolicy("bob@example.com")}}
	f.install(t)

	text, isErr := createEmailWatch(t, "email:bob@example.com")
	if isErr {
		t.Fatalf("lagotto_watch failed: %s", text)
	}
	if len(f.snsSubscribes) != 0 {
		t.Errorf("subscribed an address that already has a subscription: %v", f.snsSubscribes)
	}
	if !strings.Contains(text, "email:bob@example.com  confirmed") {
		t.Errorf("result does not show the confirmed subscription:\n%s", text)
	}
	if strings.Contains(text, "Warning:") {
		t.Errorf("warned about a filtered subscription:\n%s", text)
	}
}

// TestLagottoWatch_EmailWarnsAboutAnUnfilteredSubscription: an existing
// hand-made subscription with no filter policy is reused and left alone, but
// the caller is told it receives every user's email events.
func TestLagottoWatch_EmailWarnsAboutAnUnfilteredSubscription(t *testing.T) {
	f := newFakeAWS(t)
	f.snsTopic = fakeAlertsTopic
	f.snsSubs = []fakeSNSSub{{endpoint: "carol@example.com", confirmed: true}}
	f.install(t)

	text, isErr := createEmailWatch(t, "email:carol@example.com")
	if isErr {
		t.Fatalf("lagotto_watch failed: %s", text)
	}
	if len(f.snsSubscribes) != 0 {
		t.Errorf("subscribed again: %v", f.snsSubscribes)
	}
	if !strings.Contains(text, "NO filter policy") {
		t.Errorf("no unfiltered-subscription warning:\n%s", text)
	}
}

// TestLagottoWatch_EmailWithNoAlertsTopicRefuses: with no topic there is
// nothing to subscribe to and the email would reach no one, so the watch is
// refused and nothing is stored.
func TestLagottoWatch_EmailWithNoAlertsTopicRefuses(t *testing.T) {
	f := newFakeAWS(t)
	f.install(t)

	text, isErr := createEmailWatch(t, "email:dave@example.com")
	if !isErr {
		t.Fatalf("lagotto_watch succeeded with no alerts topic:\n%s", text)
	}
	if len(f.ddbPuts) != 0 || len(f.snsSubscribes) != 0 {
		t.Errorf("stored %d item(s), %d Subscribe(s) after refusing", len(f.ddbPuts), len(f.snsSubscribes))
	}
	for _, want := range []string{"lagotto deploy", "does not exist", "no watch was created"} {
		if !strings.Contains(text, want) {
			t.Errorf("refusal missing %q:\n%s", want, text)
		}
	}
}

// TestLagottoWatch_EmailSubscribeFailureRefuses: a Subscribe that fails
// (AuthorizationError here) refuses the watch, naming the permissions.
func TestLagottoWatch_EmailSubscribeFailureRefuses(t *testing.T) {
	f := newFakeAWS(t)
	f.snsTopic = fakeAlertsTopic
	f.snsSubscribeErr = "AuthorizationError"
	f.install(t)

	text, isErr := createEmailWatch(t, "email:erin@example.com")
	if !isErr {
		t.Fatalf("lagotto_watch succeeded although Subscribe failed:\n%s", text)
	}
	if len(f.ddbPuts) != 0 {
		t.Errorf("stored %d item(s) after Subscribe failed", len(f.ddbPuts))
	}
	for _, want := range []string{"subscribe erin@example.com", "sns:Subscribe", "no watch was created"} {
		if !strings.Contains(text, want) {
			t.Errorf("refusal missing %q:\n%s", want, text)
		}
	}
}

// TestLagottoWatch_EmailUsesTheTopicARNsRegion: LAGOTTO_SNS_TOPIC_ARN may name
// a topic in another region than the caller's, and SNS rejects a topic ARN
// from another region, so every subscription call goes to the ARN's region.
func TestLagottoWatch_EmailUsesTheTopicARNsRegion(t *testing.T) {
	const east = "arn:aws:sns:us-east-1:123456789012:team-alerts"
	t.Setenv("LAGOTTO_SNS_TOPIC_ARN", east)
	f := newFakeAWS(t)
	f.snsTopic = east
	f.install(t)

	text, isErr := createEmailWatch(t, "email:frank@example.com")
	if isErr {
		t.Fatalf("lagotto_watch failed: %s", text)
	}
	if len(f.snsCalls) == 0 {
		t.Fatal("no SNS calls")
	}
	for _, c := range f.snsCalls {
		if !strings.HasSuffix(c, "@us-east-1") {
			t.Errorf("SNS call %s, want every call in the topic's region us-east-1 (client region us-west-2)", c)
		}
	}
	if len(f.snsSubscribes) != 1 || f.snsSubscribes[0].Get("TopicArn") != east {
		t.Errorf("Subscribe = %v, want one on %s", f.snsSubscribes, east)
	}
}

// TestLagottoWatch_NoEmailNoSNS: a watch with no email channel makes no SNS
// call at all (webhook and sns channels name their own target).
func TestLagottoWatch_NoEmailNoSNS(t *testing.T) {
	f := newFakeAWS(t)
	f.install(t)

	text, isErr := createEmailWatch(t, "webhook:https://example.com/hook,sns:arn:aws:sns:us-west-2:123456789012:mine")
	if isErr {
		t.Fatalf("lagotto_watch failed: %s", text)
	}
	if len(f.snsCalls) != 0 {
		t.Errorf("SNS calls for a watch with no email channel: %v", f.snsCalls)
	}
}

// TestParseNotifyChannels_EmailValidatedAndNormalized mirrors `lagotto watch`:
// an unsubscribable address is rejected before AWS, and a good one is stored
// trimmed and lower-cased.
func TestParseNotifyChannels_EmailValidatedAndNormalized(t *testing.T) {
	chs, err := parseNotifyChannels("email:Grace@Example.com")
	if err != nil || len(chs) != 1 || chs[0].Target != "grace@example.com" {
		t.Errorf("parse = %+v, %v; want one channel grace@example.com", chs, err)
	}
	for _, bad := range []string{"email:no-at-sign", "email:a b@example.com", "email:@example.com"} {
		if _, err := parseNotifyChannels(bad); err == nil {
			t.Errorf("parseNotifyChannels(%q) accepted an invalid address", bad)
		}
	}
}
