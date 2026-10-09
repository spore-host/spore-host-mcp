package main

import (
	"testing"

	"github.com/spore-host/spawn/pkg/taskproto"
)

// TestWrapperIdentityMintsARunID guards the failure that made this fix worth
// doing carefully rather than minimally.
//
// spawn v0.111.1 added `gpu bool` and v0.112.0 added `runID string` to
// taskproto.GenerateWrapper (see spawn#679 — the first was a breaking change in
// a PATCH release). Both are positional, so the quickest way to make this
// package compile again was `GenerateWrapper(spec, bucket, region, false, "")`.
//
// That compiles, passes every test, and emits an empty run_id — which reads
// downstream as "unattributable" and reintroduces spawn#608 in the MCP path
// only: re-running a task_id overwrites the same completion.json key, so a
// record from a previous attempt becomes indistinguishable from this one's and a
// waiter reports an already-fixed task as still failing.
func TestWrapperIdentityMintsARunID(t *testing.T) {
	spec := &taskproto.TaskSpec{TaskID: "t1"}

	_, first := wrapperIdentity(spec, "m7i.large")
	if first == "" {
		t.Fatal("runID is empty. An empty run_id is accepted by the wrapper and reads as " +
			"'unattributable' downstream, so a waiter cannot tell THIS attempt's completion " +
			"record from a previous attempt's at the same S3 key (spawn#608).")
	}

	// Per ATTEMPT, not per task: two launches of the same spec must differ, or
	// the id cannot distinguish attempts, which is its whole purpose.
	_, second := wrapperIdentity(spec, "m7i.large")
	if first == second {
		t.Errorf("two calls produced the same runID (%s); the id identifies an attempt, so "+
			"re-running the same task must not reuse it", first)
	}
}

// TestWrapperIdentityDetectsGPUFromTheSizedType is the other positional
// argument. `gpu=false` on a GPU instance silently drops `--gpus all` from the
// container run, so the workload sees no GPU on hardware that has one — the
// spawn#606 symptom, which only real hardware caught there.
func TestWrapperIdentityDetectsGPUFromTheSizedType(t *testing.T) {
	tests := []struct {
		name         string
		specGPUs     int
		instanceType string
		wantGPU      bool
	}{{
		name:         "GPU instance with no GPU in the spec",
		instanceType: "g6.xlarge",
		wantGPU:      true, // the point: sizing, not the spec, decides
	}, {
		name:         "spec asks for GPUs even on a type we do not recognise",
		specGPUs:     1,
		instanceType: "some-future-type",
		wantGPU:      true,
	}, {
		name:         "plain CPU instance",
		instanceType: "m7i.large",
		wantGPU:      false,
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			spec := &taskproto.TaskSpec{}
			spec.Resources.GPUs = tc.specGPUs

			gpu, _ := wrapperIdentity(spec, tc.instanceType)
			if gpu != tc.wantGPU {
				t.Errorf("gpu = %v, want %v for spec.GPUs=%d on %q.\n\n"+
					"A task may ask for a GPU only via `families` and still be sized onto a "+
					"GPU box, so this must follow the SIZED type (spawn#601/#606).",
					gpu, tc.wantGPU, tc.specGPUs, tc.instanceType)
			}
		})
	}
}

// TestWrapperOptionsFromWrapperIdentityAreValid guards OUR side of the contract
// that spawn#764 introduced.
//
// GenerateWrapper now returns an error when ResultsPrefix or RunID is empty, and
// launch.go surfaces that as a tool error. That branch is defensive — it cannot
// be reached today, because wrapperIdentity always mints a run id and the results
// bucket is resolved before the call — and an unreachable branch is exactly the
// kind of thing that quietly becomes reachable.
//
// So rather than contort the code to cover the error return, this asserts the
// inputs are valid for the shapes launch.go actually builds. If someone later
// makes the results bucket optional, or returns an empty id from
// wrapperIdentity, this fails here instead of at a user's launch.
func TestWrapperOptionsFromWrapperIdentityAreValid(t *testing.T) {
	for _, tc := range []struct {
		name         string
		instanceType string
		bucket       string
		wantGPU      bool
	}{
		{"CPU instance", "m7i.large", "spawn-results-123456789012-us-east-1", false},
		{"GPU instance", "g6.xlarge", "spawn-results-123456789012-us-east-1", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec := &taskproto.TaskSpec{TaskID: "t1", Command: []string{"true"}}

			// The function the handler actually calls, not a lookalike rebuilt
			// here — so this cannot pass while the real call site is wrong.
			opts := wrapperOptionsFor(spec, tc.instanceType, tc.bucket, "us-east-1")

			if opts.GPU != tc.wantGPU {
				t.Errorf("GPU = %v, want %v on %q", opts.GPU, tc.wantGPU, tc.instanceType)
			}
			if err := opts.Validate(); err != nil {
				t.Fatalf("the options launch.go builds are invalid: %v", err)
			}
			if _, err := taskproto.GenerateWrapper(spec, opts); err != nil {
				t.Fatalf("GenerateWrapper rejected them: %v", err)
			}
		})
	}
}

// TestGenerateWrapperRejectsAnEmptyRunID pins the reason the error return exists
// at all: an empty run id used to be accepted and emitted an unattributable
// record (spawn#608). Asserted here, in the consumer, because this repo is where
// that bug actually shipped.
func TestGenerateWrapperRejectsAnEmptyRunID(t *testing.T) {
	spec := &taskproto.TaskSpec{TaskID: "t1", Command: []string{"true"}}
	_, err := taskproto.GenerateWrapper(spec, taskproto.WrapperOptions{
		ResultsPrefix: "spawn-results-123456789012-us-east-1",
		Region:        "us-east-1",
		RunID:         "", // the padding edit that caused spawn#679
	})
	if err == nil {
		t.Error("GenerateWrapper accepted an empty RunID. That is the spawn#608 " +
			"regression this signature exists to prevent: the wrapper would emit an " +
			"empty run_id, and a waiter could not tell this attempt's completion " +
			"record from a previous attempt's at the same S3 key.")
	}
}
