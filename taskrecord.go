package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/spore-host/libs/pricing"
	spawnclient "github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/tasklaunch"
	"github.com/spore-host/spawn/pkg/taskproto"
	truffleaws "github.com/spore-host/truffle/pkg/aws"
)

// taskRecordStatus is spawn_status when no instance matched: it looks for the
// completion record of a task with that id, which the task's wrapper wrote to
// S3 and which outlives the instance (#63). Only when there is neither an
// instance nor a record is the answer "not found".
func taskRecordStatus(ctx context.Context, client *spawnclient.Client, taskID, region, resultsPrefix string) (*mcp.CallToolResult, error) {
	scope := "all regions"
	if region != "" {
		scope = region
	}
	recordRegion := region
	if recordRegion == "" {
		recordRegion = client.Config().Region
	}
	if recordRegion == "" {
		return mcp.NewToolResultError(fmt.Sprintf("no instance or task record named %q: no instance in %s, including terminated ones, and no region to look for a task record in (pass region)", taskID, scope)), nil
	}

	rec, uri, err := fetchTaskRecord(ctx, client, taskID, recordRegion, resultsPrefix)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("no instance named %q in %s, and its task record could not be read: %v", taskID, scope, err)), nil
	}
	if rec == nil {
		return mcp.NewToolResultError(fmt.Sprintf("no instance or task record named %q: no instance in %s, including terminated ones, and no completion record at %s", taskID, scope, uri)), nil
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "No instance named %q is listed by EC2 any more; its task record says it has finished.\n\n", taskID)
	renderTaskRecord(ctx, &sb, client, rec, uri, recordRegion, nil)
	return mcp.NewToolResultText(sb.String()), nil
}

// appendTaskRecord adds the task section to spawn_status for an instance that
// carries a spawn:task-id tag.
func appendTaskRecord(ctx context.Context, sb *strings.Builder, client *spawnclient.Client, taskID, region, resultsPrefix string, inst *spawnclient.InstanceInfo) {
	rec, uri, err := fetchTaskRecord(ctx, client, taskID, region, resultsPrefix)
	switch {
	case err != nil:
		fmt.Fprintf(sb, "Task:     %s — its record could not be read: %v\n", taskID, err)
	case rec != nil:
		renderTaskRecord(ctx, sb, client, rec, uri, region, inst)
	case isTerminalState(inst.State):
		fmt.Fprintf(sb, "Task:     %s — the instance ended without a completion record at %s\n", taskID, uri)
		sb.WriteString("          (it was stopped or killed before the wrapper or the terminal-flush hook could write one)\n")
	default:
		fmt.Fprintf(sb, "Task:     %s — still running: no completion record yet at %s\n", taskID, uri)
	}
}

// fetchTaskRecord reads a task's completion record. The location is spawn's
// own resolution, EffectiveResultsPrefix, so it is where the launch told the
// wrapper to write: s3://spawn-results-<account>-<region>/tasks/<id> unless the
// spec named a results_prefix, which the caller must then pass here too, as
// with `spawn task status --results-prefix`. A missing record is (nil, uri,
// nil); uri is always the completion record's location when it could be
// resolved.
func fetchTaskRecord(ctx context.Context, client *spawnclient.Client, taskID, region, resultsPrefix string) (*taskproto.CompletionRecord, string, error) {
	account, err := client.GetAccountID(ctx)
	if err != nil {
		return nil, "", fmt.Errorf("resolve account id: %w", err)
	}
	prefix := taskproto.EffectiveResultsPrefix(&taskproto.TaskSpec{TaskID: taskID, ResultsPrefix: resultsPrefix}, account, region)
	bucket, keyPrefix, ok := taskproto.SplitS3URI(prefix)
	if !ok {
		return nil, "", fmt.Errorf("results_prefix %q is not an s3://bucket/prefix URI", resultsPrefix)
	}
	key := tasklaunch.CompletionKey(keyPrefix)
	uri := "s3://" + bucket + "/" + key
	data, err := client.GetS3Object(ctx, region, bucket, key)
	if errors.Is(err, spawnclient.ErrS3NoSuchKey) {
		return nil, uri, nil
	}
	if err != nil {
		return nil, uri, err
	}
	rec, err := taskproto.ParseCompletionRecord(data)
	if err != nil {
		return nil, uri, err
	}
	return rec, uri, nil
}

// renderTaskRecord writes how a task ended, from its completion record. inst
// is the task's instance when EC2 still lists it, else nil; it is what makes a
// cost derivable when the record carries none.
func renderTaskRecord(ctx context.Context, sb *strings.Builder, client *spawnclient.Client, rec *taskproto.CompletionRecord, uri, region string, inst *spawnclient.InstanceInfo) {
	fmt.Fprintf(sb, "Task:     %s\n", rec.TaskID)
	fmt.Fprintf(sb, "State:    %s\n", rec.State)
	fmt.Fprintf(sb, "Exit code: %d\n", rec.ExitCode)
	fmt.Fprintf(sb, "Ended because: %s\n", taskEndReason(rec))
	if rec.RetryClass != taskproto.RetryNone {
		fmt.Fprintf(sb, "Retry class: %s (retryable: %v)\n", rec.RetryClass, rec.RetryClass.Retryable())
	}
	if rec.StartedAt != "" {
		fmt.Fprintf(sb, "Started:  %s\n", rec.StartedAt)
	}
	if rec.EndedAt != "" {
		fmt.Fprintf(sb, "Ended:    %s\n", rec.EndedAt)
	}
	wall, wallOK := recordWallTime(rec)
	if wallOK {
		fmt.Fprintf(sb, "Wall time: %s (the task's command window: from staging in to the record, not the instance's boot)\n", wall.Round(time.Second))
	}
	fmt.Fprintf(sb, "Cost:     %s\n", taskCost(ctx, client, rec, region, inst, wall, wallOK))
	if rec.RunID != "" {
		fmt.Fprintf(sb, "Run ID:   %s\n", rec.RunID)
	}
	for _, l := range rec.Logs {
		fmt.Fprintf(sb, "Log:      %s\n", l)
	}
	fmt.Fprintf(sb, "Record:   %s\n", uri)
}

// taskEndReason says why a task ended, from the record's fields: a lifecycle
// limit named by the terminal-flush hook wins, then the wrapper's state and
// retry class.
func taskEndReason(rec *taskproto.CompletionRecord) string {
	if r := rec.TerminalReason; r != "" && r != taskproto.ExitCompleted {
		cut := "a lifecycle limit ended the instance while the command was still running"
		switch r {
		case taskproto.ExitTTLExpired:
			return "its TTL expired while the command was still running (" + string(r) + ")"
		case taskproto.ExitCostLimitExceeded:
			return "its cost limit was reached while the command was still running (" + string(r) + ")"
		case taskproto.ExitIdleTimeout:
			return "it hit its idle timeout while the command was still running (" + string(r) + ")"
		case taskproto.ExitSpotInterruption:
			return "its Spot instance was interrupted while the command was still running (" + string(r) + ")"
		case taskproto.ExitUserTerminated:
			return "it was terminated by a user while the command was still running (" + string(r) + ")"
		}
		return cut + " (" + string(r) + ")"
	}
	if rec.State == taskproto.StateCompleted {
		return "the command exited 0 and its outputs were delivered"
	}
	switch rec.RetryClass {
	case taskproto.RetryAppError:
		return fmt.Sprintf("the command exited non-zero (%d)", rec.ExitCode)
	case taskproto.RetryStagingError:
		return "an input failed to stage in from S3, so the command never ran"
	case taskproto.RetryOutputDeliveryError:
		return "the command succeeded but a declared output failed to upload"
	case taskproto.RetryInstanceHealth:
		return "the instance could not run the task (Docker failed to install or start), so the command never ran"
	case taskproto.RetrySpecError:
		return "the task's spec could not be parsed on the instance, so nothing ran"
	case taskproto.RetryCapacity:
		return "no capacity was available"
	case taskproto.RetryNone:
		return fmt.Sprintf("the task %s with no recorded class", rec.State)
	}
	return string(rec.RetryClass)
}

// recordWallTime is ended_at minus started_at, when both parse.
func recordWallTime(rec *taskproto.CompletionRecord) (time.Duration, bool) {
	start, err1 := time.Parse(time.RFC3339, rec.StartedAt)
	end, err2 := time.Parse(time.RFC3339, rec.EndedAt)
	if err1 != nil || err2 != nil || end.Before(start) {
		return 0, false
	}
	return end.Sub(start), true
}

// taskCost is the cost line. Every figure is labelled an estimate and compute
// only: none of them includes EBS, S3 requests or data transfer.
func taskCost(ctx context.Context, client *spawnclient.Client, rec *taskproto.CompletionRecord, region string, inst *spawnclient.InstanceInfo, wall time.Duration, wallOK bool) string {
	const only = "estimate, compute only (excludes EBS, S3 and data transfer)"
	if rec.CostEstimate > 0 {
		return fmt.Sprintf("~$%.4f — %s, recorded by spored on the instance", rec.CostEstimate, only)
	}
	if !wallOK {
		return "not derivable: the record carries no cost_estimate and no usable start/end times"
	}
	if inst == nil || inst.InstanceType == "" {
		return "not derivable: the record carries no cost_estimate, and EC2 no longer lists the instance, so its type is unknown"
	}
	rate, source, err := onDemandRate(ctx, client, region, inst.InstanceType)
	if err != nil {
		return fmt.Sprintf("not derivable: no on-demand rate for %s in %s (%v)", inst.InstanceType, region, err)
	}
	line := fmt.Sprintf("~$%.4f — %s: %s on-demand $%.4f/hr (%s) × wall time %s",
		rate*wall.Hours(), only, inst.InstanceType, rate, source, wall.Round(time.Second))
	if inst.SpotInstance {
		line += "; it ran as Spot, so the on-demand rate overstates it"
	}
	return line
}

// onDemandRate is the hourly on-demand rate for an instance type, and where it
// came from: truffle's live Price List lookup, else libs/pricing's static
// table, labelled as static with its collection date and why it was used. A
// variable so tests price without AWS.
var onDemandRate = func(ctx context.Context, client *spawnclient.Client, region, instanceType string) (float64, string, error) {
	live, liveErr := truffleaws.NewClientFromConfig(client.Config()).OnDemandPrice(ctx, instanceType, region)
	if liveErr == nil && live > 0 {
		return live, "live AWS Price List", nil
	}
	if liveErr == nil {
		liveErr = errors.New("no price returned")
	}
	static, err := pricing.GetEC2HourlyRate(region, instanceType)
	if err != nil {
		return 0, "", fmt.Errorf("live lookup failed (%v), and %w", liveErr, err)
	}
	return static, fmt.Sprintf("STATIC rate from the libs/pricing table collected %s, used because the live Price List lookup failed: %v", pricing.PricesAsOf, liveErr), nil
}

// isTerminalState reports an EC2 state an instance does not come back from.
func isTerminalState(state string) bool {
	return state == "terminated" || state == "shutting-down"
}
