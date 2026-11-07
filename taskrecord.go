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

	// spawn's one reader of completion records (spawn#1260): the key it
	// builds cannot drift from the one the wrapper writes.
	rec, uri, err := tasklaunch.ReadCompletion(ctx, client, taskID, resultsPrefix, recordRegion)
	if errors.Is(err, tasklaunch.ErrNoCompletionRecord) {
		return mcp.NewToolResultError(fmt.Sprintf("no instance or task record named %q: no instance in %s, including terminated ones, and no completion record at %s", taskID, scope, uri)), nil
	}
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("no instance named %q in %s, and its task record could not be read: %v", taskID, scope, err)), nil
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "No instance named %q is listed by EC2 any more; its task record says it has finished.\n\n", taskID)
	out := renderTaskRecord(ctx, &sb, client, rec, uri, recordRegion, nil)
	return withSummary(taskSummary(rec, out, "instance no longer listed, from task record"), sb.String()), nil
}

// appendTaskRecord adds the task section to spawn_status for an instance that
// carries a spawn:task-id tag, and returns spawn_status's summary line (#75).
func appendTaskRecord(ctx context.Context, sb *strings.Builder, client *spawnclient.Client, taskID, region, resultsPrefix string, inst *spawnclient.InstanceInfo) string {
	rec, uri, err := tasklaunch.ReadCompletion(ctx, client, taskID, resultsPrefix, region)
	switch {
	case err == nil:
		out := renderTaskRecord(ctx, sb, client, rec, uri, region, inst)
		return taskSummary(rec, out, fmt.Sprintf("instance %s %s", inst.InstanceID, inst.State))
	case !errors.Is(err, tasklaunch.ErrNoCompletionRecord):
		fmt.Fprintf(sb, "Task:     %s — its record could not be read: %v\n", taskID, err)
		return instanceSummary(inst, taskID, "task record unreadable", time.Now())
	case isTerminalState(inst.State):
		fmt.Fprintf(sb, "Task:     %s — the instance ended without a completion record at %s\n", taskID, uri)
		sb.WriteString("          (it was stopped or killed before the wrapper or the terminal-flush hook could write one)\n")
		return instanceSummary(inst, taskID, "ended without a completion record", time.Now())
	default:
		fmt.Fprintf(sb, "Task:     %s — still running: no completion record yet at %s\n", taskID, uri)
		return instanceSummary(inst, taskID, "no completion record yet", time.Now())
	}
}

// taskOutcome is what renderTaskRecord derived from a record that the
// summary line needs too: the wall time and the cost.
type taskOutcome struct {
	wall      time.Duration
	wallOK    bool
	cost      float64
	costKnown bool
}

// taskSummary is spawn_status's summary line (#75) for a task with a
// completion record: its state, exit code, wall time and cost, why it ended
// when that is not the plain outcome, then where the instance is.
func taskSummary(rec *taskproto.CompletionRecord, out taskOutcome, instance string) string {
	line := fmt.Sprintf("%s %s (exit %d)", rec.TaskID, rec.State, rec.ExitCode)
	if out.wallOK {
		if rec.State == taskproto.StateCompleted {
			line += " in " + clockDuration(out.wall)
		} else {
			line += " after " + clockDuration(out.wall)
		}
	}
	if out.costKnown {
		line += ", cost ~" + money(out.cost)
	} else {
		line += ", cost unknown"
	}
	if why := shortEndReason(rec); why != "" {
		line += " — ended because " + why
	}
	return line + "; " + instance
}

// shortEndReason is taskEndReason cut to a clause for the summary line, or ""
// when the state and exit code already say it: a completed task, or a
// command that exited non-zero.
func shortEndReason(rec *taskproto.CompletionRecord) string {
	if r := rec.TerminalReason; r != "" && r != taskproto.ExitCompleted {
		switch r {
		case taskproto.ExitTTLExpired:
			return "its TTL expired"
		case taskproto.ExitCostLimitExceeded:
			return "its cost limit was reached"
		case taskproto.ExitIdleTimeout:
			return "it hit its idle timeout"
		case taskproto.ExitSpotInterruption:
			return "its Spot instance was interrupted"
		case taskproto.ExitUserTerminated:
			return "a user terminated it"
		}
		return "a lifecycle limit ended it (" + string(r) + ")"
	}
	if rec.State == taskproto.StateCompleted {
		return ""
	}
	switch rec.RetryClass {
	case taskproto.RetryStagingError:
		return "an input failed to stage in"
	case taskproto.RetryOutputDeliveryError:
		return "an output failed to upload"
	case taskproto.RetryInstanceHealth:
		return "the instance could not run it"
	case taskproto.RetrySpecError:
		return "its spec could not be parsed"
	case taskproto.RetryCapacity:
		return "no capacity was available"
	}
	return ""
}

// renderTaskRecord writes how a task ended, from its completion record. inst
// is the task's instance when EC2 still lists it, else nil; it, or the
// record's own instance_type, is what makes a cost derivable when the record
// carries none.
func renderTaskRecord(ctx context.Context, sb *strings.Builder, client *spawnclient.Client, rec *taskproto.CompletionRecord, uri, region string, inst *spawnclient.InstanceInfo) taskOutcome {
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
	costText, cost, costKnown := taskCost(ctx, client, rec, region, inst, wall, wallOK)
	fmt.Fprintf(sb, "Cost:     %s\n", costText)
	if rec.RunID != "" {
		fmt.Fprintf(sb, "Run ID:   %s\n", rec.RunID)
	}
	for _, l := range rec.Logs {
		fmt.Fprintf(sb, "Log:      %s\n", l)
	}
	fmt.Fprintf(sb, "Record:   %s\n", uri)
	return taskOutcome{wall: wall, wallOK: wallOK, cost: cost, costKnown: costKnown}
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
//
// The instance type comes from EC2 while it still lists the instance, else
// from the record's instance_type and purchase, which spawn's wrapper writes
// from IMDS (spawn#1260) so a record stays priceable after EC2 forgets the
// instance. A record from an older wrapper carries neither, and without an
// instance it is not derivable.
func taskCost(ctx context.Context, client *spawnclient.Client, rec *taskproto.CompletionRecord, region string, inst *spawnclient.InstanceInfo, wall time.Duration, wallOK bool) (line string, amount float64, known bool) {
	const only = "estimate, compute only (excludes EBS, S3 and data transfer)"
	if rec.CostEstimate > 0 {
		return fmt.Sprintf("~$%.4f — %s, recorded by spored on the instance", rec.CostEstimate, only), rec.CostEstimate, true
	}
	if !wallOK {
		return "not derivable: the record carries no cost_estimate and no usable start/end times", 0, false
	}
	if inst != nil && inst.InstanceType != "" {
		return onDemandCost(ctx, client, region, inst.InstanceType, wall, only, inst.SpotInstance)
	}
	if rec.InstanceType == "" {
		return "not derivable: the record carries no cost_estimate, and EC2 no longer lists the instance, so its type is unknown", 0, false
	}
	switch rec.Purchase {
	case taskproto.PurchaseSpot:
		rate, source, err := spotRate(ctx, client, region, rec.InstanceType)
		if err != nil {
			line, amount, known := onDemandCost(ctx, client, region, rec.InstanceType, wall, only, true)
			return line + fmt.Sprintf(" (no Spot rate: %v)", err), amount, known
		}
		return fmt.Sprintf("~$%.4f — %s: %s Spot $%.4f/hr (%s) × wall time %s; type and purchase from the task record",
			rate*wall.Hours(), only, rec.InstanceType, rate, source, wall.Round(time.Second)), rate * wall.Hours(), true
	case taskproto.PurchaseOnDemand:
		line, amount, known := onDemandCost(ctx, client, region, rec.InstanceType, wall, only, false)
		return line + "; type and purchase from the task record", amount, known
	}
	line, amount, known = onDemandCost(ctx, client, region, rec.InstanceType, wall, only, false)
	return line + "; type from the task record, which names no purchase option, so if it ran as Spot this overstates it", amount, known
}

// onDemandCost prices wall time at instanceType's on-demand rate. spot says
// the instance ran as Spot, which the line then says the rate overstates.
func onDemandCost(ctx context.Context, client *spawnclient.Client, region, instanceType string, wall time.Duration, only string, spot bool) (string, float64, bool) {
	rate, source, err := onDemandRate(ctx, client, region, instanceType)
	if err != nil {
		return fmt.Sprintf("not derivable: no on-demand rate for %s in %s (%v)", instanceType, region, err), 0, false
	}
	line := fmt.Sprintf("~$%.4f — %s: %s on-demand $%.4f/hr (%s) × wall time %s",
		rate*wall.Hours(), only, instanceType, rate, source, wall.Round(time.Second))
	if spot {
		line += "; it ran as Spot, so the on-demand rate overstates it"
	}
	return line, rate * wall.Hours(), true
}

// spotRate is the hourly Spot rate for an instance type in a region, and where
// it came from. The record names no AZ and Spot prices move, so it is the
// highest current price across the region's AZs, labelled as such: an upper
// estimate of what the task paid. There is no static Spot table to fall back
// to; on error the caller prices at on-demand and says so. A variable so tests
// price without AWS.
var spotRate = func(ctx context.Context, client *spawnclient.Client, region, instanceType string) (float64, string, error) {
	prices, err := truffleaws.NewClientFromConfig(client.Config()).GetSpotPricing(ctx,
		[]truffleaws.InstanceTypeResult{{InstanceType: instanceType, Region: region}}, truffleaws.SpotOptions{})
	if err != nil {
		return 0, "", err
	}
	var high float64
	for _, p := range prices {
		if p.InstanceType == instanceType && p.SpotPrice > high {
			high = p.SpotPrice
		}
	}
	if high <= 0 {
		return 0, "", fmt.Errorf("no Spot price history for %s in %s", instanceType, region)
	}
	return high, "live, the highest current Spot price across the region's AZs: the record names no AZ, and this is today's price, not the run's", nil
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
