package main

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/spore-host/libs/catalog"
	spawnclient "github.com/spore-host/spawn/pkg/aws"
)

// TestToolResultsLeadWithTheSummaryLine pins #75 through the real handlers:
// each result's first line is the summary, then a blank line, then the output
// the tool returned before #75 (checked by its opening). The truffle tools and
// lagotto's reads cannot be pointed at the fake; their lines are pinned in
// summary_test.go from values.
func TestToolResultsLeadWithTheSummaryLine(t *testing.T) {
	const taskSpec = `{"task_id":"era5-2023","command":["python","run.py"],"resources":{"cpu":8,"memory_gib":64},"lifecycle":{"ttl":"45m"}}`
	// desktop needs no image, so it plans from the embedded catalog alone; an
	// image-less application such as desktopApp plans only with a user overlay.
	desktopApp, ok := catalog.Lookup("desktop")
	if !ok {
		t.Fatal("desktop is not in the catalog")
	}
	desktopAppType := desktopApp.InstanceFamilies[0] + ".xlarge"

	// A task instance up 12m with 33m left on its TTL.
	runningTask := func(f *fakeAWS) {
		f.instances = []fakeInstance{{id: "i-0abc123", name: "era5-2023", state: "running", instanceType: "r8g.2xlarge",
			launched: time.Now().Add(-12*time.Minute - 10*time.Second),
			tags: map[string]string{"spawn:task-id": "era5-2023",
				"spawn:ttl-deadline": time.Now().Add(33*time.Minute + 10*time.Second).UTC().Format(time.RFC3339)}}}
	}
	terminatedTask := func(f *fakeAWS) {
		f.instances = []fakeInstance{{id: "i-0abc123", name: "era5", state: "terminated", instanceType: "r8g.2xlarge",
			tags: map[string]string{"spawn:task-id": "era5"}}}
	}
	record := func(key, body string) func(*fakeAWS) {
		return func(f *fakeAWS) {
			f.objects["spawn-results-123456789012-us-west-2/tasks/"+key+"/completion.json"] = body
		}
	}
	live := func(f *fakeAWS) {
		f.instances = []fakeInstance{{id: "i-live", name: "job", state: "running", instanceType: "t4g.small"}}
	}

	for _, tc := range []struct {
		name  string
		setup []func(*fakeAWS)
		tool  func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error)
		args  map[string]any
		// lead is the exact first line, or, when it starts with "^", a regexp.
		lead string
		// body is how today's output opens below the blank line; "" for a
		// result that is the summary line alone.
		body string
	}{
		// spawn_list
		{name: "spawn_list none", tool: handleSpawnList, args: map[string]any{"region": "us-west-2"},
			lead: "No running spawn-managed instances found in us-west-2."},
		{name: "spawn_list some", tool: handleSpawnList, args: map[string]any{"region": "us-west-2"},
			setup: []func(*fakeAWS){func(f *fakeAWS) {
				f.instances = []fakeInstance{
					{id: "i-1", name: "era5-2023", state: "running", instanceType: "r8g.2xlarge", launched: time.Now().Add(-12 * time.Minute)},
					{id: "i-2", name: "notebook", state: "running", instanceType: "t4g.large", launched: time.Now().Add(-3*time.Hour - 5*time.Minute)},
				}
			}},
			lead: "2 running in us-west-2: era5-2023 (r8g.2xlarge, 12m), notebook (t4g.large, 3h)",
			body: "Found 2 running instance(s):"},

		// spawn_status
		{name: "spawn_status running task", tool: handleSpawnStatus, args: map[string]any{"instance": "era5-2023", "region": "us-west-2"},
			setup: []func(*fakeAWS){runningTask},
			lead:  "era5-2023 running: i-0abc123 r8g.2xlarge, up 12m, TTL deadline in 33m, no completion record yet",
			body:  "Instance: era5-2023 (i-0abc123)"},
		{name: "spawn_status completed", tool: handleSpawnStatus, args: map[string]any{"instance": "era5", "region": "us-west-2"},
			setup: []func(*fakeAWS){terminatedTask, record("era5", finishedRecord)},
			lead:  "era5 completed (exit 0) in 18m 0s, cost ~$0.12; instance i-0abc123 terminated",
			body:  "Instance: era5 (i-0abc123)"},
		{name: "spawn_status failed, cut short", tool: handleSpawnStatus, args: map[string]any{"instance": "era5", "region": "us-west-2"},
			setup: []func(*fakeAWS){terminatedTask, record("era5", `{"task_id":"era5","exit_code":137,"state":"failed","retry_class":"ttl_expired","terminal_reason":"ttl_expired","cost_estimate":0.03,"started_at":"2026-10-20T21:00:00Z","ended_at":"2026-10-20T21:04:10Z"}`)},
			lead:  "era5 failed (exit 137) after 4m 10s, cost ~$0.03 — ended because its TTL expired; instance i-0abc123 terminated",
			body:  "Instance: era5 (i-0abc123)"},
		{name: "spawn_status record only", tool: handleSpawnStatus, args: map[string]any{"instance": "era5", "region": "us-west-2"},
			setup: []func(*fakeAWS){record("era5", recordWith(`"cost_estimate":0.16`))},
			lead:  "era5 completed (exit 0) in 18m 0s, cost ~$0.16; instance no longer listed, from task record",
			body:  `No instance named "era5" is listed by EC2 any more`},
		{name: "spawn_status record only, cost not derivable", tool: handleSpawnStatus, args: map[string]any{"instance": "era5", "region": "us-west-2"},
			setup: []func(*fakeAWS){record("era5", finishedRecord)},
			lead:  "era5 completed (exit 0) in 18m 0s, cost unknown; instance no longer listed, from task record",
			body:  `No instance named "era5" is listed by EC2 any more`},
		{name: "spawn_status ended without a record", tool: handleSpawnStatus, args: map[string]any{"instance": "era5", "region": "us-west-2"},
			setup: []func(*fakeAWS){terminatedTask},
			lead:  "era5 terminated: i-0abc123 r8g.2xlarge, ended without a completion record",
			body:  "Instance: era5 (i-0abc123)"},
		{name: "spawn_status plain instance", tool: handleSpawnStatus, args: map[string]any{"instance": "job", "region": "us-west-2"},
			setup: []func(*fakeAWS){live},
			lead:  "job running: i-live t4g.small, up 30m",
			body:  "Instance: job (i-live)"},

		// the instance-changing tools: their whole answer is the line
		{name: "spawn_stop", tool: handleSpawnStop, args: map[string]any{"instance": "job", "region": "us-west-2"},
			setup: []func(*fakeAWS){live}, lead: "✅ job (i-live) stopped successfully."},
		{name: "spawn_terminate preview", tool: handleSpawnTerminate, args: map[string]any{"instance": "job", "region": "us-west-2"},
			setup: []func(*fakeAWS){live},
			lead:  "Not terminated (preview): call again with confirm=true to terminate job (i-live, t4g.small, running in us-west-2)",
			body:  "⚠️  This will PERMANENTLY terminate:"},
		{name: "spawn_terminate confirmed", tool: handleSpawnTerminate, args: map[string]any{"instance": "job", "region": "us-west-2", "confirm": true},
			setup: []func(*fakeAWS){live}, lead: "✅ job (i-live) terminated."},
		{name: "spawn_extend", tool: handleSpawnExtend, args: map[string]any{"instance": "job", "region": "us-west-2", "ttl": "2h"},
			setup: []func(*fakeAWS){live}, lead: `^✅ job TTL extended by 2h — new deadline \d{4}-\d\d-\d\d \d\d:\d\d UTC$`},

		// spawn_task_run
		{name: "spawn_task_run dry run", tool: handleSpawnTaskRun, args: map[string]any{"spec": taskSpec, "region": "us-west-2", "dry_run": true},
			lead: "Dry run: era5-2023 would launch r8g.2xlarge On-Demand in us-west-2, TTL 45m (max ~$0.35), on-complete terminate — nothing launched",
			body: "DRY RUN — nothing will be launched."},
		{name: "spawn_task_run launched", tool: handleSpawnTaskRun, args: map[string]any{"spec": taskSpec, "region": "us-west-2"},
			lead: "✅ Launched era5-2023: i-0123456789abcdef0 r8g.2xlarge On-Demand in us-west-2b, TTL 45m (max ~$0.35), terminates on completion",
			body: "✅ Task launched: era5-2023"},

		// spawn_app_launch never launches; the line says so either way
		{name: "spawn_app_launch dry run", tool: handleSpawnAppLaunch, args: map[string]any{"app": "desktop", "ttl": "4h", "region": "us-west-2", "dry_run": true},
			lead: "Dry run: desktop (" + string(desktopApp.Kind()) + ") would launch " + desktopAppType + " in us-west-2, TTL 4h — nothing launched",
			body: "DRY RUN — nothing will be launched."},
		{name: "spawn_app_launch plan only", tool: handleSpawnAppLaunch, args: map[string]any{"app": "desktop", "ttl": "4h", "region": "us-west-2"},
			lead: "Plan only: desktop (" + string(desktopApp.Kind()) + ") on " + desktopAppType + " in us-west-2, TTL 4h — not launched; launch it with the spawn app launch CLI",
			body: "PLAN ONLY — this MCP tool does not perform the real DCV/web launch."},

		// lagotto_watch
		{name: "lagotto_watch, poller on", tool: handleLagottoWatch, args: map[string]any{"pattern": "g5.xlarge", "regions": "us-west-2", "ttl": "24h"},
			setup: []func(*fakeAWS){func(f *fakeAWS) { f.schedule = pollerSchedule("ENABLED") }},
			lead:  `^✅ Created watch w-[0-9a-f]{8}: g5\.xlarge On-Demand in us-west-2, action notify, expires in 1d 0h 0m; poller on$`,
			body:  "✅ Created watch w-"},
		{name: "lagotto_watch, no poller", tool: handleLagottoWatch, args: map[string]any{"pattern": "g5.xlarge", "spot": true, "ttl": "2h"},
			lead: `^✅ Created watch w-[0-9a-f]{8}: g5\.xlarge Spot in all enabled regions, action notify, expires in 2h 0m; no hosted poller, so nothing polls it yet$`,
			body: "✅ Created watch w-"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeAWS(t)
			for _, s := range tc.setup {
				s(f)
			}
			f.install(t)
			withTaskDeps(t, &recordingProvisioner{})
			prev := onDemandRate
			onDemandRate = func(context.Context, *spawnclient.Client, string, string) (float64, string, error) {
				return 0.4, "test rate", nil
			}
			t.Cleanup(func() { onDemandRate = prev })

			res, err := tc.tool(context.Background(), newRequest(tc.args))
			if err != nil || res == nil {
				t.Fatalf("transport error: %v %+v", err, res)
			}
			out := resultText(res)
			if res.IsError {
				t.Fatalf("tool failed:\n%s", out)
			}
			lines := strings.Split(out, "\n")
			if strings.HasPrefix(tc.lead, "^") {
				if !regexp.MustCompile(tc.lead).MatchString(lines[0]) {
					t.Errorf("first line\n  %q\ndoes not match\n  %s", lines[0], tc.lead)
				}
			} else if lines[0] != tc.lead {
				t.Errorf("first line\n got %q\nwant %q", lines[0], tc.lead)
			}
			if strings.Contains(lines[0], "123456789012") {
				t.Errorf("the summary line carries the account ID: %q", lines[0])
			}
			if tc.body == "" {
				if len(lines) != 1 {
					t.Errorf("want the summary line alone, got:\n%s", out)
				}
				return
			}
			if len(lines) < 3 || lines[1] != "" || !strings.HasPrefix(lines[2], tc.body) {
				t.Errorf("want the summary, a blank line, then output opening %q; got:\n%s", tc.body, out)
			}
		})
	}
}

// writeSpec writes content to a file in a fresh directory and returns its path.
func writeSpec(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestSpawnTaskRunSpecFileIsTheInlineSpec: spec_file's content is used
// exactly as the same JSON inline, for the plan and for the launch.
func TestSpawnTaskRunSpecFileIsTheInlineSpec(t *testing.T) {
	const spec = `{"task_id":"era5-2023","command":["python","run.py"],"resources":{"cpu":8,"memory_gib":64},"lifecycle":{"ttl":"45m"}}`
	path := writeSpec(t, "era5-2023.task.json", spec)

	for _, dryRun := range []bool{true, false} {
		inlineProv, fileProv := &recordingProvisioner{}, &recordingProvisioner{}

		withTaskDeps(t, inlineProv)
		inline := runTaskTool(t, spec, dryRun)

		withTaskDeps(t, fileProv)
		res, err := handleSpawnTaskRun(context.Background(), newRequest(map[string]any{"spec_file": path, "region": "us-west-2", "dry_run": dryRun}))
		if err != nil || res == nil || res.IsError {
			t.Fatalf("dry_run=%v: spec_file failed: %v %s", dryRun, err, resultText(res))
		}
		fromFile := resultText(res)

		// The run ID is fresh per launch; everything else must match.
		runID := regexp.MustCompile(`(?m)^Run ID: .*$`)
		if runID.ReplaceAllString(inline, "") != runID.ReplaceAllString(fromFile, "") {
			t.Errorf("dry_run=%v: spec_file result differs from the inline spec's:\n--- inline\n%s\n--- spec_file\n%s", dryRun, inline, fromFile)
		}
		if len(inlineProv.configs) != len(fileProv.configs) {
			t.Fatalf("dry_run=%v: inline launched %d, spec_file %d", dryRun, len(inlineProv.configs), len(fileProv.configs))
		}
		for i := range inlineProv.configs {
			a, b := inlineProv.configs[i], fileProv.configs[i]
			if a.InstanceType != b.InstanceType || a.Name != b.Name || a.TTL != b.TTL || a.Spot != b.Spot {
				t.Errorf("dry_run=%v: launch configs differ: inline %+v, spec_file %+v", dryRun, a, b)
			}
		}
	}
}

// TestSpawnTaskRunSpecSources covers the refusals: both or neither of spec and
// spec_file, and a spec_file that cannot be used.
func TestSpawnTaskRunSpecSources(t *testing.T) {
	const spec = `{"task_id":"t","command":["true"],"resources":{"cpu":1},"lifecycle":{"ttl":"1h"}}`
	good := writeSpec(t, "t.json", spec)
	big := writeSpec(t, "big.json", strings.Repeat(" ", maxSpecFileBytes)+spec)
	empty := writeSpec(t, "empty.json", " \n")
	noTTL := writeSpec(t, "nottl.json", `{"task_id":"t","command":["true"],"resources":{"cpu":1}}`)

	for _, tc := range []struct {
		name string
		args map[string]any
		want []string // substrings of the error
	}{
		{"neither", map[string]any{}, []string{"spec", "spec_file", "exactly one"}},
		{"blank spec, no file", map[string]any{"spec": "  "}, []string{"spec", "spec_file", "exactly one"}},
		{"both", map[string]any{"spec": spec, "spec_file": good}, []string{"spec", "spec_file", "not both"}},
		{"missing file", map[string]any{"spec_file": "no/such/spec.json"}, []string{`spec_file "no/such/spec.json" cannot be read`, "working directory"}},
		{"a directory", map[string]any{"spec_file": t.TempDir()}, []string{"is not a regular file"}},
		{"too large", map[string]any{"spec_file": big}, []string{"over the 1048576-byte (1 MiB) limit"}},
		{"empty file", map[string]any{"spec_file": empty}, []string{"is empty"}},
		// The file's content goes through the same validation as inline.
		{"file without a TTL", map[string]any{"spec_file": noTTL}, []string{"TTL required"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prov := &recordingProvisioner{}
			withTaskDeps(t, prov)
			tc.args["region"] = "us-west-2"
			res, err := handleSpawnTaskRun(context.Background(), newRequest(tc.args))
			if err != nil || res == nil {
				t.Fatalf("transport error: %v", err)
			}
			out := resultText(res)
			if !res.IsError {
				t.Fatalf("want an error, got:\n%s", out)
			}
			for _, w := range tc.want {
				if !strings.Contains(out, w) {
					t.Errorf("error %q missing %q", out, w)
				}
			}
			if len(prov.configs) != 0 {
				t.Errorf("a refused call launched %d instance(s)", len(prov.configs))
			}
		})
	}
}

// TestSpawnTaskRunSpecFileIsRelativeToTheWorkingDirectory: a relative
// spec_file is resolved against the server's working directory.
func TestSpawnTaskRunSpecFileIsRelativeToTheWorkingDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "era5"), 0o750); err != nil {
		t.Fatal(err)
	}
	spec := `{"task_id":"rel","command":["true"],"resources":{"cpu":1},"lifecycle":{"ttl":"1h"}}`
	if err := os.WriteFile(filepath.Join(dir, "era5", "rel.task.json"), []byte(spec), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	withTaskDeps(t, &recordingProvisioner{})
	res, err := handleSpawnTaskRun(context.Background(), newRequest(map[string]any{"spec_file": "era5/rel.task.json", "region": "us-west-2", "dry_run": true}))
	if err != nil || res == nil || res.IsError {
		t.Fatalf("relative spec_file failed: %v %s", err, resultText(res))
	}
	if !strings.HasPrefix(resultText(res), "Dry run: rel would launch") {
		t.Errorf("unexpected result:\n%s", resultText(res))
	}
}

// TestSpawnTaskRunSchemaOffersBothSpecSources: spec is no longer required by
// the schema, since spec_file can stand in for it; the handler enforces one.
func TestSpawnTaskRunSchemaOffersBothSpecSources(t *testing.T) {
	s := server.NewMCPServer("test", "0.0.0", server.WithToolCapabilities(true))
	registerLaunchTools(s)
	tool := s.GetTool("spawn_task_run")
	if tool == nil {
		t.Fatal("spawn_task_run not registered")
	}
	for _, p := range []string{"spec", "spec_file"} {
		if _, ok := tool.Tool.InputSchema.Properties[p]; !ok {
			t.Errorf("schema has no %s property", p)
		}
	}
	for _, r := range tool.Tool.InputSchema.Required {
		if r == "spec" || r == "spec_file" {
			t.Errorf("schema requires %s; exactly one of spec and spec_file is required, which the handler checks", r)
		}
	}
}
