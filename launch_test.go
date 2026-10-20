package main

import (
	"context"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
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
