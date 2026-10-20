package main

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	spawnclient "github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/launcher"
	"github.com/spore-host/spawn/pkg/tasklaunch"
	"github.com/spore-host/spawn/pkg/taskproto"
)

// fakeTaskAWS is tasklaunch.AWS without AWS: it records the buckets the launch
// asked to create.
type fakeTaskAWS struct {
	account        string
	createdBuckets []string
}

func (f *fakeTaskAWS) GetAccountID(context.Context) (string, error) { return f.account, nil }
func (f *fakeTaskAWS) CreateS3BucketIfNotExists(_ context.Context, bucket, _ string) error {
	f.createdBuckets = append(f.createdBuckets, bucket)
	return nil
}
func (f *fakeTaskAWS) DeleteS3Object(context.Context, string, string, string) error { return nil }
func (f *fakeTaskAWS) CreateOrGetInstanceProfile(context.Context, spawnclient.IAMRoleConfig) (string, error) {
	return "spawn-task-test-profile", nil
}
func (f *fakeTaskAWS) GetFSxFilesystem(context.Context, string, string) (*spawnclient.FSxInfo, error) {
	return nil, errors.New("fakeTaskAWS: no FSx")
}
func (f *fakeTaskAWS) InstanceMemoryMiB(context.Context, string, string) (int64, bool) {
	return 0, false
}

// recordingProvisioner stands in for launcher.Provision. It records every
// attempt's config and Options and, when spotCapacityErr is set, fails every
// Spot attempt the way EC2 does when a Spot pool is empty.
type recordingProvisioner struct {
	spotCapacityErr bool
	configs         []spawnclient.LaunchConfig
	opts            []launcher.Options
}

func (r *recordingProvisioner) Provision(_ context.Context, cfg spawnclient.LaunchConfig, opts launcher.Options) (*spawnclient.LaunchResult, error) {
	r.configs = append(r.configs, cfg)
	r.opts = append(r.opts, opts)
	if cfg.Spot && r.spotCapacityErr {
		return nil, &spawnclient.LaunchError{Code: "InsufficientInstanceCapacity"}
	}
	return &spawnclient.LaunchResult{InstanceID: "i-0123456789abcdef0", Region: cfg.Region, AvailabilityZone: cfg.Region + "b"}, nil
}

// taskFinder is an AWS-free taskproto.InstanceFinder. It includes a 0.5 GiB
// type that is the cheapest of all, the box a container task used to land on.
type taskFinder struct{}

func (taskFinder) FindCandidates(context.Context, taskproto.ResourceRequest) ([]taskproto.Candidate, error) {
	return []taskproto.Candidate{
		{InstanceType: "t4g.nano", Family: "t4g", VCPUs: 2, MemoryGiB: 0.5, Architecture: "arm64", OnDemandPrice: 0.0042},
		{InstanceType: "t4g.small", Family: "t4g", VCPUs: 2, MemoryGiB: 2, Architecture: "arm64", OnDemandPrice: 0.0168},
		{InstanceType: "r8g.2xlarge", Family: "r8g", VCPUs: 8, MemoryGiB: 64, Architecture: "arm64", OnDemandPrice: 0.47128},
	}, nil
}

// withTaskDeps makes spawn_task_run use fakes for the rest of the test, and
// returns them.
func withTaskDeps(t *testing.T, prov *recordingProvisioner) *fakeTaskAWS {
	t.Helper()
	fake := &fakeTaskAWS{account: "123456789012"}
	prev := newTaskDeps
	newTaskDeps = func(_ context.Context, region string, notes io.Writer) (tasklaunch.Deps, string, error) {
		if region == "" {
			region = "us-west-2"
		}
		return tasklaunch.Deps{Finder: taskFinder{}, AWS: fake, Provisioner: prov, Out: notes, Warn: notes}, region, nil
	}
	t.Cleanup(func() { newTaskDeps = prev })
	return fake
}

func runTaskTool(t *testing.T, spec string, dryRun bool) string {
	t.Helper()
	res, err := handleSpawnTaskRun(context.Background(), newRequest(map[string]any{"spec": spec, "region": "us-west-2", "dry_run": dryRun}))
	if err != nil {
		t.Fatalf("transport error: %v", err)
	}
	if res == nil || res.IsError {
		t.Fatalf("spawn_task_run failed: %s", resultText(res))
	}
	return resultText(res)
}

// TestTaskRunFallsBackToOnDemandWithTheSameOptions is #61. The spec asks for
// Spot with an On-Demand fallback; the Spot attempt fails for capacity. The
// tool must retry On-Demand, and both attempts must carry the same, non-empty
// launcher.Options: the terminal-flush hook that writes a completion record
// on a TTL kill rides in them (spawn#1144). spawn_task_run used to call
// launcher.Provision once with launcher.Options{}.
func TestTaskRunFallsBackToOnDemandWithTheSameOptions(t *testing.T) {
	prov := &recordingProvisioner{spotCapacityErr: true}
	withTaskDeps(t, prov)

	out := runTaskTool(t, `{"task_id":"kiro-demo","command":["run.sh"],
		"resources":{"instance_type":"r8g.2xlarge","purchase":"spot","fallback":"on_demand"},
		"lifecycle":{"ttl":"45m","on_complete":"terminate"}}`, false)

	if len(prov.configs) != 2 {
		t.Fatalf("Provision was called %d time(s), want 2: a Spot attempt, then the On-Demand fallback the spec asks for", len(prov.configs))
	}
	if !prov.configs[0].Spot || prov.configs[1].Spot {
		t.Errorf("attempts Spot = %v, %v; want true, false", prov.configs[0].Spot, prov.configs[1].Spot)
	}
	for i, o := range prov.opts {
		if o.TaskFlushScript == "" {
			t.Errorf("attempt %d launched with no terminal-flush hook (launcher.Options %+v): a TTL kill would leave no completion record (spawn#1144)", i+1, o)
		}
	}
	if !reflect.DeepEqual(prov.opts[0], prov.opts[1]) {
		t.Errorf("the On-Demand retry got different launcher.Options from the Spot attempt:\n spot:      %+v\n on-demand: %+v", prov.opts[0], prov.opts[1])
	}
	if !strings.Contains(out, "On-Demand") || strings.Contains(out, "Spot)") {
		t.Errorf("the tool must report the purchase option that launched (On-Demand), got:\n%s", out)
	}
	if !strings.Contains(out, "retrying on-demand") {
		t.Errorf("the tool must pass on spawn's fallback notice, got:\n%s", out)
	}
}

// TestTaskRunReportsTheCompletionPathTheInstanceWrites is #64. The Completion
// line must be the resolved results prefix tasklaunch gave the wrapper, so that
// is what the test compares it with: the RESULTS_PREFIX the generated wrapper
// actually writes to. spawn_task_run used to hand the wrapper a bare bucket
// name, so `aws s3 cp` wrote the record to the instance's local disk while the
// tool reported an S3 path nothing ever wrote.
func TestTaskRunReportsTheCompletionPathTheInstanceWrites(t *testing.T) {
	for _, tc := range []struct {
		name, resultsPrefix, wantPrefix string
		wantCreated                     []string
	}{{
		name:        "default prefix",
		wantPrefix:  "s3://spawn-results-123456789012-us-west-2/tasks/era5",
		wantCreated: []string{"spawn-results-123456789012-us-west-2"},
	}, {
		name:          "the spec's results_prefix",
		resultsPrefix: "s3://lab-results/runs/",
		wantPrefix:    "s3://lab-results/runs/era5",
		wantCreated:   nil, // never create a bucket the caller named
	}} {
		t.Run(tc.name, func(t *testing.T) {
			prov := &recordingProvisioner{}
			fake := withTaskDeps(t, prov)

			spec := `{"task_id":"era5","command":["run.sh"],"resources":{"cpu":2},"lifecycle":{"ttl":"1h"}`
			if tc.resultsPrefix != "" {
				spec += `,"results_prefix":` + strconv.Quote(tc.resultsPrefix)
			}
			out := runTaskTool(t, spec+`}`, false)

			if len(prov.configs) != 1 {
				t.Fatalf("Provision called %d times, want 1", len(prov.configs))
			}
			wrapper := prov.configs[0].JobArrayCommand
			wrote := "RESULTS_PREFIX='" + tc.wantPrefix + "'"
			if !strings.Contains(wrapper, wrote) {
				t.Fatalf("the wrapper does not write under %s (want the line %s)", tc.wantPrefix, wrote)
			}
			want := "Completion: " + tc.wantPrefix + "/completion.json\n"
			if !strings.Contains(out, want) {
				t.Errorf("the tool reports a completion path the instance does not write.\nwant %q in:\n%s", want, out)
			}
			if !reflect.DeepEqual(fake.createdBuckets, tc.wantCreated) {
				t.Errorf("buckets created = %v, want %v (only spawn's own results bucket may be created)", fake.createdBuckets, tc.wantCreated)
			}
		})
	}
}

// TestTaskRunSizesAContainerTaskToTheMemoryFloor is #60: a container task
// that asks only for CPU must not size onto a 0.5 GiB box, where the boot-time
// Docker install is OOM-killed (spawn#1180). Checked on both paths, since both
// used to size with spec.Resources.
func TestTaskRunSizesAContainerTaskToTheMemoryFloor(t *testing.T) {
	spec := `{"task_id":"ctr","command":["true"],"container":"public.ecr.aws/docker/library/alpine:3.20",
		"resources":{"cpu":2},"lifecycle":{"ttl":"30m"}}`

	prov := &recordingProvisioner{}
	withTaskDeps(t, prov)
	runTaskTool(t, spec, false)
	if got := prov.configs[0].InstanceType; got != "t4g.small" {
		t.Errorf("launched a container task on %s, want t4g.small (the cheapest type at the %.0f GiB container floor)", got, taskproto.ContainerMemoryFloorGiB)
	}

	plan := runTaskTool(t, spec, true)
	if !strings.Contains(plan, "t4g.small") || strings.Contains(plan, "t4g.nano") {
		t.Errorf("the dry run must size to the container floor too, got:\n%s", plan)
	}
}

// TestTaskRunDryRunLaunchesNothing: the dry run is spawn's own plan, and it
// provisions nothing.
func TestTaskRunDryRunLaunchesNothing(t *testing.T) {
	prov := &recordingProvisioner{}
	fake := withTaskDeps(t, prov)
	out := runTaskTool(t, `{"task_id":"plan1","command":["run.sh"],"resources":{"cpu":2,"memory_gib":3},"lifecycle":{"ttl":"2h"}}`, true)

	if len(prov.configs) != 0 || len(fake.createdBuckets) != 0 {
		t.Fatalf("a dry run provisioned %d instance(s) and created buckets %v", len(prov.configs), fake.createdBuckets)
	}
	for _, want := range []string{"DRY RUN", "plan1", "r8g.2xlarge", "TTL:", "2h", "dry_run=false"} {
		if !strings.Contains(out, want) {
			t.Errorf("plan missing %q; got:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Task launched") {
		t.Errorf("a dry run must not claim a launch: %q", out)
	}
}

// TestTaskLaunchGoesThroughTasklaunch keeps spawn_task_run on spawn's launch
// path. Each of #60, #61 and #64 was this server sizing, building the wrapper
// or provisioning by itself and drifting from `spawn task run`: Size with
// spec.Resources skipped the container floor, Provision with Options{} lost the
// flush hook and the On-Demand fallback, and a hand-built WrapperOptions wrote
// the record to local disk. So no non-test file in this package may refer to
// those functions at all; tasklaunch is the one place that does.
//
// It resolves each file's imports, so an aliased import is still caught, and
// matches a reference, not only a call, so `p := launcher.Provision` is too.
func TestTaskLaunchGoesThroughTasklaunch(t *testing.T) {
	forbidden := map[string]map[string]bool{
		"github.com/spore-host/spawn/pkg/launcher":  {"Provision": true},
		"github.com/spore-host/spawn/pkg/taskproto": {"Size": true, "GenerateWrapper": true, "GenerateFlushScript": true},
	}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	parsed := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		parsed++
		imports := map[string]string{} // local name -> path
		for _, imp := range f.Imports {
			path, _ := strconv.Unquote(imp.Path.Value)
			local := path[strings.LastIndex(path, "/")+1:]
			if imp.Name != nil {
				local = imp.Name.Name
			}
			imports[local] = path
		}
		ast.Inspect(f, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok {
				return true
			}
			if forbidden[imports[pkg.Name]][sel.Sel.Name] {
				t.Errorf("%s: %s.%s — spawn_task_run must size, build and provision through spawn's pkg/tasklaunch, not by itself (#60, #61, #64)",
					fset.Position(sel.Pos()), pkg.Name, sel.Sel.Name)
			}
			return true
		})
	}
	if parsed == 0 {
		t.Fatal("parsed no non-test Go files; the discovery is broken, not the package clean")
	}
}
