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
