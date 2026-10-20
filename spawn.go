package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	spawnclient "github.com/spore-host/spawn/pkg/aws"
)

// auditLog records a mutating operation to stderr (stdout is reserved for the
// MCP protocol). Mirrors the CLI, which audit-logs destructive actions (#12).
func auditLog(op, name, instanceID, region string) {
	log.Printf("AUDIT op=%s instance=%s id=%s region=%s", op, name, instanceID, region)
}

func registerSpawnTools(s *server.MCPServer) {
	s.AddTool(mcp.NewTool("spawn_list",
		mcp.WithDescription("List spawn-managed EC2 instances. Returns instance name, type, state, region, IP, launch time, and TTL."),
		mcp.WithString("region",
			mcp.Description("AWS region to search (e.g. us-east-1). Omit to search all regions."),
		),
		mcp.WithString("state",
			mcp.Description("Filter by state: running, stopped, all"),
			mcp.DefaultString("running"),
		),
	), handleSpawnList)

	s.AddTool(mcp.NewTool("spawn_status",
		mcp.WithDescription("Get detailed status of a specific spawn-managed instance or task by name, instance ID or task ID. Finds instances in every state, terminated included (EC2 lists a terminated instance for about an hour). For a task, it also reads the task's completion record from S3 and reports how the task ended — state, exit code, why it ended, start/end and wall time, and an estimated compute cost — so a finished task is reported even after its instance is gone."),
		mcp.WithString("instance",
			mcp.Description("Instance name (e.g. my-gpu), EC2 instance ID (e.g. i-0abc123), or a spawn_task_run task_id"),
			mcp.Required(),
		),
		mcp.WithString("region",
			mcp.Description("AWS region. Omit to search all regions for the instance; the task record is then looked up in the configured default region."),
		),
		mcp.WithString("results_prefix",
			mcp.Description("Only for a task launched with a TaskSpec results_prefix: the same s3:// prefix, so its completion record can be found. Omit for spawn's default location."),
		),
	), handleSpawnStatus)

	s.AddTool(mcp.NewTool("spawn_stop",
		mcp.WithDescription("Stop a running spawn-managed instance (instance is preserved, can be restarted)."),
		mcp.WithString("instance",
			mcp.Description("Instance name or EC2 instance ID"),
			mcp.Required(),
		),
		mcp.WithString("region",
			mcp.Description("AWS region. Omit to search all regions."),
		),
		mcp.WithBoolean("hibernate",
			mcp.Description("Hibernate instead of stopping (saves RAM state to disk, faster resume). Requires hibernation-enabled instance."),
			mcp.DefaultBool(false),
		),
	), handleSpawnStop)

	s.AddTool(mcp.NewTool("spawn_terminate",
		mcp.WithDescription("Permanently terminate a spawn-managed instance. This is irreversible and destroys the instance. Two-phase: call once to preview the exact instance that would be terminated, then call again with confirm=true to actually terminate it."),
		mcp.WithString("instance",
			mcp.Description("Instance name or EC2 instance ID. An ambiguous name (matching more than one instance) is rejected — use the instance ID."),
			mcp.Required(),
		),
		mcp.WithString("region",
			mcp.Description("AWS region. Omit to search all regions."),
		),
		mcp.WithBoolean("confirm",
			mcp.Description("Must be true to actually terminate. When false or omitted, returns a preview of the instance that would be terminated without destroying it."),
			mcp.DefaultBool(false),
		),
	), handleSpawnTerminate)

	s.AddTool(mcp.NewTool("spawn_extend",
		mcp.WithDescription("Extend the TTL (time-to-live) of a running instance. The new TTL replaces the existing one."),
		mcp.WithString("instance",
			mcp.Description("Instance name or EC2 instance ID"),
			mcp.Required(),
		),
		mcp.WithString("ttl",
			mcp.Description("New TTL value in Go duration units — h/m/s only (e.g. 2h, 24h, 168h for a week). Day/week suffixes like 7d are not supported. Replaces the current TTL."),
			mcp.Required(),
		),
		mcp.WithString("region",
			mcp.Description("AWS region. Omit to search all regions."),
		),
	), handleSpawnExtend)
}

// spawnClient returns the spawn AWS client every spawn tool uses. A variable so
// tests can point the real handlers at a fake AWS; nothing else assigns it.
var spawnClient = func(ctx context.Context) (*spawnclient.Client, error) {
	return spawnclient.NewClient(ctx)
}

// The two instance sets a tool resolves a name against, as spawn's
// ListInstances stateFilter values.
const (
	// activeInstances is spawn's default: pending, running, stopping and
	// stopped, never terminated. The tools that change an instance (spawn_stop,
	// spawn_terminate, spawn_extend) resolve against this, so an old terminated
	// instance with the same name cannot turn a valid name into an ambiguity
	// error, or be acted on.
	activeInstances = ""
	// everyInstance includes shutting-down and terminated (spawn#527). Only
	// read-only tools use it: spawn_status, so a task that has just finished
	// still resolves, and spawn_list state=all.
	everyInstance = "all"
)

// errInstanceNotFound is what findInstance returns, wrapped, when nothing
// matches, so spawn_status can tell "not found" from an ambiguity or an AWS
// error and fall back to the task's completion record.
var errInstanceNotFound = errors.New("not found")

// findInstance looks up an instance by name or ID across regions, among the
// instances in states (activeInstances or everyInstance).
//
// An exact instance-ID match is unique and returned immediately. For a name
// match it collects ALL matches and refuses to guess when more than one
// instance shares the name (case-insensitive) — returning an ambiguity error
// instead of silently acting on an arbitrary one. This matters most for the
// destructive tools: an ambiguous name must never terminate the wrong box (#12).
func findInstance(ctx context.Context, client *spawnclient.Client, nameOrID, region, states string) (*spawnclient.InstanceInfo, error) {
	instances, err := client.ListInstances(ctx, region, states)
	if err != nil {
		return nil, fmt.Errorf("list instances: %w", err)
	}
	return selectInstance(instances, nameOrID)
}

// selectInstance resolves a name-or-ID against a list of instances. It is the
// pure, AWS-free core of findInstance so the ambiguity guarantee can be unit
// tested. An exact instance-ID match is unique and wins immediately; a name match
// collects ALL matches and refuses to guess when more than one instance shares the
// name — returning an ambiguity error rather than acting on an arbitrary one. This
// matters most for the destructive tools (#12).
func selectInstance(instances []spawnclient.InstanceInfo, nameOrID string) (*spawnclient.InstanceInfo, error) {
	var nameMatches []*spawnclient.InstanceInfo
	for i := range instances {
		inst := &instances[i]
		if inst.InstanceID == nameOrID {
			return inst, nil // instance IDs are unique — exact match wins
		}
		if strings.EqualFold(inst.Name, nameOrID) {
			nameMatches = append(nameMatches, inst)
		}
	}

	switch len(nameMatches) {
	case 0:
		return nil, fmt.Errorf("instance %q %w", nameOrID, errInstanceNotFound)
	case 1:
		return nameMatches[0], nil
	default:
		var ids []string
		for _, m := range nameMatches {
			ids = append(ids, fmt.Sprintf("%s (%s, %s)", m.InstanceID, m.Region, m.State))
		}
		return nil, fmt.Errorf("name %q is ambiguous — %d instances match: %s. Re-run with the specific instance ID",
			nameOrID, len(nameMatches), strings.Join(ids, "; "))
	}
}

func handleSpawnList(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	region, _ := args["region"].(string)
	state, _ := args["state"].(string)
	if state == "" {
		state = "running"
	}

	// Map the MCP "state" arg onto spawn's ListInstances stateFilter. "all" is
	// spawn's "all": every state, shutting-down and terminated included. spawn's
	// "" is NOT all — since spawn#527 it means non-terminated only, so mapping
	// "all" to "" (as this did until #63) made a finished task vanish from
	// state=all the moment its instance terminated.
	var stateFilter string
	switch state {
	case "all":
		stateFilter = everyInstance
	case "running", "stopped":
		stateFilter = state
	default:
		return mcp.NewToolResultError(fmt.Sprintf("invalid state %q — use one of: running, stopped, all", state)), nil
	}

	client, err := spawnClient(ctx)
	if err != nil {
		return mcp.NewToolResultError("failed to connect to AWS: " + err.Error()), nil
	}

	instances, err := client.ListInstances(ctx, region, stateFilter)
	if err != nil {
		return mcp.NewToolResultError("list instances: " + err.Error()), nil
	}

	if len(instances) == 0 {
		scope := "all regions"
		if region != "" {
			scope = region
		}
		stateDesc := state + " "
		if state == "all" {
			stateDesc = ""
		}
		return mcp.NewToolResultText(fmt.Sprintf("No %sspawn-managed instances found in %s.", stateDesc, scope)), nil
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "Found %d %s instance(s):\n\n", len(instances), state)
	for _, inst := range instances {
		age := "unknown"
		if !inst.LaunchTime.IsZero() {
			age = formatDuration(time.Since(inst.LaunchTime))
		}
		fmt.Fprintf(&sb, "• %s (%s)\n", inst.Name, inst.InstanceID)
		fmt.Fprintf(&sb, "  Type: %s  State: %s  Region: %s\n", inst.InstanceType, inst.State, inst.Region)
		if inst.PublicIP != "" {
			fmt.Fprintf(&sb, "  IP: %s\n", inst.PublicIP)
		}
		fmt.Fprintf(&sb, "  Age: %s", age)
		if inst.TTL != "" {
			fmt.Fprintf(&sb, "  TTL: %s", inst.TTL)
		}
		sb.WriteString("\n\n")
	}
	return mcp.NewToolResultText(strings.TrimRight(sb.String(), "\n")), nil
}

func handleSpawnStatus(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	nameOrID, _ := args["instance"].(string)
	region, _ := args["region"].(string)
	resultsPrefix, _ := args["results_prefix"].(string)
	if strings.TrimSpace(nameOrID) == "" {
		return mcp.NewToolResultError("instance is required (a name, instance ID or task ID)"), nil
	}

	client, err := spawnClient(ctx)
	if err != nil {
		return mcp.NewToolResultError("failed to connect to AWS: " + err.Error()), nil
	}

	// Every state, so a task whose instance has just terminated still resolves
	// (#63). Read-only, so the ambiguity this can add is only reported.
	inst, err := findInstance(ctx, client, nameOrID, region, everyInstance)
	if errors.Is(err, errInstanceNotFound) {
		// No instance: the task may have finished and its instance aged out of
		// EC2. Its completion record outlives it.
		return taskRecordStatus(ctx, client, nameOrID, region, resultsPrefix)
	}
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	age := "unknown"
	if !inst.LaunchTime.IsZero() {
		age = formatDuration(time.Since(inst.LaunchTime))
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "Instance: %s (%s)\n", inst.Name, inst.InstanceID)
	fmt.Fprintf(&sb, "State:    %s\n", inst.State)
	fmt.Fprintf(&sb, "Type:     %s\n", inst.InstanceType)
	fmt.Fprintf(&sb, "Region:   %s", inst.Region)
	if inst.AvailabilityZone != "" {
		fmt.Fprintf(&sb, " (%s)", inst.AvailabilityZone)
	}
	sb.WriteString("\n")
	if inst.PublicIP != "" {
		fmt.Fprintf(&sb, "Public IP: %s\n", inst.PublicIP)
	}
	if inst.PrivateIP != "" {
		fmt.Fprintf(&sb, "Private IP: %s\n", inst.PrivateIP)
	}
	fmt.Fprintf(&sb, "Age:      %s\n", age)
	if inst.TTL != "" {
		fmt.Fprintf(&sb, "TTL:      %s\n", inst.TTL)
	}
	// spored reaps on the absolute spawn:ttl-deadline, not the relative TTL — so
	// surface the actual death time (and how far off it is) rather than leaving the
	// reader to guess. This is the value spawn_extend pushes forward.
	if dl, ok := inst.Tags["spawn:ttl-deadline"]; ok && dl != "" {
		if deadline, perr := time.Parse(time.RFC3339, dl); perr == nil {
			remaining := time.Until(deadline)
			when := deadline.UTC().Format("2006-01-02 15:04 UTC")
			if remaining > 0 {
				fmt.Fprintf(&sb, "Deadline: %s (in %s)\n", when, formatDuration(remaining))
			} else {
				fmt.Fprintf(&sb, "Deadline: %s (expired)\n", when)
			}
		} else {
			fmt.Fprintf(&sb, "Deadline: %s\n", dl)
		}
	}
	if inst.IdleTimeout != "" {
		fmt.Fprintf(&sb, "Idle timeout: %s\n", inst.IdleTimeout)
	}
	if inst.SpotInstance {
		sb.WriteString("Spot:     yes\n")
	}

	// DNS name from tags
	if dns, ok := inst.Tags["spawn:dns-name"]; ok && dns != "" {
		fmt.Fprintf(&sb, "DNS:      %s\n", dns)
	}
	if inst.StateReason != "" && isTerminalState(inst.State) {
		fmt.Fprintf(&sb, "Reason:   %s\n", inst.StateReason)
	}

	// A task instance: report its completion record too, which is how the task
	// ended rather than how the instance did.
	if taskID := inst.Tags["spawn:task-id"]; taskID != "" {
		sb.WriteString("\n")
		appendTaskRecord(ctx, &sb, client, taskID, inst.Region, resultsPrefix, inst)
	}

	return mcp.NewToolResultText(sb.String()), nil
}

func handleSpawnStop(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	nameOrID, _ := args["instance"].(string)
	region, _ := args["region"].(string)
	hibernate, _ := args["hibernate"].(bool)

	client, err := spawnClient(ctx)
	if err != nil {
		return mcp.NewToolResultError("failed to connect to AWS: " + err.Error()), nil
	}

	inst, err := findInstance(ctx, client, nameOrID, region, activeInstances)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if inst.State != "running" {
		return mcp.NewToolResultError(fmt.Sprintf("instance %s is %s, not running", inst.Name, inst.State)), nil
	}

	if err := client.StopInstance(ctx, inst.Region, inst.InstanceID, hibernate); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("stop failed: %v", err)), nil
	}

	verb := "stopped"
	if hibernate {
		verb = "hibernated"
	}
	return mcp.NewToolResultText(fmt.Sprintf("✅ %s (%s) %s successfully.", inst.Name, inst.InstanceID, verb)), nil
}

func handleSpawnTerminate(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	nameOrID, _ := args["instance"].(string)
	region, _ := args["region"].(string)
	confirm, _ := args["confirm"].(bool)

	client, err := spawnClient(ctx)
	if err != nil {
		return mcp.NewToolResultError("failed to connect to AWS: " + err.Error()), nil
	}

	// findInstance refuses ambiguous names, so we never terminate an arbitrary
	// instance that happens to share a name (#12).
	inst, err := findInstance(ctx, client, nameOrID, region, activeInstances)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	// Two-phase: without confirm=true, preview the exact instance and stop.
	// spawn_terminate is directly LLM-callable and irreversible, so we never
	// destroy an instance on the first call.
	if !confirm {
		return mcp.NewToolResultText(fmt.Sprintf(
			"⚠️  This will PERMANENTLY terminate:\n"+
				"  %s (%s)\n  type: %s  state: %s  region: %s\n\n"+
				"This is irreversible. To proceed, call spawn_terminate again with confirm=true.",
			inst.Name, inst.InstanceID, inst.InstanceType, inst.State, inst.Region)), nil
	}

	// Audit every mutating operation (to stderr — stdout is the MCP protocol
	// channel). The CLI audit-logs terminations; the MCP path must too (#12).
	auditLog("spawn_terminate", inst.Name, inst.InstanceID, inst.Region)

	if err := client.Terminate(ctx, inst.Region, inst.InstanceID); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("terminate failed: %v", err)), nil
	}

	return mcp.NewToolResultText(fmt.Sprintf("✅ %s (%s) terminated.", inst.Name, inst.InstanceID)), nil
}

func handleSpawnExtend(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	nameOrID, _ := args["instance"].(string)
	newTTL, _ := args["ttl"].(string)
	region, _ := args["region"].(string)

	if _, err := time.ParseDuration(newTTL); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("invalid TTL %q — use Go duration units (h/m/s), e.g. 2h, 24h, or 168h for a week (day/week suffixes like 7d are not supported)", newTTL)), nil
	}

	client, err := spawnClient(ctx)
	if err != nil {
		return mcp.NewToolResultError("failed to connect to AWS: " + err.Error()), nil
	}

	inst, err := findInstance(ctx, client, nameOrID, region, activeInstances)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	// spored treats spawn:ttl-deadline (absolute) as authoritative and ignores
	// spawn:ttl for current-vintage instances — so writing only spawn:ttl is a
	// silent no-op: the instance still dies at its original deadline. Mirror the
	// CLI (cmd/extend.go): push the absolute deadline forward (anchored to launch)
	// and write BOTH tags. (#11)
	extendDuration, err := time.ParseDuration(newTTL)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("invalid TTL %q: %v", newTTL, err)), nil
	}
	newDeadline := computeExtendedDeadline(inst.Tags["spawn:ttl-deadline"], inst.TTL, extendDuration, time.Now())
	tags := map[string]string{
		"spawn:ttl":          newTTL,
		"spawn:ttl-deadline": newDeadline.UTC().Format(time.RFC3339),
	}

	if err := client.UpdateInstanceTags(ctx, inst.Region, inst.InstanceID, tags); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("update TTL: %v", err)), nil
	}

	return mcp.NewToolResultText(fmt.Sprintf("✅ %s TTL extended by %s — new deadline %s",
		inst.Name, newTTL, newDeadline.UTC().Format("2006-01-02 15:04 UTC"))), nil
}

// computeExtendedDeadline returns the new absolute TTL deadline for an extend.
//
// spored treats spawn:ttl-deadline (absolute) as authoritative and ignores
// spawn:ttl for current-vintage instances, so an extend MUST push the deadline
// forward. Preference order:
//   - a parseable existing deadline is pushed forward by extendDuration (anchored
//     to launch, matching the spawn extend CLI);
//   - otherwise (older instance, or unparseable) best-effort from the current TTL;
//   - otherwise now+extendDuration.
//
// A safety floor guarantees the result is never earlier than now+extendDuration —
// an already-expired existing deadline must not reap the instance the moment the
// user asks to extend it (spore-host#374). now is a parameter for testability.
func computeExtendedDeadline(existingDeadline, currentTTL string, extendDuration time.Duration, now time.Time) time.Time {
	var newDeadline time.Time
	if parsed, perr := time.Parse(time.RFC3339, existingDeadline); perr == nil {
		newDeadline = parsed.Add(extendDuration)
	} else if cur, cerr := time.ParseDuration(currentTTL); cerr == nil {
		newDeadline = now.Add(cur).Add(extendDuration)
	} else {
		newDeadline = now.Add(extendDuration)
	}
	if floor := now.Add(extendDuration); newDeadline.Before(floor) {
		newDeadline = floor
	}
	return newDeadline
}

func formatDuration(d time.Duration) string {
	d = d.Round(time.Minute)
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	if h >= 24 {
		days := h / 24
		h = h % 24
		return fmt.Sprintf("%dd %dh %dm", days, h, m)
	}
	if h > 0 {
		return fmt.Sprintf("%dh %dm", h, m)
	}
	return fmt.Sprintf("%dm", m)
}
