package main

import (
	"context"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/spore-host/spawn/pkg/taskproto"
)

// resultText extracts the concatenated text content of a tool result, for
// asserting on error/plan messages in tests.
func resultText(res *mcp.CallToolResult) string {
	if res == nil {
		return ""
	}
	var sb strings.Builder
	for _, c := range res.Content {
		if tc, ok := mcp.AsTextContent(c); ok {
			sb.WriteString(tc.Text)
		}
	}
	return sb.String()
}

// fakeFinder is an AWS-free taskproto.InstanceFinder for exercising the dry-run
// sizing/plan path without any network call.
type fakeFinder struct{}

func (fakeFinder) FindCandidates(_ context.Context, _ taskproto.ResourceRequest) ([]taskproto.Candidate, error) {
	return []taskproto.Candidate{
		{InstanceType: "c7i.large", Family: "c7i", VCPUs: 2, MemoryGiB: 4, Architecture: "x86_64", OnDemandPrice: 0.085},
		{InstanceType: "c7i.xlarge", Family: "c7i", VCPUs: 4, MemoryGiB: 8, Architecture: "x86_64", OnDemandPrice: 0.17},
	}, nil
}

func TestRegisterLaunchAndLagottoTools(t *testing.T) {
	// Registration must not panic and must accept every new tool onto a server.
	s := server.NewMCPServer("test", "0.0.0", server.WithToolCapabilities(true))
	registerLaunchTools(s)
	registerLagottoTools(s)
}

// TestSpawnTaskRun_RequiresTTL is the mandatory cost guardrail: a TaskSpec with
// no lifecycle.ttl is rejected BEFORE any AWS call, with a TTL-specific message.
func TestSpawnTaskRun_RequiresTTL(t *testing.T) {
	// Valid otherwise, but no lifecycle.ttl → must be rejected.
	spec := `{"task_id":"t1","command":["echo","hi"],"resources":{"cpu":2},"lifecycle":{"on_complete":"terminate"}}`
	res, err := handleSpawnTaskRun(context.Background(), newRequest(map[string]any{"spec": spec, "dry_run": true}))
	if err != nil {
		t.Fatalf("transport error: %v", err)
	}
	if res == nil || !res.IsError {
		t.Fatalf("expected an error result for a spec with no TTL, got %+v", res)
	}
	if !strings.Contains(resultText(res), "TTL") {
		t.Errorf("expected a TTL-specific rejection, got: %q", resultText(res))
	}
}

func TestSpawnTaskRun_EmptySpec(t *testing.T) {
	res, err := handleSpawnTaskRun(context.Background(), newRequest(map[string]any{"spec": ""}))
	if err != nil {
		t.Fatalf("transport error: %v", err)
	}
	if res == nil || !res.IsError {
		t.Errorf("expected an error result for an empty spec, got %+v", res)
	}
}

// TestSpawnTaskRun_RejectsPlacementStorage: a spec with placement storage is
// refused up front (the MCP path doesn't wire the boot-time mount script), before
// any launch. TTL is present so we get past the TTL gate to the storage check.
func TestSpawnTaskRun_RejectsPlacementStorage(t *testing.T) {
	spec := `{"task_id":"t1","command":["echo","hi"],"resources":{"instance_type":"c7i.large"},"lifecycle":{"ttl":"4h"},"placement":{"efs_id":"fs-abc"}}`
	res, err := handleSpawnTaskRun(context.Background(), newRequest(map[string]any{"spec": spec, "dry_run": true}))
	if err != nil {
		t.Fatalf("transport error: %v", err)
	}
	if res == nil || !res.IsError {
		t.Fatalf("expected an error result for placement storage, got %+v", res)
	}
	if !strings.Contains(resultText(res), "placement storage") {
		t.Errorf("expected a placement-storage rejection, got: %q", resultText(res))
	}
}

// TestRenderTaskPlan_NoAWS exercises the dry-run plan rendering with a fake finder
// (no AWS): it must size, pick the cheapest fitting type, and NOT claim to launch.
func TestRenderTaskPlan_NoAWS(t *testing.T) {
	spec, err := taskproto.ParseSpec([]byte(`{"task_id":"plan1","command":["run.sh"],"resources":{"cpu":2,"memory_gib":3},"lifecycle":{"ttl":"2h"}}`))
	if err != nil {
		t.Fatalf("ParseSpec: %v", err)
	}
	res, err := renderTaskPlan(context.Background(), spec, fakeFinder{}, "us-east-1")
	if err != nil {
		t.Fatalf("transport error: %v", err)
	}
	if res == nil || res.IsError {
		t.Fatalf("expected a non-error plan result, got %+v", res)
	}
	out := resultText(res)
	for _, want := range []string{"DRY RUN", "plan1", "c7i.large", "TTL:", "2h"} {
		if !strings.Contains(out, want) {
			t.Errorf("plan output missing %q; got:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Task launched") {
		t.Errorf("dry-run plan must not claim a launch: %q", out)
	}
}

// TestSpawnAppLaunch_RequiresTTL: the app-launch cost guardrail — no ttl → reject
// before catalog lookup or AWS.
func TestSpawnAppLaunch_RequiresTTL(t *testing.T) {
	res, err := handleSpawnAppLaunch(context.Background(), newRequest(map[string]any{"app": "paraview"}))
	if err != nil {
		t.Fatalf("transport error: %v", err)
	}
	if res == nil || !res.IsError {
		t.Fatalf("expected an error result with no ttl, got %+v", res)
	}
	if !strings.Contains(resultText(res), "TTL") {
		t.Errorf("expected a TTL-required rejection, got: %q", resultText(res))
	}
}

func TestSpawnAppLaunch_RejectsBadTTL(t *testing.T) {
	res, err := handleSpawnAppLaunch(context.Background(), newRequest(map[string]any{"app": "paraview", "ttl": "7d"}))
	if err != nil {
		t.Fatalf("transport error: %v", err)
	}
	if res == nil || !res.IsError {
		t.Errorf("expected an error result for a non-Go-duration ttl, got %+v", res)
	}
}

func TestSpawnAppLaunch_UnknownApp(t *testing.T) {
	res, err := handleSpawnAppLaunch(context.Background(), newRequest(map[string]any{"app": "definitely-not-an-app", "ttl": "4h"}))
	if err != nil {
		t.Fatalf("transport error: %v", err)
	}
	if res == nil || !res.IsError {
		t.Fatalf("expected an error result for an unknown app, got %+v", res)
	}
	if !strings.Contains(resultText(res), "not found in catalog") {
		t.Errorf("expected a catalog not-found error, got: %q", resultText(res))
	}
}

// TestTaskStagingPolicy checks the scoped S3 policy grants read on inputs and
// write on outputs + the results bucket — the exact access the wrapper needs.
func TestTaskStagingPolicy(t *testing.T) {
	pol := taskStagingPolicy([]string{"in-bkt"}, []string{"out-bkt"}, "spawn-results-1-us-east-1", nil)
	for _, want := range []string{
		`"arn:aws:s3:::in-bkt/*"`,
		`"arn:aws:s3:::out-bkt/*"`,
		`"arn:aws:s3:::spawn-results-1-us-east-1/*"`,
		`"s3:PutObject"`,
		`"s3:GetObject"`,
	} {
		if !strings.Contains(pol, want) {
			t.Errorf("policy missing %q; got: %s", want, pol)
		}
	}
}

func TestS3Bucket(t *testing.T) {
	cases := map[string]string{
		"s3://my-bucket/key/path": "my-bucket",
		"s3://bare-bucket":        "bare-bucket",
		"/local/path":             "",
		"":                        "",
	}
	for in, want := range cases {
		if got := s3Bucket(in); got != want {
			t.Errorf("s3Bucket(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTaskAMIPlan(t *testing.T) {
	// Explicit AMI wins.
	spec := &taskproto.TaskSpec{}
	spec.Placement.AMI = "ami-123"
	if got := taskAMIPlan(spec, "c7i.large"); !strings.Contains(got, "ami-123") {
		t.Errorf("explicit AMI not honored: %q", got)
	}
	// GPU family gets the DLAMI note.
	if got := taskAMIPlan(&taskproto.TaskSpec{}, "g5.xlarge"); !strings.Contains(got, "GPU") {
		t.Errorf("GPU instance should get a GPU DLAMI plan: %q", got)
	}
}
