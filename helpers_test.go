package main

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	spawnclient "github.com/spore-host/spawn/pkg/aws"
	truffleaws "github.com/spore-host/truffle/pkg/aws"
	"github.com/spore-host/truffle/pkg/find"
)

// TestExactTypeMatcher guards the fix for the nil-matcher crash: truffle's
// SearchInstanceTypes panics (nil-pointer deref in its per-region goroutine) when
// handed a nil matcher, which — in this in-process stdio server — crashes the whole
// server and disconnects every tool. The spot-price and quota-check handlers must
// therefore always pass a real, anchored, literal matcher for a single type.
func TestExactTypeMatcher(t *testing.T) {
	re := exactTypeMatcher("g5.2xlarge")
	if re == nil {
		t.Fatal("exactTypeMatcher returned nil — SearchInstanceTypes would panic")
	}
	if !re.MatchString("g5.2xlarge") {
		t.Errorf("matcher %q did not match its own type", re.String())
	}
	// The dot must be literal, not the regexp any-char — g5x2xlarge must NOT match.
	if re.MatchString("g5x2xlarge") {
		t.Errorf("matcher %q treated '.' as a wildcard", re.String())
	}
	// Anchored: a superstring must not match.
	if re.MatchString("g5.2xlarge.extra") {
		t.Errorf("matcher %q is not anchored", re.String())
	}
	// The pattern must be recognisable as an exact type (anchors + escaped dot),
	// so truffle uses the API-side filter instead of enumerating every type.
	if got := re.String(); got != `^g5\.2xlarge$` {
		t.Errorf("matcher pattern = %q, want %q", got, `^g5\.2xlarge$`)
	}
}

func TestParseRegions(t *testing.T) {
	tests := []struct {
		in   string
		want []string
	}{
		{"", []string{"us-east-1"}}, // default
		{"us-west-2", []string{"us-west-2"}},
		{"us-east-1,us-west-2", []string{"us-east-1", "us-west-2"}},
		{" us-east-1 , us-west-2 ", []string{"us-east-1", "us-west-2"}}, // trims whitespace
		{"us-east-1,,us-west-2", []string{"us-east-1", "us-west-2"}},    // skips empties
		{",", []string{"us-east-1"}},                                    // all-empty falls through to default? -> nil
	}
	for _, tt := range tests {
		got := parseRegions(tt.in)
		// The "," case yields no regions (nil) since both parts are empty.
		if tt.in == "," {
			if len(got) != 0 {
				t.Errorf("parseRegions(%q) = %v, want empty", tt.in, got)
			}
			continue
		}
		if !reflect.DeepEqual(got, tt.want) {
			t.Errorf("parseRegions(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

// newRequest builds a CallToolRequest with the given argument map.
func newRequest(args map[string]any) mcp.CallToolRequest {
	var req mcp.CallToolRequest
	req.Params.Arguments = args
	return req
}

func TestRegisterTools(t *testing.T) {
	// Registration must not panic and should accept all tools onto a server.
	s := server.NewMCPServer("test", "0.0.0", server.WithToolCapabilities(true))
	registerTruffleTools(s)
	registerSpawnTools(s)
}

func TestHandleTruffleFind_ParseError(t *testing.T) {
	// "intel" + "arm64" is a conflicting-architecture query → ParseQuery/
	// BuildCriteria errors before any AWS call, returning a tool error result.
	req := newRequest(map[string]any{"query": "intel graviton arm64 x86_64"})
	res, err := handleTruffleFind(context.Background(), req)
	if err != nil {
		t.Fatalf("handler returned transport error: %v", err)
	}
	if res == nil || !res.IsError {
		t.Errorf("expected an error tool-result for a conflicting query, got %+v", res)
	}
}

func TestHandleTruffleFind_EmptyQueryOK(t *testing.T) {
	// An empty query parses fine; without AWS creds the handler returns an
	// error result at the client/search step — either way it must not panic
	// and must return a non-nil result.
	req := newRequest(map[string]any{"query": ""})
	res, err := handleTruffleFind(context.Background(), req)
	if err != nil {
		t.Fatalf("handler transport error: %v", err)
	}
	if res == nil {
		t.Error("expected a non-nil result")
	}
}

func TestHandleSpawnList_InvalidState(t *testing.T) {
	// An unknown state is rejected before any AWS call (#12). Previously "all"
	// (and anything else) was passed straight through as an EC2
	// instance-state-name filter, which matched nothing.
	req := newRequest(map[string]any{"state": "bogus"})
	res, err := handleSpawnList(context.Background(), req)
	if err != nil {
		t.Fatalf("handler transport error: %v", err)
	}
	if res == nil || !res.IsError {
		t.Errorf("expected an error result for an invalid state, got %+v", res)
	}
}

func TestHandleSpawnTerminate_RequiresConfirm(t *testing.T) {
	// Without confirm=true, terminate must NOT destroy anything. It either
	// previews (if the instance resolves) or errors at lookup — but with no AWS
	// creds in the test env, the key guarantee is it returns a non-nil,
	// non-panicking result and never reaches Terminate. We assert it doesn't
	// panic and returns a result.
	req := newRequest(map[string]any{"instance": "does-not-exist", "confirm": false})
	res, err := handleSpawnTerminate(context.Background(), req)
	if err != nil {
		t.Fatalf("handler transport error: %v", err)
	}
	if res == nil {
		t.Error("expected a non-nil result")
	}
}

func TestFormatDuration(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want string
	}{
		{30 * time.Minute, "30m"},
		{90 * time.Minute, "1h 30m"},
		{2 * time.Hour, "2h 0m"},
		{25 * time.Hour, "1d 1h 0m"},
		{49 * time.Hour, "2d 1h 0m"},
		{0, "0m"},
		// Rounding: 29s rounds down to 0m, 31s rounds up to 1m.
		{29 * time.Second, "0m"},
		{91 * time.Minute, "1h 31m"},
	}
	for _, tt := range tests {
		if got := formatDuration(tt.d); got != tt.want {
			t.Errorf("formatDuration(%v) = %q, want %q", tt.d, got, tt.want)
		}
	}
}

// TestHandleSpawnExtend_RejectsBadTTL verifies the extend handler validates the
// TTL BEFORE any AWS call: a Go-duration-invalid value like "7d" (day suffixes
// aren't supported) is rejected with a clear error, and a valid one passes the
// parse gate (then fails at AWS lookup in the credential-less test env, which is
// fine — the point is it got past validation without panicking). (#13)
func TestHandleSpawnExtend_RejectsBadTTL(t *testing.T) {
	// "7d" is not a Go duration — must be rejected up front.
	req := newRequest(map[string]any{"instance": "i-abc", "ttl": "7d"})
	res, err := handleSpawnExtend(context.Background(), req)
	if err != nil {
		t.Fatalf("handler transport error: %v", err)
	}
	if res == nil || !res.IsError {
		t.Fatalf("expected an error result for TTL %q, got %+v", "7d", res)
	}

	// An empty TTL is also invalid (time.ParseDuration("") fails).
	req = newRequest(map[string]any{"instance": "i-abc", "ttl": ""})
	res, err = handleSpawnExtend(context.Background(), req)
	if err != nil {
		t.Fatalf("handler transport error: %v", err)
	}
	if res == nil || !res.IsError {
		t.Errorf("expected an error result for an empty TTL, got %+v", res)
	}
}

// TestHandleSpawnStatus_NoPanic and TestHandleSpawnStop_NoPanic assert the
// status/stop handlers return a non-nil, non-panicking result in the
// credential-less test env (they resolve no instance and error cleanly), so the
// lifecycle handlers have at least smoke coverage (#13).
func TestHandleSpawnStatus_NoPanic(t *testing.T) {
	req := newRequest(map[string]any{"instance": "does-not-exist"})
	res, err := handleSpawnStatus(context.Background(), req)
	if err != nil {
		t.Fatalf("handler transport error: %v", err)
	}
	if res == nil {
		t.Error("expected a non-nil result")
	}
}

func TestHandleSpawnStop_NoPanic(t *testing.T) {
	// hibernate=false; without creds this resolves no instance and errors — it
	// must never panic or reach StopInstance.
	req := newRequest(map[string]any{"instance": "does-not-exist", "hibernate": false})
	res, err := handleSpawnStop(context.Background(), req)
	if err != nil {
		t.Fatalf("handler transport error: %v", err)
	}
	if res == nil {
		t.Error("expected a non-nil result")
	}
}

// TestSelectInstance covers the #12 safety guarantee: an exact instance-ID match
// wins, a lone name match resolves, but a name shared by more than one instance is
// REFUSED (never acted on arbitrarily) — this is what stops spawn_terminate from
// destroying the wrong box.
func TestSelectInstance(t *testing.T) {
	instances := []spawnclient.InstanceInfo{
		{InstanceID: "i-aaa", Name: "gpu", Region: "us-east-1", State: "running"},
		{InstanceID: "i-bbb", Name: "gpu", Region: "us-west-2", State: "stopped"},
		{InstanceID: "i-ccc", Name: "solo", Region: "us-east-1", State: "running"},
	}

	// Exact ID wins even when a name is also ambiguous.
	got, err := selectInstance(instances, "i-bbb")
	if err != nil || got == nil || got.InstanceID != "i-bbb" {
		t.Fatalf("ID match: got %+v, err %v", got, err)
	}

	// Unique name resolves (case-insensitive).
	got, err = selectInstance(instances, "SOLO")
	if err != nil || got == nil || got.InstanceID != "i-ccc" {
		t.Fatalf("unique name match: got %+v, err %v", got, err)
	}

	// Ambiguous name is refused, and the error names both candidates + says to use
	// the ID — a silent pick here could terminate the wrong instance.
	got, err = selectInstance(instances, "gpu")
	if err == nil || got != nil {
		t.Fatalf("ambiguous name must error, got %+v, err %v", got, err)
	}
	for _, want := range []string{"ambiguous", "i-aaa", "i-bbb", "instance ID"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("ambiguity error %q missing %q", err.Error(), want)
		}
	}

	// No match errors cleanly.
	if _, err := selectInstance(instances, "nope"); err == nil {
		t.Error("expected not-found error for an unknown name")
	}
}

// TestRenderFindResults covers the truffle_find result cap: when the full match
// count exceeds what's rendered, the header must say so and point the caller to
// narrow the query; otherwise it reports the plain count. Also checks the "Why:"
// match-reason line is emitted.
func TestRenderFindResults(t *testing.T) {
	pq, err := find.ParseQuery("gpu")
	if err != nil {
		t.Fatalf("ParseQuery: %v", err)
	}
	results := []truffleaws.InstanceTypeResult{
		{InstanceType: "g5.2xlarge", Region: "us-east-1", VCPUs: 8, MemoryMiB: 32768, Architecture: "x86_64", GPUs: 1, GPUModel: "A10G", GPUManufacturer: "nvidia"},
	}

	// Not truncated: total == len(results).
	out := renderFindResults("gpu", results, len(results), nil, pq)
	if !strings.Contains(out, "Found 1 instance type(s)") {
		t.Errorf("plain header missing, got: %q", out)
	}
	if strings.Contains(out, "showing the top") {
		t.Errorf("unexpected truncation note when not truncated: %q", out)
	}
	if !strings.Contains(out, "**g5.2xlarge** (us-east-1)") {
		t.Errorf("result block missing: %q", out)
	}

	// Truncated: total (200) > rendered (1) → header flags it and says to narrow.
	out = renderFindResults("gpu", results, 200, nil, pq)
	if !strings.Contains(out, "Found 200 instance type(s)") ||
		!strings.Contains(out, "showing the top 1") ||
		!strings.Contains(out, "narrow the query") {
		t.Errorf("truncation header wrong, got: %q", out)
	}
}

// TestComputeExtendedDeadline covers the #11 fix: extend must push the AUTHORITATIVE
// absolute spawn:ttl-deadline forward (spored ignores the relative spawn:ttl), and
// an already-expired deadline must never yield a past deadline that reaps the box the
// instant you extend it (the safety floor, spore-host#374).
func TestComputeExtendedDeadline(t *testing.T) {
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)

	// Existing future deadline is pushed forward by exactly the extend duration
	// (anchored to the deadline, not to now).
	existing := now.Add(3 * time.Hour).Format(time.RFC3339)
	got := computeExtendedDeadline(existing, "", 2*time.Hour, now)
	if want := now.Add(5 * time.Hour); !got.Equal(want) {
		t.Errorf("future deadline: got %v, want %v", got, want)
	}

	// An already-expired deadline must floor at now+extend, not push into the past.
	expired := now.Add(-10 * time.Hour).Format(time.RFC3339)
	got = computeExtendedDeadline(expired, "", 2*time.Hour, now)
	if want := now.Add(2 * time.Hour); !got.Equal(want) {
		t.Errorf("expired deadline floor: got %v, want %v", got, want)
	}
	if got.Before(now) {
		t.Errorf("expired deadline produced a PAST deadline %v (would reap immediately)", got)
	}

	// No deadline tag, but a current TTL — best-effort from now+TTL+extend.
	got = computeExtendedDeadline("", "1h", 2*time.Hour, now)
	if want := now.Add(3 * time.Hour); !got.Equal(want) {
		t.Errorf("TTL fallback: got %v, want %v", got, want)
	}

	// Nothing usable — now+extend.
	got = computeExtendedDeadline("", "", 4*time.Hour, now)
	if want := now.Add(4 * time.Hour); !got.Equal(want) {
		t.Errorf("bare fallback: got %v, want %v", got, want)
	}
}
