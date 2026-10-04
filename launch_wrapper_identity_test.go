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
