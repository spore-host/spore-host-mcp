package main

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	spawnclient "github.com/spore-host/spawn/pkg/aws"
)

// TestCompletionRecordsAreReadOnlyThroughSpawnsReader gates spawn#1260's
// point: spawn has one reader of completion records,
// tasklaunch.ReadCompletion, so the key it reads cannot drift from the key
// the wrapper writes again (spawn#715, four releases of "running" for tasks
// that had finished). MCP had its own copy of that chain. This fails if any
// non-test file here parses a record, builds a completion key or reads an S3
// object itself, and if nothing calls ReadCompletion at all.
//
// It matches calls, resolved through each file's imports, not names: a
// comment saying which function not to call does not trip it.
func TestCompletionRecordsAreReadOnlyThroughSpawnsReader(t *testing.T) {
	const (
		tasklaunchPath = "github.com/spore-host/spawn/pkg/tasklaunch"
		taskprotoPath  = "github.com/spore-host/spawn/pkg/taskproto"
	)
	forbidden := map[string]string{
		taskprotoPath + ".ParseCompletionRecord": "parses a completion record itself",
		tasklaunchPath + ".CompletionKey":        "builds a completion key itself",
	}
	files, err := filepath.Glob(filepath.Join(repoRoot(), "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var parsed, readers int
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		parsed++
		imports := map[string]string{} // local name -> import path
		for _, im := range f.Imports {
			p, _ := strconv.Unquote(im.Path.Value)
			name := p[strings.LastIndex(p, "/")+1:]
			if im.Name != nil {
				name = im.Name.Name
			}
			imports[name] = p
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			// A read of an S3 object by any receiver: the only object this
			// server reads is a completion record, and spawn reads it.
			if sel.Sel.Name == "GetS3Object" || sel.Sel.Name == "GetObject" {
				t.Errorf("%s: calls %s; completion records are read through tasklaunch.ReadCompletion, not by hand",
					fset.Position(call.Pos()), sel.Sel.Name)
			}
			x, ok := sel.X.(*ast.Ident)
			if !ok {
				return true
			}
			qualified := imports[x.Name] + "." + sel.Sel.Name
			if why, bad := forbidden[qualified]; bad {
				t.Errorf("%s: %s: %s; call tasklaunch.ReadCompletion, spawn's one reader (spawn#1260)",
					fset.Position(call.Pos()), qualified, why)
			}
			if qualified == tasklaunchPath+".ReadCompletion" {
				readers++
			}
			return true
		})
	}
	if parsed == 0 {
		t.Fatal("found no non-test Go files: the discovery is broken, not the tree clean")
	}
	if readers == 0 {
		t.Error("nothing calls tasklaunch.ReadCompletion, so spawn_status reads no completion records through spawn's reader")
	}
}

// stubRates replaces both live price lookups for one test. A nil func fails
// the test if that lookup is made.
func stubRates(t *testing.T, onDemand, spot func(region, instanceType string) (float64, string, error)) {
	t.Helper()
	prevOD, prevSpot := onDemandRate, spotRate
	never := func(kind string) func(string, string) (float64, string, error) {
		return func(region, it string) (float64, string, error) {
			t.Errorf("looked up the %s rate for %q in %s; this case must not", kind, it, region)
			return 0, "", errors.New("no rate in this test")
		}
	}
	if onDemand == nil {
		onDemand = never("on-demand")
	}
	if spot == nil {
		spot = never("Spot")
	}
	onDemandRate = func(_ context.Context, _ *spawnclient.Client, r, it string) (float64, string, error) {
		return onDemand(r, it)
	}
	spotRate = func(_ context.Context, _ *spawnclient.Client, r, it string) (float64, string, error) {
		return spot(r, it)
	}
	t.Cleanup(func() { onDemandRate, spotRate = prevOD, prevSpot })
}

// recordWith is finishedRecord (18 minutes of wall time, no cost_estimate)
// with extra JSON fields.
func recordWith(fields string) string {
	return strings.Replace(finishedRecord, `"exit_code":0,`, `"exit_code":0,`+fields+`,`, 1)
}

// TestStatusPricesAGoneSpotTaskFromItsRecord: once EC2 stops listing a
// terminated instance, a record that names its instance_type and
// purchase=spot (spawn#1260) is priced at the Spot rate times the wall time,
// still labelled an estimate of compute only. Before, it was "not derivable".
func TestStatusPricesAGoneSpotTaskFromItsRecord(t *testing.T) {
	fake := newFakeAWS(t)
	fake.objects["spawn-results-123456789012-us-west-2/tasks/era5/completion.json"] =
		recordWith(`"instance_type":"r8g.2xlarge","purchase":"spot"`)
	fake.install(t)
	stubRates(t,
		func(string, string) (float64, string, error) { return 0.4, "test on-demand rate", nil },
		func(region, it string) (float64, string, error) {
			if region != "us-west-2" || it != "r8g.2xlarge" {
				t.Errorf("Spot-priced %s in %s", it, region)
			}
			return 0.12, "test Spot rate", nil
		})

	out := resultText(statusOf(t, map[string]any{"instance": "era5", "region": "us-west-2"}))
	for _, want := range []string{
		// 0.12 $/hr × 18 min = $0.036
		"Cost:     ~$0.0360 — estimate, compute only (excludes EBS, S3 and data transfer)",
		"r8g.2xlarge Spot $0.1200/hr (test Spot rate) × wall time 18m0s",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("status missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "not derivable") || strings.Contains(out, "on-demand $") {
		t.Errorf("a Spot task named by its record was not priced at the Spot rate:\n%s", out)
	}
}

// TestStatusPricesAGoneTaskFromItsRecordOtherwise covers the record's other
// purchase values: on_demand is the on-demand rate, and a Spot task whose
// Spot rate cannot be found falls back to on-demand and says it overstates.
func TestStatusPricesAGoneTaskFromItsRecordOtherwise(t *testing.T) {
	od := func(string, string) (float64, string, error) { return 0.4, "test on-demand rate", nil }
	for _, tc := range []struct {
		name, fields string
		spot         func(string, string) (float64, string, error)
		want         []string
	}{
		{"on_demand", `"instance_type":"r8g.2xlarge","purchase":"on_demand"`, nil, []string{
			"Cost:     ~$0.1200 — estimate, compute only",
			"r8g.2xlarge on-demand $0.4000/hr (test on-demand rate) × wall time 18m0s",
		}},
		{"spot without a Spot rate", `"instance_type":"r8g.2xlarge","purchase":"spot"`,
			func(string, string) (float64, string, error) { return 0, "", errors.New("throttled") },
			[]string{
				"Cost:     ~$0.1200 — estimate, compute only",
				"it ran as Spot, so the on-demand rate overstates it",
				"no Spot rate: throttled",
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFakeAWS(t)
			fake.objects["spawn-results-123456789012-us-west-2/tasks/era5/completion.json"] = recordWith(tc.fields)
			fake.install(t)
			stubRates(t, od, tc.spot)
			out := resultText(statusOf(t, map[string]any{"instance": "era5", "region": "us-west-2"}))
			for _, want := range tc.want {
				if !strings.Contains(out, want) {
					t.Errorf("status missing %q:\n%s", want, out)
				}
			}
		})
	}
}

// TestStatusOfAnOldRecordWithoutTheInstanceIsUnchanged: a record written by a
// wrapper older than spawn#1260 carries no instance_type or purchase. With no
// instance listed it reports what it did before: the record's cost_estimate
// when it has one, else "not derivable", and it looks up no rate.
func TestStatusOfAnOldRecordWithoutTheInstanceIsUnchanged(t *testing.T) {
	for _, tc := range []struct{ name, record, want string }{
		{"no cost_estimate", finishedRecord,
			"Cost:     not derivable: the record carries no cost_estimate, and EC2 no longer lists the instance, so its type is unknown"},
		{"cost_estimate", recordWith(`"cost_estimate":0.25`),
			"Cost:     ~$0.2500 — estimate, compute only (excludes EBS, S3 and data transfer), recorded by spored on the instance"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFakeAWS(t)
			fake.objects["spawn-results-123456789012-us-west-2/tasks/era5/completion.json"] = tc.record
			fake.install(t)
			stubRates(t, nil, nil)
			out := resultText(statusOf(t, map[string]any{"instance": "era5", "region": "us-west-2"}))
			if !strings.Contains(out, tc.want) {
				t.Errorf("status missing %q:\n%s", tc.want, out)
			}
		})
	}
}
