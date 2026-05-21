package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	spawnclient "github.com/spore-host/spawn/pkg/aws"
)

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
		mcp.WithDescription("Permanently terminate a spawn-managed instance. This cannot be undone."),
		mcp.WithString("instance",
			mcp.Description("Instance name or EC2 instance ID"),
			mcp.Required(),
		),
		mcp.WithString("region",
			mcp.Description("AWS region. Omit to search all regions."),
		),
	), handleSpawnTerminate)

	s.AddTool(mcp.NewTool("spawn_extend",
		mcp.WithDescription("Extend the TTL (time-to-live) of a running instance. The new TTL replaces the existing one."),
		mcp.WithString("instance",
			mcp.Description("Instance name or EC2 instance ID"),
			mcp.Required(),
		),
		mcp.WithString("ttl",
			mcp.Description("New TTL value (e.g. 2h, 24h, 7d). Replaces the current TTL."),
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
func findInstance(ctx context.Context, client *spawnclient.Client, nameOrID, region string) (*spawnclient.InstanceInfo, error) {
	instances, err := client.ListInstances(ctx, region, "")
	if err != nil {
		return nil, fmt.Errorf("list instances: %w", err)
	}
	for i := range instances {
		inst := &instances[i]
		if inst.InstanceID == nameOrID || strings.EqualFold(inst.Name, nameOrID) {
			return inst, nil
		}
	}
	return nil, fmt.Errorf("instance %q not found", nameOrID)
}

func handleSpawnList(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	region, _ := args["region"].(string)
	state, _ := args["state"].(string)
	if state == "" {
		state = "running"
	}

	client, err := spawnClient(ctx)
	if err != nil {
		return mcp.NewToolResultError("failed to connect to AWS: " + err.Error()), nil
	}

	instances, err := client.ListInstances(ctx, region, state)
	if err != nil {
		return mcp.NewToolResultError("list instances: " + err.Error()), nil
	}

	if len(instances) == 0 {
		scope := "all regions"
		if region != "" {
			scope = region
		}
		return mcp.NewToolResultText(fmt.Sprintf("No %s instances found in %s.", state, scope)), nil
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

	client, err := spawnClient(ctx)
	if err != nil {
		return mcp.NewToolResultError("failed to connect to AWS: " + err.Error()), nil
	}

	inst, err := findInstance(ctx, client, nameOrID, region)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

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
		return mcp.NewToolResultError(fmt.Sprintf("invalid TTL %q — use Go duration format, e.g. 2h, 24h, 7d is not valid (use 168h)", newTTL)), nil
	}

	client, err := spawnClient(ctx)
	if err != nil {
		return mcp.NewToolResultError("failed to connect to AWS: " + err.Error()), nil
	}

	inst, err := findInstance(ctx, client, nameOrID, region)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	if err := client.UpdateInstanceTags(ctx, inst.Region, inst.InstanceID, map[string]string{
		"spawn:ttl": newTTL,
	}); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("update TTL: %v", err)), nil
	}

	return mcp.NewToolResultText(fmt.Sprintf("✅ %s TTL updated: %s → %s", inst.Name, inst.TTL, newTTL)), nil
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
