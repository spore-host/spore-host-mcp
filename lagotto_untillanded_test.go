package main

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/spore-host/lagotto/pkg/watcher"
)

// fakeCoverageEC2 answers the two EC2 calls the default-subnet coverage check
// makes (lagotto#256): offered lists the AZs that offer every type,
// defaultSubnets the AZs with a default subnet.
type fakeCoverageEC2 struct {
	offered, defaultSubnets []string
}

func (f fakeCoverageEC2) DescribeSubnets(context.Context, *ec2.DescribeSubnetsInput, ...func(*ec2.Options)) (*ec2.DescribeSubnetsOutput, error) {
	out := &ec2.DescribeSubnetsOutput{}
	for _, az := range f.defaultSubnets {
		out.Subnets = append(out.Subnets, ec2types.Subnet{AvailabilityZone: awssdk.String(az), DefaultForAz: awssdk.Bool(true)})
	}
	return out, nil
}

func (f fakeCoverageEC2) DescribeInstanceTypeOfferings(_ context.Context, in *ec2.DescribeInstanceTypeOfferingsInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstanceTypeOfferingsOutput, error) {
	out := &ec2.DescribeInstanceTypeOfferingsOutput{}
	for _, flt := range in.Filters {
		for _, typ := range flt.Values {
			for _, az := range f.offered {
				out.InstanceTypeOfferings = append(out.InstanceTypeOfferings, ec2types.InstanceTypeOffering{
					InstanceType: ec2types.InstanceType(typ), Location: awssdk.String(az), LocationType: ec2types.LocationTypeAvailabilityZone})
			}
		}
	}
	return out, nil
}

// withSubnetCoverage points lagotto_watch's coverage check at f.
func withSubnetCoverage(t *testing.T, f fakeCoverageEC2) {
	t.Helper()
	prev := newSubnetCoverageChecker
	newSubnetCoverageChecker = func(awssdk.Config) *watcher.SubnetCoverageChecker {
		return watcher.NewSubnetCoverageCheckerWith(
			func(string) watcher.SubnetCoverageEC2 { return f },
			func(context.Context) ([]string, error) { return []string{"us-west-2"}, nil })
	}
	t.Cleanup(func() { newSubnetCoverageChecker = prev })
}

// landable is a coverage where us-west-2a both offers the type and has a
// default subnet.
var landable = fakeCoverageEC2{offered: []string{"us-west-2a"}, defaultSubnets: []string{"us-west-2a"}}

func callWatch(t *testing.T, args map[string]any) (string, bool) {
	t.Helper()
	res, err := handleLagottoWatch(context.Background(), newRequest(args))
	if err != nil || res == nil {
		t.Fatalf("transport error: %v %+v", err, res)
	}
	return resultText(res), res.IsError
}

func callAck(t *testing.T, id string) (string, bool) {
	t.Helper()
	res, err := handleLagottoAck(context.Background(), newRequest(map[string]any{"watch_id": id}))
	if err != nil || res == nil {
		t.Fatalf("transport error: %v %+v", err, res)
	}
	return resultText(res), res.IsError
}

// untilLandedHold is the arguments of a confirmed until-landed hold watch.
func untilLandedHold() map[string]any {
	return map[string]any{
		"pattern": "g5.xlarge", "regions": "us-west-2", "action": "hold", "until_landed": true,
		"notify": "sns:arn:aws:sns:us-west-2:123456789012:mine", "confirm": true,
	}
}

func TestWatchLifetime(t *testing.T) {
	for _, tc := range []struct {
		name        string
		ttl, remind string
		ul          bool
		want        watcher.Lifetime
		err         string
	}{
		{name: "default ttl", want: watcher.Lifetime{TTL: 24 * time.Hour}},
		{name: "short-form ttl", ttl: "7d", want: watcher.Lifetime{TTL: 7 * 24 * time.Hour}},
		{name: "zero ttl", ttl: "0s", err: "must be greater than zero (to keep hunting until a launch lands, use until_landed: true"},
		{name: "remind needs until_landed", ttl: "24h", remind: "7d", err: "remind only applies to until_landed"},
		{name: "until_landed default remind", ul: true,
			want: watcher.Lifetime{UntilLanded: true, RemindEvery: watcher.DefaultRemindEvery, RemindPauseAfter: watcher.DefaultRemindPauseAfter}},
		{name: "until_landed remind", ul: true, remind: "2d",
			want: watcher.Lifetime{UntilLanded: true, RemindEvery: 48 * time.Hour, RemindPauseAfter: watcher.DefaultRemindPauseAfter}},
		{name: "until_landed and ttl", ul: true, ttl: "24h", err: "mutually exclusive"},
		{name: "bad remind", ul: true, remind: "soon", err: "invalid remind"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := watchLifetime(tc.ttl, tc.ul, tc.remind)
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("err = %v, want one containing %q", err, tc.err)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("watchLifetime = %+v, %v; want %+v", got, err, tc.want)
			}
		})
	}
}

// TestLagottoWatch_UntilLandedRefusedBeforeAWS: lagotto's until-landed rules
// and the confirm requirement are checked before any AWS call (#92), worded
// with this tool's argument names.
func TestLagottoWatch_UntilLandedRefusedBeforeAWS(t *testing.T) {
	withNotify := func(m map[string]any) map[string]any {
		m["pattern"], m["until_landed"] = "g5.xlarge", true
		return m
	}
	for _, tc := range []struct {
		name string
		args map[string]any
		want []string
	}{
		{"notify action", withNotify(map[string]any{"action": "notify", "notify": "sns:arn:aws:sns:us-west-2:1:t"}), []string{"until_landed"}},
		{"no channel", withNotify(map[string]any{"action": "hold", "confirm": true}), []string{"until_landed action=hold needs a notify channel"}},
		{"remind too short", withNotify(map[string]any{"action": "hold", "remind": "10m", "notify": "sns:arn:aws:sns:us-west-2:1:t", "confirm": true}),
			[]string{"remind must be at least 1h"}},
		{"ttl and until_landed", withNotify(map[string]any{"action": "hold", "ttl": "24h", "notify": "sns:arn:aws:sns:us-west-2:1:t", "confirm": true}),
			[]string{"mutually exclusive"}},
		{"hold without confirm", withNotify(map[string]any{"action": "hold", "regions": "us-west-2", "notify": "sns:arn:aws:sns:us-west-2:1:t"}),
			[]string{"needs confirmation", "billable capacity reservation for g5.xlarge", "billed for 30 minutes", "if this lands",
				"lagotto_ack within 21d (3 × the 7d remind interval)", "confirm: true", "no watch was created"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeAWS(t)
			f.install(t)
			text, isErr := callWatch(t, tc.args)
			if !isErr {
				t.Fatalf("accepted:\n%s", text)
			}
			for _, w := range tc.want {
				if !strings.Contains(text, w) {
					t.Errorf("refusal missing %q:\n%s", w, text)
				}
			}
			if strings.Contains(text, "--until-landed") || strings.Contains(text, "--notify") {
				t.Errorf("refusal names CLI flags instead of the tool's arguments:\n%s", text)
			}
			if len(f.actions) != 0 {
				t.Errorf("AWS calls %v before the refusal, want none", f.actions)
			}
		})
	}
}

// TestLagottoWatch_UntilLandedHoldIsStored: a confirmed until-landed hold watch
// is stored as lagotto's NewWatch builds it: until_landed, the remind cadence,
// the first acknowledgement deadline in expires_at, and the retention horizon
// after it. The result shows the deadline, how to move it, and the exposure.
func TestLagottoWatch_UntilLandedHoldIsStored(t *testing.T) {
	f := newFakeAWS(t)
	f.schedule = pollerSchedule("ENABLED")
	f.install(t)
	withSubnetCoverage(t, landable)

	before := time.Now().UTC()
	text, isErr := callWatch(t, untilLandedHold())
	if isErr {
		t.Fatalf("lagotto_watch failed:\n%s", text)
	}
	item := putWatchItem(t, f)
	if item["until_landed"]["BOOL"] != true {
		t.Errorf("until_landed = %v, want true", item["until_landed"])
	}
	if got := item["remind_every_seconds"]["N"]; got != "604800" {
		t.Errorf("remind_every_seconds = %v, want 604800 (7d)", got)
	}
	if got := item["remind_pause_after"]["N"]; got != "2" {
		t.Errorf("remind_pause_after = %v, want 2", got)
	}
	expiresAt, err := time.Parse(time.RFC3339Nano, item["expires_at"]["S"].(string))
	if err != nil {
		t.Fatal(err)
	}
	if d := expiresAt.Sub(before); d < 21*24*time.Hour-time.Minute || d > 21*24*time.Hour+time.Minute {
		t.Errorf("expires_at is %s after creation, want the 21d acknowledgement deadline", d)
	}
	ttl, _ := strconv.ParseInt(item["ttl_timestamp"]["N"].(string), 10, 64)
	if ttl != watcher.RetentionTTL(expiresAt) {
		t.Errorf("ttl_timestamp = %d, want RetentionTTL(expires_at) = %d", ttl, watcher.RetentionTTL(expiresAt))
	}
	lead := strings.SplitN(text, "\n", 2)[0]
	if !strings.Contains(lead, "action hold, until landed, pauses in 21d 0h 0m unless acked; poller on") {
		t.Errorf("summary line %q does not say until landed", lead)
	}
	for _, want := range []string{"Expires: until landed; pauses", "lagotto_ack watch_id w-", "each ack moves it 21d ahead",
		"Exposure: if this lands:", "static"} {
		if !strings.Contains(text, want) {
			t.Errorf("result missing %q:\n%s", want, text)
		}
	}
}

// TestLagottoWatch_HoldThatCannotLandIsRefused is the default-subnet check
// (lagotto#256): a hold watch whose only region offers the type in an AZ with
// no default subnet can never land, so it is refused before anything is
// stored, as `lagotto watch` refuses it.
func TestLagottoWatch_HoldThatCannotLandIsRefused(t *testing.T) {
	f := newFakeAWS(t)
	f.schedule = pollerSchedule("ENABLED")
	f.install(t)
	withSubnetCoverage(t, fakeCoverageEC2{offered: []string{"us-west-2a"}, defaultSubnets: []string{"us-west-2b"}})

	text, isErr := callWatch(t, untilLandedHold())
	if !isErr {
		t.Fatalf("accepted a hold watch that can never land:\n%s", text)
	}
	for _, want := range []string{"can never launch in us-west-2", "create-default-subnet", "No watch was created"} {
		if !strings.Contains(text, want) {
			t.Errorf("refusal missing %q:\n%s", want, text)
		}
	}
	if len(f.ddbPuts) != 0 {
		t.Errorf("PutItem calls = %d, want none", len(f.ddbPuts))
	}
}

// createUntilLanded creates an until-landed hold watch through the tool and
// returns its stored item.
func createUntilLanded(t *testing.T, f *fakeAWS) map[string]map[string]any {
	t.Helper()
	withSubnetCoverage(t, landable)
	if text, isErr := callWatch(t, untilLandedHold()); isErr {
		t.Fatalf("lagotto_watch failed:\n%s", text)
	}
	return putWatchItem(t, f)
}

// pause stages item as the poller leaves a watch whose deadline passed.
func pause(item map[string]map[string]any) {
	item["status"] = map[string]any{"S": string(watcher.StatusExpired)}
	item["expired_reason"] = map[string]any{"S": watcher.ExpiredReasonUnacknowledged}
	item["expires_at"] = map[string]any{"S": time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)}
}

// TestLagottoAck_ActiveWatchMovesTheDeadline: acknowledging an active
// until-landed watch moves its deadline a full window from now, through
// ExtendWatch without reactivating, and touches no schedule.
func TestLagottoAck_ActiveWatchMovesTheDeadline(t *testing.T) {
	f := newFakeAWS(t)
	f.schedule = pollerSchedule("ENABLED")
	f.install(t)
	id := createUntilLanded(t, f)["watch_id"]["S"].(string)
	f.actions = nil

	before := time.Now().UTC()
	text, isErr := callAck(t, id)
	if isErr {
		t.Fatalf("lagotto_ack failed:\n%s", text)
	}
	if len(f.ddbUpdates) != 1 {
		t.Fatalf("UpdateItem calls = %d, want 1 (actions %v)", len(f.ddbUpdates), f.actions)
	}
	up := f.ddbUpdates[0]
	if up.WatchID != id || strings.Contains(up.Expression, "#st") || strings.Contains(up.Expression, "REMOVE") {
		t.Errorf("update %+v, want an ExtendWatch of %s without reactivation", up, id)
	}
	exp, err := time.Parse(time.RFC3339, up.Values[":exp"]["S"].(string))
	if err != nil {
		t.Fatal(err)
	}
	if d := exp.Sub(before); d < 21*24*time.Hour-time.Minute || d > 21*24*time.Hour+time.Minute {
		t.Errorf("new deadline is %s from now, want 21d", d)
	}
	if indexOf(f.actions, "scheduler:") >= 0 {
		t.Errorf("an active watch's ack touched the schedule: %v", f.actions)
	}
	lead := strings.SplitN(text, "\n", 2)[0]
	if !strings.HasPrefix(lead, "✅ Acknowledged watch "+id+": g5.xlarge hunts until it lands, pauses in 21d 0h 0m unless acked again") ||
		strings.Contains(lead, "resumed") {
		t.Errorf("summary line %q", lead)
	}
}

// TestLagottoAck_PausedWatchResumesAndRearmsThePoller is the CLI's reactivate
// path: preflight the schedule, ExtendWatch with reactivate (status active,
// expired_reason removed), then switch a DISABLED schedule back on, in that
// order.
func TestLagottoAck_PausedWatchResumesAndRearmsThePoller(t *testing.T) {
	f := newFakeAWS(t)
	f.schedule = pollerSchedule("ENABLED")
	f.install(t)
	item := createUntilLanded(t, f)
	id := item["watch_id"]["S"].(string)
	pause(item)
	f.schedule.State = "DISABLED"
	f.actions = nil

	text, isErr := callAck(t, id)
	if isErr {
		t.Fatalf("lagotto_ack failed:\n%s", text)
	}
	if len(f.ddbUpdates) != 1 {
		t.Fatalf("UpdateItem calls = %d, want 1", len(f.ddbUpdates))
	}
	up := f.ddbUpdates[0]
	if !strings.Contains(up.Expression, "#st = :active") || !strings.Contains(up.Expression, "REMOVE expired_reason") {
		t.Errorf("UpdateExpression %q, want the reactivating ExtendWatch", up.Expression)
	}
	get, upd, sched := indexOf(f.actions, "scheduler:GetSchedule"), indexOf(f.actions, "dynamodb:UpdateItem"), indexOf(f.actions, "scheduler:UpdateSchedule")
	if get < 0 || upd < 0 || sched < 0 || get >= upd || upd >= sched {
		t.Errorf("order = %v, want GetSchedule < UpdateItem < UpdateSchedule", f.actions)
	}
	for _, want := range []string{"Reactivated expired watch " + id, "switched the hosted poller's schedule lagotto-capacity-poller back on"} {
		if !strings.Contains(text, want) {
			t.Errorf("result missing %q:\n%s", want, text)
		}
	}
	if lead := strings.SplitN(text, "\n", 2)[0]; !strings.HasSuffix(lead, "unless acked again; resumed") {
		t.Errorf("summary line %q does not say it resumed", lead)
	}
}

// TestLagottoAck_EnableFailurePutsTheWatchBack: if the schedule cannot be
// switched on, the watch is expired again with its reason (paused), as the
// CLI does, and the call is refused saying so.
func TestLagottoAck_EnableFailurePutsTheWatchBack(t *testing.T) {
	f := newFakeAWS(t)
	f.schedule = pollerSchedule("ENABLED")
	f.install(t)
	item := createUntilLanded(t, f)
	id := item["watch_id"]["S"].(string)
	pause(item)
	f.schedule.State = "DISABLED"
	f.schedUpdateErr = "AccessDeniedException"

	text, isErr := callAck(t, id)
	if !isErr {
		t.Fatalf("lagotto_ack succeeded although the schedule could not be enabled:\n%s", text)
	}
	if len(f.ddbUpdates) != 2 {
		t.Fatalf("UpdateItem calls = %d, want 2 (resume, then put back)", len(f.ddbUpdates))
	}
	if got := f.ddbUpdates[1].Values[":reason"]["S"]; got != watcher.ExpiredReasonUnacknowledged {
		t.Errorf("put-back reason = %v, want %s", got, watcher.ExpiredReasonUnacknowledged)
	}
	for _, want := range []string{"cannot reactivate watch " + id, "scheduler:UpdateSchedule", "the watch is still expired"} {
		if !strings.Contains(text, want) {
			t.Errorf("refusal missing %q:\n%s", want, text)
		}
	}
}

// TestLagottoAck_UnreadableScheduleRefusesBeforeTheWrite: a paused watch whose
// poller schedule can't be read is left untouched.
func TestLagottoAck_UnreadableScheduleRefusesBeforeTheWrite(t *testing.T) {
	f := newFakeAWS(t)
	f.schedule = pollerSchedule("ENABLED")
	f.install(t)
	item := createUntilLanded(t, f)
	pause(item)
	f.schedGetErr = "AccessDeniedException"

	text, isErr := callAck(t, item["watch_id"]["S"].(string))
	if !isErr || !strings.Contains(text, "nothing was changed") {
		t.Fatalf("want a refusal saying nothing was changed, got (error=%v):\n%s", isErr, text)
	}
	if len(f.ddbUpdates) != 0 {
		t.Errorf("UpdateItem calls = %d, want none", len(f.ddbUpdates))
	}
}

func TestLagottoAck_Refusals(t *testing.T) {
	for _, tc := range []struct {
		name  string
		stage func(item map[string]map[string]any)
		want  string
	}{
		{"another owner's watch", func(item map[string]map[string]any) {
			item["user_id"] = map[string]any{"S": "arn:aws:iam::123456789012:user/someone-else"}
		}, "not found"},
		{"a TTL watch", func(item map[string]map[string]any) {
			delete(item, "until_landed")
			delete(item, "remind_every_seconds")
		}, "is not an until-landed watch"},
		{"a cancelled watch", func(item map[string]map[string]any) {
			item["status"] = map[string]any{"S": string(watcher.StatusCancelled)}
		}, "is not active (status: cancelled)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeAWS(t)
			f.schedule = pollerSchedule("ENABLED")
			f.install(t)
			item := createUntilLanded(t, f)
			tc.stage(item)
			text, isErr := callAck(t, item["watch_id"]["S"].(string))
			if !isErr || !strings.Contains(text, tc.want) {
				t.Fatalf("want a refusal containing %q, got (error=%v):\n%s", tc.want, isErr, text)
			}
			if len(f.ddbUpdates) != 0 {
				t.Errorf("UpdateItem calls = %d, want none", len(f.ddbUpdates))
			}
		})
	}
	f := newFakeAWS(t)
	f.install(t)
	if text, isErr := callAck(t, "w-00000000"); !isErr || !strings.Contains(text, "not found") {
		t.Errorf("a missing watch: (error=%v) %s", isErr, text)
	}
	if text, isErr := callAck(t, " "); !isErr || !strings.Contains(text, "watch_id is required") {
		t.Errorf("an empty watch_id: (error=%v) %s", isErr, text)
	}
}

// TestLagottoStatus_ShowsThePauseAndTheAckDeadline is #92's gap 4 through the
// handler: a paused watch says why and how to resume it; an active one shows
// its acknowledgement deadline and the ack tool, and the exposure.
func TestLagottoStatus_ShowsThePauseAndTheAckDeadline(t *testing.T) {
	f := newFakeAWS(t)
	f.schedule = pollerSchedule("ENABLED")
	f.install(t)
	item := createUntilLanded(t, f)
	id := item["watch_id"]["S"].(string)
	status := func() (string, string) {
		t.Helper()
		res, err := handleLagottoStatus(context.Background(), newRequest(map[string]any{"watch_id": id}))
		if err != nil || res.IsError {
			t.Fatalf("lagotto_status: %v %s", err, resultText(res))
		}
		text := resultText(res)
		return strings.SplitN(text, "\n", 2)[0], text
	}

	lead, text := status()
	if !strings.Contains(lead, "active") || !strings.Contains(lead, "until landed, ack due in 2") {
		t.Errorf("active summary line %q", lead)
	}
	for _, want := range []string{"Expires:  until landed; pauses", "unless acknowledged", "Acknowledge with lagotto_ack (watch_id " + id + ")",
		"21d ahead (3 × the 7d remind interval)", "Exposure: if this lands:"} {
		if !strings.Contains(text, want) {
			t.Errorf("active status missing %q:\n%s", want, text)
		}
	}

	pause(item)
	item["attempts"] = map[string]any{"N": "42"}
	item["last_error"] = map[string]any{"S": "capacity: InsufficientInstanceCapacity: none left"}
	lead, text = status()
	if !strings.Contains(lead, "paused (unacked)") || !strings.Contains(lead, "lagotto_ack resumes it") {
		t.Errorf("paused summary line %q", lead)
	}
	for _, want := range []string{"PAUSED at", "not acknowledged in time (expired_reason: unacknowledged)", "Resume the hunt with lagotto_ack",
		"Attempts: 42", "Last error: capacity: InsufficientInstanceCapacity"} {
		if !strings.Contains(text, want) {
			t.Errorf("paused status missing %q:\n%s", want, text)
		}
	}
}

func TestExpiresColumn(t *testing.T) {
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	ttlWatch := watcher.Watch{Status: watcher.StatusActive, ExpiresAt: now.Add(time.Hour)}
	ul := watcher.Watch{Status: watcher.StatusActive, UntilLanded: true, RemindEverySeconds: 604800, RemindPauseAfter: 2, ExpiresAt: now.Add(4 * 24 * time.Hour)}
	paused := ul
	paused.Status, paused.ExpiredReason = watcher.StatusExpired, watcher.ExpiredReasonUnacknowledged
	due := ul
	due.ExpiresAt = now.Add(-time.Minute)
	for _, tc := range []struct {
		name string
		w    watcher.Watch
		want string
	}{
		{"ttl", ttlWatch, "2026-11-07T13:00:00Z"},
		{"until landed", ul, "until landed (ack in 4d 0h 0m, by 2026-11-11T12:00:00Z)"},
		{"paused", paused, "paused (unacked): resume it with lagotto_ack"},
		{"due", due, "until landed (ack due)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := expiresColumn(&tc.w, now); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestUndeliverableUntilLandedWarning(t *testing.T) {
	w := watcher.Watch{WatchID: "w-1", UntilLanded: true, NotifyChannels: []watcher.NotifyChannel{{Type: "email", Target: "a@b.c", Subscription: watcher.SubPending}}}
	if got := undeliverableUntilLandedWarning(&w); !strings.Contains(got, "none of this watch's email addresses has confirmed") {
		t.Errorf("pending-only: %q", got)
	}
	w.NotifyChannels = append(w.NotifyChannels, watcher.NotifyChannel{Type: "sns", Target: "arn:aws:sns:us-west-2:1:t"})
	if got := undeliverableUntilLandedWarning(&w); got != "" {
		t.Errorf("with an sns channel: %q, want none", got)
	}
}
