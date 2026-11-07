package main

import (
	"context"
	"errors"
	"fmt"
	"os"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/scheduler"
	schedulertypes "github.com/aws/aws-sdk-go-v2/service/scheduler/types"
	"github.com/aws/smithy-go"
	"github.com/spore-host/lagotto/pkg/deploy"
	"github.com/spore-host/lagotto/pkg/runtimeiam"
)

// Arming the hosted poller for a new watch (#78).
//
// The hosted lagotto poller switches its own EventBridge Scheduler schedule
// OFF when there are no active watches and no pending scheduled launches. So a
// watch is only polled if whatever creates it switches the schedule back on.
// `lagotto watch` does (lagotto#231, cmd/pollschedule.go); lagotto_watch did
// not, so on an idle account its watch sat active and unpolled until its TTL.
//
// This is an equivalent of lagotto v0.67.0's cmd/pollschedule.go
// (pollSchedulePreflight, enablePollScheduleIfOff, enablePollSchedule,
// pollScheduleError). Those are unexported in lagotto's cmd package, so they
// cannot be reused; the behaviour, sequencing and refusal text follow them.
// handleLagottoWatch runs the three steps in lagotto's order:
//
//  1. pollSchedulePreflight, BEFORE the watch is stored. ResourceNotFound means
//     the hosted poller is not deployed: the watch is stored (for `lagotto poll
//     --daemon`) and the result says so. Any other error, AccessDenied
//     included, means we cannot know the watch will be polled: refuse, store
//     nothing.
//  2. PutWatch. Storing first and enabling second means a poller run that
//     starts after the enable always sees the watch; the other order lets a
//     run that starts in between find no watch and switch the schedule
//     straight back off.
//  3. enablePollScheduleIfOff. If it fails, the watch is rolled back (stored
//     again as cancelled, through the same PutWatch call) and the call refused.

// pollScheduleAPI is the slice of EventBridge Scheduler lagotto_watch uses.
type pollScheduleAPI interface {
	GetSchedule(ctx context.Context, in *scheduler.GetScheduleInput, optFns ...func(*scheduler.Options)) (*scheduler.GetScheduleOutput, error)
	UpdateSchedule(ctx context.Context, in *scheduler.UpdateScheduleInput, optFns ...func(*scheduler.Options)) (*scheduler.UpdateScheduleOutput, error)
}

// pollScheduleName is the hosted poller's schedule, overridable with
// LAGOTTO_SCHEDULE_NAME exactly as the lagotto CLI allows.
func pollScheduleName() string {
	if n := os.Getenv("LAGOTTO_SCHEDULE_NAME"); n != "" {
		return n
	}
	return deploy.PollerScheduleName
}

// pollSchedulePreflight reads the poller's schedule. (nil, nil) means it is not
// deployed. Any other error is the refusal to return. action names what is
// refused ("create watch w-1").
func pollSchedulePreflight(ctx context.Context, sched pollScheduleAPI, action string) (*scheduler.GetScheduleOutput, error) {
	name := pollScheduleName()
	cur, err := sched.GetSchedule(ctx, &scheduler.GetScheduleInput{Name: awssdk.String(name)})
	if err != nil {
		var notFound *schedulertypes.ResourceNotFoundException
		if errors.As(err, &notFound) {
			return nil, nil
		}
		return nil, pollScheduleError(action, "nothing was changed", "scheduler:GetSchedule", name, err)
	}
	return cur, nil
}

// enablePollScheduleIfOff switches a DISABLED schedule on and reports whether it
// did. A nil (not deployed) or already-enabled schedule is left alone.
func enablePollScheduleIfOff(ctx context.Context, sched pollScheduleAPI, cur *scheduler.GetScheduleOutput) (bool, error) {
	if cur == nil || cur.State != schedulertypes.ScheduleStateDisabled {
		return false, nil
	}
	// UpdateSchedule is a full PUT, so every field the schedule carries is
	// round-tripped (an omitted optional field would be dropped).
	_, err := sched.UpdateSchedule(ctx, &scheduler.UpdateScheduleInput{
		Name:                       cur.Name,
		Description:                cur.Description,
		ScheduleExpression:         cur.ScheduleExpression,
		ScheduleExpressionTimezone: cur.ScheduleExpressionTimezone,
		FlexibleTimeWindow:         cur.FlexibleTimeWindow,
		Target:                     cur.Target,
		GroupName:                  cur.GroupName,
		KmsKeyArn:                  cur.KmsKeyArn,
		StartDate:                  cur.StartDate,
		EndDate:                    cur.EndDate,
		ActionAfterCompletion:      cur.ActionAfterCompletion,
		State:                      schedulertypes.ScheduleStateEnabled,
	})
	if err != nil {
		return false, err
	}
	return true, nil
}

// pollScheduleUpdateRefusal is the refusal after enablePollScheduleIfOff
// failed. rollbackErr is the rollback's own failure, if any.
func pollScheduleUpdateRefusal(action string, rollbackErr, err error) error {
	state := "no watch was created"
	if rollbackErr != nil {
		state = fmt.Sprintf("the watch could NOT be rolled back (%v): it is active but will not be polled — cancel it with 'lagotto cancel'", rollbackErr)
	}
	return pollScheduleError(action, state, "scheduler:UpdateSchedule", pollScheduleName(), err)
}

// pollScheduleError is the refusal: what failed, what state it left, and, for
// AccessDenied, exactly which permissions the caller is missing.
func pollScheduleError(action, state, call, name string, err error) error {
	var apiErr smithy.APIError
	denied := errors.As(err, &apiErr) && (apiErr.ErrorCode() == "AccessDeniedException" || apiErr.ErrorCode() == "AccessDenied")
	if !denied {
		return fmt.Errorf("cannot %s: %s on the hosted poller's schedule %s failed, so lagotto cannot make sure the poller will poll it (%s): %w",
			action, call, name, state, err)
	}
	return fmt.Errorf(`cannot %s: %s on the hosted poller's schedule %s was denied (%s): %w

The hosted poller switches its schedule off when no watches are active, so
arming a watch must read the schedule and switch it back on. Without that the
watch would be stored as active and never polled. Your identity needs:
  - scheduler:GetSchedule and scheduler:UpdateSchedule on schedule %s
  - iam:PassRole on role %s (UpdateSchedule passes the role the schedule assumes)
These are the permissions 'lagotto watch' needs (see lagotto docs/reference/cli-iam-actions.md)`,
		action, call, name, state, err, name, runtimeiam.SchedulerInvokeRoleName)
}
