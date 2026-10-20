package main

import (
	"context"
	"sort"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	spawnclient "github.com/spore-host/spawn/pkg/aws"
)

// TestSpawnListStateAllSendsNoStateFilter is the #63 regression. state=all
// means every state, so the DescribeInstances call must carry no
// instance-state-name filter. spawn's "" sends the non-terminated filter
// (since spawn#527), which is what state=all was mapped to, so a task vanished
// from the list the moment its instance terminated.
//
// It reads the filter off the request spawn's real client sends, so it fails
// whichever layer drops "all".
func TestSpawnListStateAllSendsNoStateFilter(t *testing.T) {
	fake := newFakeAWS(t)
	fake.instances = []fakeInstance{
		{id: "i-run", name: "live", state: "running", instanceType: "t4g.small"},
		{id: "i-done", name: "kiro-demo", state: "terminated", instanceType: "r8g.2xlarge"},
	}
	fake.install(t)

	res, err := handleSpawnList(context.Background(), newRequest(map[string]any{"state": "all", "region": "us-west-2"}))
	if err != nil || res == nil || res.IsError {
		t.Fatalf("spawn_list state=all failed: %v %s", err, resultText(res))
	}
	if len(fake.describeCalls) != 1 {
		t.Fatalf("DescribeInstances called %d times, want 1", len(fake.describeCalls))
	}
	if states := fake.describeCalls[0]; states != nil {
		t.Errorf("state=all sent instance-state-name filter %v; it must send none, or terminated instances are dropped", states)
	}
	if !strings.Contains(resultText(res), "i-done") {
		t.Errorf("state=all omitted the terminated instance:\n%s", resultText(res))
	}

	// The other states still filter.
	fake.describeCalls = nil
	if _, err := handleSpawnList(context.Background(), newRequest(map[string]any{"state": "running", "region": "us-west-2"})); err != nil {
		t.Fatal(err)
	}
	if got := fake.describeCalls; len(got) != 1 || len(got[0]) != 1 || got[0][0] != "running" {
		t.Errorf("state=running sent state filters %v, want [[running]]", got)
	}
}

// TestOnlySpawnStatusResolvesAgainstTerminatedInstances gates the split #63
// asked for. spawn_status searches every state, so a task whose instance just
// terminated still resolves. Every tool that changes an instance resolves
// against non-terminated instances only: with "all", an old terminated
// instance sharing a live one's name would turn a valid name into an
// ambiguity error, or worse.
//
// The set of tools is discovered from the registered tools that take an
// "instance" argument, so a new one is covered without editing this test.
func TestOnlySpawnStatusResolvesAgainstTerminatedInstances(t *testing.T) {
	s := server.NewMCPServer("test", "0.0.0", server.WithToolCapabilities(true))
	registerSpawnTools(s)
	var tools []string
	for name, tool := range s.ListTools() {
		if _, ok := tool.Tool.InputSchema.Properties["instance"]; ok {
			tools = append(tools, name)
		}
	}
	sort.Strings(tools)
	if len(tools) < 2 || !contains(tools, "spawn_status") {
		t.Fatalf("discovered instance tools %v; the discovery is broken", tools)
	}

	for _, name := range tools {
		t.Run(name, func(t *testing.T) {
			fake := newFakeAWS(t)
			fake.instances = []fakeInstance{
				{id: "i-live", name: "job", state: "running", instanceType: "t4g.small"},
				{id: "i-old", name: "job", state: "terminated", instanceType: "t4g.small"},
			}
			fake.install(t)
			args := map[string]any{"instance": "job", "region": "us-west-2", "ttl": "2h", "confirm": false}
			if name == "spawn_status" {
				args["instance"] = "i-old"
			}
			res, err := s.GetTool(name).Handler(context.Background(), newRequest(args))
			if err != nil || res == nil {
				t.Fatalf("%s: %v %+v", name, err, res)
			}
			if len(fake.describeCalls) == 0 {
				t.Fatalf("%s never listed instances", name)
			}
			states := fake.describeCalls[0]
			out := resultText(res)

			if name == "spawn_status" {
				if states != nil {
					t.Errorf("spawn_status listed only states %v; it must search every state, terminated included", states)
				}
				if res.IsError || !strings.Contains(out, "i-old") || !strings.Contains(out, "terminated") {
					t.Errorf("spawn_status did not resolve the terminated instance by ID:\n%s", out)
				}
				return
			}
			if states == nil || contains(states, "terminated") || contains(states, "shutting-down") {
				t.Errorf("%s resolved against states %v; a tool that changes an instance must exclude terminated ones", name, states)
			}
			// It acted on, or previewed, the live instance.
			resolved := strings.Contains(out, "i-live") || (len(fake.mutated) > 0 && !contains(fake.mutated, "i-old"))
			if strings.Contains(out, "ambiguous") || !resolved {
				t.Errorf("%s did not resolve the name to the one live instance (changed %v):\n%s", name, fake.mutated, out)
			}
			if contains(fake.mutated, "i-old") {
				t.Errorf("%s changed the terminated instance", name)
			}
		})
	}
}

const finishedRecord = `{"task_id":"era5","run_id":"0b5e","exit_code":0,"state":"completed",
	"started_at":"2026-10-20T21:05:30Z","ended_at":"2026-10-20T21:23:30Z",
	"logs":["s3://spawn-results-123456789012-us-west-2/tasks/era5/command.log"]}`

func statusOf(t *testing.T, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	res, err := handleSpawnStatus(context.Background(), newRequest(args))
	if err != nil || res == nil {
		t.Fatalf("spawn_status: %v %+v", err, res)
	}
	return res
}

// TestStatusReportsAFinishedTaskFromItsRecord: once EC2 no longer lists a
// task's instance, spawn_status answers from the task's completion record at
// spawn's resolved results prefix (#63), not "not found".
func TestStatusReportsAFinishedTaskFromItsRecord(t *testing.T) {
	fake := newFakeAWS(t)
	fake.objects["spawn-results-123456789012-us-west-2/tasks/era5/completion.json"] = finishedRecord
	fake.install(t)

	res := statusOf(t, map[string]any{"instance": "era5", "region": "us-west-2"})
	out := resultText(res)
	if res.IsError {
		t.Fatalf("a finished task with a record was reported as an error:\n%s", out)
	}
	for _, want := range []string{
		"State:    completed",
		"Exit code: 0",
		"Ended because: the command exited 0",
		"Started:  2026-10-20T21:05:30Z",
		"Ended:    2026-10-20T21:23:30Z",
		"Wall time: 18m0s",
		"Cost:     not derivable", // no cost_estimate, and no instance to know the type of
		"Record:   s3://spawn-results-123456789012-us-west-2/tasks/era5/completion.json",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("status missing %q:\n%s", want, out)
		}
	}
}

// TestStatusReportsAFailedTaskAndWhy: the exit code and the reason come from
// the record's retry_class and terminal_reason.
func TestStatusReportsAFailedTaskAndWhy(t *testing.T) {
	for _, tc := range []struct{ name, record, want string }{
		{"app error", `{"task_id":"t","exit_code":3,"state":"failed","retry_class":"app_error","started_at":"2026-10-20T21:00:00Z","ended_at":"2026-10-20T21:01:00Z"}`,
			"the command exited non-zero (3)"},
		{"TTL kill", `{"task_id":"t","exit_code":137,"state":"failed","retry_class":"ttl_expired","terminal_reason":"ttl_expired","started_at":"2026-10-20T21:00:00Z","ended_at":"2026-10-20T21:45:00Z"}`,
			"its TTL expired while the command was still running"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFakeAWS(t)
			fake.objects["spawn-results-123456789012-us-west-2/tasks/t/completion.json"] = tc.record
			fake.install(t)
			out := resultText(statusOf(t, map[string]any{"instance": "t", "region": "us-west-2"}))
			if !strings.Contains(out, tc.want) || !strings.Contains(out, "State:    failed") {
				t.Errorf("want %q and state failed in:\n%s", tc.want, out)
			}
		})
	}
}

// TestStatusOfATerminatedTaskInstanceReportsTheRecordAndCost: while EC2 still
// lists the terminated instance, its type is known, so the cost is derived
// from the on-demand rate and the wall time, and labelled as an estimate of
// compute only.
func TestStatusOfATerminatedTaskInstanceReportsTheRecordAndCost(t *testing.T) {
	fake := newFakeAWS(t)
	fake.instances = []fakeInstance{{id: "i-0e55c943696f74966", name: "era5", state: "terminated", instanceType: "r8g.2xlarge",
		tags: map[string]string{"spawn:task-id": "era5"}}}
	fake.objects["spawn-results-123456789012-us-west-2/tasks/era5/completion.json"] = finishedRecord
	fake.install(t)
	prev := onDemandRate
	onDemandRate = func(_ context.Context, _ *spawnclient.Client, region, instanceType string) (float64, string, error) {
		if region != "us-west-2" || instanceType != "r8g.2xlarge" {
			t.Errorf("priced %s in %s", instanceType, region)
		}
		return 0.4, "test rate", nil
	}
	t.Cleanup(func() { onDemandRate = prev })

	out := resultText(statusOf(t, map[string]any{"instance": "era5", "region": "us-west-2"}))
	for _, want := range []string{
		"State:    terminated",
		"Exit code: 0",
		// 0.4 $/hr × 18 min = $0.12
		"Cost:     ~$0.1200 — estimate, compute only",
		"r8g.2xlarge on-demand $0.4000/hr (test rate) × wall time 18m0s",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("status missing %q:\n%s", want, out)
		}
	}
}

// TestStatusHonoursACustomResultsPrefix: a task launched with a results_prefix
// writes its record there, and spawn_status finds it when given the same
// prefix, as `spawn task status --results-prefix` does.
func TestStatusHonoursACustomResultsPrefix(t *testing.T) {
	fake := newFakeAWS(t)
	fake.objects["lab-results/runs/era5/completion.json"] = finishedRecord
	fake.install(t)
	res := statusOf(t, map[string]any{"instance": "era5", "region": "us-west-2", "results_prefix": "s3://lab-results/runs"})
	if res.IsError || !strings.Contains(resultText(res), "Record:   s3://lab-results/runs/era5/completion.json") {
		t.Errorf("custom results_prefix not honoured:\n%s", resultText(res))
	}
}

// TestStatusWithNeitherInstanceNorRecordIsNotFound: only when there is no
// instance in any state and no record is the answer "not found", worded so it
// is clear both were looked for.
func TestStatusWithNeitherInstanceNorRecordIsNotFound(t *testing.T) {
	fake := newFakeAWS(t)
	fake.install(t)
	res := statusOf(t, map[string]any{"instance": "ghost", "region": "us-west-2"})
	out := resultText(res)
	if !res.IsError {
		t.Fatalf("want an error result, got:\n%s", out)
	}
	if !strings.Contains(out, `no instance or task record named "ghost"`) {
		t.Errorf("not-found wording wrong:\n%s", out)
	}
	if len(fake.s3Gets) != 1 || fake.s3Gets[0] != "spawn-results-123456789012-us-west-2/tasks/ghost/completion.json" {
		t.Errorf("looked for the record at %v, want spawn's default prefix", fake.s3Gets)
	}
}
