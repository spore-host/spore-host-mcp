package main

import (
	"context"
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
		mcp.WithDescription("Get detailed status of a specific spawn-managed instance by name or instance ID."),
		mcp.WithString("instance",
			mcp.Description("Instance name (e.g. my-gpu) or EC2 instance ID (e.g. i-0abc123)"),
			mcp.Required(),
		),
		mcp.WithString("region",
			mcp.Description("AWS region. Omit to search all regions."),
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

func spawnClient(ctx context.Context) (*spawnclient.Client, error) {
	return spawnclient.NewClient(ctx)
}

// findInstance looks up an instance by name or ID across regions.
//
// An exact instance-ID match is unique and returned immediately. For a name
// match it collects ALL matches and refuses to guess when more than one
// instance shares the name (case-insensitive) — returning an ambiguity error
// instead of silently acting on an arbitrary one. This matters most for the
// destructive tools: an ambiguous name must never terminate the wrong box (#12).
func findInstance(ctx context.Context, client *spawnclient.Client, nameOrID, region string) (*spawnclient.InstanceInfo, error) {
	instances, err := client.ListInstances(ctx, region, "")
	if err != nil {
		return nil, fmt.Errorf("list instances: %w", err)
	}

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
		return nil, fmt.Errorf("instance %q not found", nameOrID)
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

	// Map the MCP "state" arg onto spawn's ListInstances stateFilter. spawn treats
	// an empty filter as "all non-terminated" — so "all" must become "", not the
	// literal string "all" (which is not a valid EC2 instance-state-name and would
	// match nothing, hiding the still-EBS-billing stopped instances). (#12)
	var stateFilter string
	switch state {
	case "all":
		stateFilter = ""
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
	sb.WriteString(fmt.Sprintf("Found %d %s instance(s):\n\n", len(instances), state))
	for _, inst := range instances {
		age := "unknown"
		if !inst.LaunchTime.IsZero() {
			age = formatDuration(time.Since(inst.LaunchTime))
		}
		sb.WriteString(fmt.Sprintf("• %s (%s)\n", inst.Name, inst.InstanceID))
		sb.WriteString(fmt.Sprintf("  Type: %s  State: %s  Region: %s\n", inst.InstanceType, inst.State, inst.Region))
		if inst.PublicIP != "" {
			sb.WriteString(fmt.Sprintf("  IP: %s\n", inst.PublicIP))
		}
		sb.WriteString(fmt.Sprintf("  Age: %s", age))
		if inst.TTL != "" {
			sb.WriteString(fmt.Sprintf("  TTL: %s", inst.TTL))
		}
		sb.WriteString("\n\n")
	}
	return mcp.NewToolResultText(strings.TrimRight(sb.String(), "\n")), nil
}

func handleSpawnStatus(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	nameOrID, _ := args["instance"].(string)
	region, _ := args["region"].(string)

	client, err := spawnClient(ctx)
	if err != nil {
		return mcp.NewToolResultError("failed to connect to AWS: " + err.Error()), nil
	}

	inst, err := findInstance(ctx, client, nameOrID, region)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	age := "unknown"
	if !inst.LaunchTime.IsZero() {
		age = formatDuration(time.Since(inst.LaunchTime))
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Instance: %s (%s)\n", inst.Name, inst.InstanceID))
	sb.WriteString(fmt.Sprintf("State:    %s\n", inst.State))
	sb.WriteString(fmt.Sprintf("Type:     %s\n", inst.InstanceType))
	sb.WriteString(fmt.Sprintf("Region:   %s", inst.Region))
	if inst.AvailabilityZone != "" {
		sb.WriteString(fmt.Sprintf(" (%s)", inst.AvailabilityZone))
	}
	sb.WriteString("\n")
	if inst.PublicIP != "" {
		sb.WriteString(fmt.Sprintf("Public IP: %s\n", inst.PublicIP))
	}
	if inst.PrivateIP != "" {
		sb.WriteString(fmt.Sprintf("Private IP: %s\n", inst.PrivateIP))
	}
	sb.WriteString(fmt.Sprintf("Age:      %s\n", age))
	if inst.TTL != "" {
		sb.WriteString(fmt.Sprintf("TTL:      %s\n", inst.TTL))
	}
	if inst.IdleTimeout != "" {
		sb.WriteString(fmt.Sprintf("Idle timeout: %s\n", inst.IdleTimeout))
	}
	if inst.SpotInstance {
		sb.WriteString("Spot:     yes\n")
	}

	// DNS name from tags
	if dns, ok := inst.Tags["spawn:dns-name"]; ok && dns != "" {
		sb.WriteString(fmt.Sprintf("DNS:      %s\n", dns))
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

	inst, err := findInstance(ctx, client, nameOrID, region)
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
	inst, err := findInstance(ctx, client, nameOrID, region)
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

	inst, err := findInstance(ctx, client, nameOrID, region)
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
	tags := map[string]string{"spawn:ttl": newTTL}
	var newDeadline time.Time
	if dl, ok := inst.Tags["spawn:ttl-deadline"]; ok {
		if parsed, perr := time.Parse(time.RFC3339, dl); perr == nil {
			newDeadline = parsed.Add(extendDuration)
		}
	}
	if newDeadline.IsZero() {
		// Older instance without the deadline tag — best-effort from current TTL.
		if cur, cerr := time.ParseDuration(inst.TTL); cerr == nil {
			newDeadline = time.Now().Add(cur).Add(extendDuration)
		} else {
			newDeadline = time.Now().Add(extendDuration)
		}
	}
	// Safety floor: never set a deadline earlier than the requested duration from
	// now — a past/expired existing deadline must not reap the instance the moment
	// the user asks to extend it.
	if floor := time.Now().Add(extendDuration); newDeadline.Before(floor) {
		newDeadline = floor
	}
	tags["spawn:ttl-deadline"] = newDeadline.UTC().Format(time.RFC3339)

	if err := client.UpdateInstanceTags(ctx, inst.Region, inst.InstanceID, tags); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("update TTL: %v", err)), nil
	}

	return mcp.NewToolResultText(fmt.Sprintf("✅ %s TTL extended by %s — new deadline %s",
		inst.Name, newTTL, newDeadline.UTC().Format("2006-01-02 15:04 UTC"))), nil
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
