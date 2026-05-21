package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	truffleaws "github.com/spore-host/truffle/pkg/aws"
	"github.com/spore-host/truffle/pkg/find"
	"github.com/spore-host/truffle/pkg/quotas"
)

func registerTruffleTools(s *server.MCPServer) {
	s.AddTool(mcp.NewTool("truffle_find",
		mcp.WithDescription("Find EC2 instance types matching a natural language description. Returns matching types with vCPUs, memory, GPU specs, and on-demand pricing. Examples: 'nvidia h100 8gpu', 'cheap arm64 with 32gb memory', 'inference gpu us-east-1'."),
		mcp.WithString("query",
			mcp.Description("Natural language description of the instance you need (e.g. 'nvidia a100 40gb', '16 vcpu 64gb memory', 'cheapest spot gpu')"),
			mcp.Required(),
		),
		mcp.WithString("regions",
			mcp.Description("Comma-separated AWS regions to search (e.g. us-east-1,eu-west-1). Defaults to us-east-1."),
		),
		mcp.WithBoolean("spot_prices",
			mcp.Description("Include current Spot prices alongside on-demand prices."),
			mcp.DefaultBool(false),
		),
	), handleTruffleFind)

	s.AddTool(mcp.NewTool("truffle_spot_prices",
		mcp.WithDescription("Get current Spot instance prices for a specific instance type across regions and availability zones."),
		mcp.WithString("instance_type",
			mcp.Description("EC2 instance type (e.g. p4d.24xlarge, g5.2xlarge)"),
			mcp.Required(),
		),
		mcp.WithString("regions",
			mcp.Description("Comma-separated AWS regions (e.g. us-east-1,us-west-2). Defaults to us-east-1."),
		),
	), handleTruffleSpotPrices)

	s.AddTool(mcp.NewTool("truffle_quota_check",
		mcp.WithDescription("Check whether your AWS account has sufficient quota to launch an instance type in a region. Returns current quota, usage, and whether the launch would be allowed."),
		mcp.WithString("instance_type",
			mcp.Description("EC2 instance type to check (e.g. p4d.24xlarge)"),
			mcp.Required(),
		),
		mcp.WithString("region",
			mcp.Description("AWS region (e.g. us-east-1)"),
			mcp.Required(),
		),
		mcp.WithBoolean("spot",
			mcp.Description("Check Spot quota instead of On-Demand quota"),
			mcp.DefaultBool(false),
		),
	), handleTruffleQuotaCheck)
}

func parseRegions(s string) []string {
	if s == "" {
		return []string{"us-east-1"}
	}
	var regions []string
	for _, r := range strings.Split(s, ",") {
		if r = strings.TrimSpace(r); r != "" {
			regions = append(regions, r)
		}
	}
	return regions
}

func handleTruffleFind(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	query, _ := args["query"].(string)
	regionsStr, _ := args["regions"].(string)
	withSpot, _ := args["spot_prices"].(bool)

	pq, err := find.ParseQuery(query)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("parse query: %v", err)), nil
	}

	criteria, err := pq.BuildCriteria()
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("build criteria: %v", err)), nil
	}

	regions := parseRegions(regionsStr)

	client, err := truffleaws.NewClient(ctx)
	if err != nil {
		return mcp.NewToolResultError("failed to connect to AWS: " + err.Error()), nil
	}

	results, err := client.SearchInstanceTypes(ctx, regions, criteria.InstanceTypePattern, criteria.FilterOptions)
	if err != nil {
		return mcp.NewToolResultError("search failed: " + err.Error()), nil
	}

	if len(results) == 0 {
		return mcp.NewToolResultText(fmt.Sprintf("No instance types found matching %q in %s.", query, strings.Join(regions, ", "))), nil
	}

	// Optionally fetch spot prices
	var spotPrices []truffleaws.SpotPriceResult
	if withSpot && len(results) > 0 {
		spotPrices, _ = client.GetSpotPricing(ctx, results, truffleaws.SpotOptions{ShowSavings: true})
	}
	spotByType := make(map[string]truffleaws.SpotPriceResult)
	for _, sp := range spotPrices {
		spotByType[sp.InstanceType] = sp
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Found %d instance type(s) matching %q:\n\n", len(results), query))

	for _, r := range results {
		sb.WriteString(fmt.Sprintf("**%s** (%s)\n", r.InstanceType, r.Region))
		sb.WriteString(fmt.Sprintf("  vCPUs: %d  Memory: %.1f GiB  Arch: %s\n",
			r.VCPUs, float64(r.MemoryMiB)/1024.0, r.Architecture))
		if r.GPUs > 0 {
			sb.WriteString(fmt.Sprintf("  GPUs: %d× %s (%s, %.0f GB VRAM)\n",
				r.GPUs, r.GPUModel, r.GPUManufacturer, float64(r.GPUMemoryMiB)/1024.0))
		}
		if r.OnDemandPrice > 0 {
			sb.WriteString(fmt.Sprintf("  On-Demand: $%.4f/hr\n", r.OnDemandPrice))
		}
		if sp, ok := spotByType[r.InstanceType]; ok {
			sb.WriteString(fmt.Sprintf("  Spot: $%.4f/hr (%.0f%% savings, %s)\n",
				sp.SpotPrice, sp.SavingsPercent, sp.AvailabilityZone))
		}
		if len(r.AvailableAZs) > 0 {
			sb.WriteString(fmt.Sprintf("  Available AZs: %s\n", strings.Join(r.AvailableAZs, ", ")))
		}
		sb.WriteString("\n")
	}

	return mcp.NewToolResultText(strings.TrimRight(sb.String(), "\n")), nil
}

func handleTruffleSpotPrices(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	instanceType, _ := args["instance_type"].(string)
	regionsStr, _ := args["regions"].(string)

	regions := parseRegions(regionsStr)

	client, err := truffleaws.NewClient(ctx)
	if err != nil {
		return mcp.NewToolResultError("failed to connect to AWS: " + err.Error()), nil
	}

	// Search for this exact instance type first to get metadata
	results, err := client.SearchInstanceTypes(ctx, regions, nil, truffleaws.FilterOptions{})
	if err != nil {
		return mcp.NewToolResultError("search failed: " + err.Error()), nil
	}

	// Filter to requested type
	var filtered []truffleaws.InstanceTypeResult
	for _, r := range results {
		if strings.EqualFold(r.InstanceType, instanceType) {
			filtered = append(filtered, r)
		}
	}
	if len(filtered) == 0 {
		return mcp.NewToolResultText(fmt.Sprintf("Instance type %s not found in %s.", instanceType, strings.Join(regions, ", "))), nil
	}

	spotPrices, err := client.GetSpotPricing(ctx, filtered, truffleaws.SpotOptions{ShowSavings: true})
	if err != nil {
		return mcp.NewToolResultError("spot price lookup failed: " + err.Error()), nil
	}
	if len(spotPrices) == 0 {
		return mcp.NewToolResultText(fmt.Sprintf("No Spot price data available for %s.", instanceType)), nil
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Spot prices for %s:\n\n", instanceType))
	for _, sp := range spotPrices {
		sb.WriteString(fmt.Sprintf("  %s (%s): $%.4f/hr", sp.Region, sp.AvailabilityZone, sp.SpotPrice))
		if sp.OnDemandPrice > 0 {
			sb.WriteString(fmt.Sprintf("  [on-demand: $%.4f/hr, %.0f%% savings]", sp.OnDemandPrice, sp.SavingsPercent))
		}
		sb.WriteString("\n")
	}
	return mcp.NewToolResultText(sb.String()), nil
}

func handleTruffleQuotaCheck(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	instanceType, _ := args["instance_type"].(string)
	region, _ := args["region"].(string)
	isSpot, _ := args["spot"].(bool)

	client, err := quotas.NewClient(ctx)
	if err != nil {
		return mcp.NewToolResultError("failed to connect to AWS: " + err.Error()), nil
	}

	info, err := client.GetQuotas(ctx, region)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("get quotas: %v", err)), nil
	}

	family := quotas.GetQuotaFamily(instanceType)

	// Look up vCPUs for this instance type
	trClient, err := truffleaws.NewClient(ctx)
	if err != nil {
		return mcp.NewToolResultError("failed to connect to AWS: " + err.Error()), nil
	}
	results, err := trClient.SearchInstanceTypes(ctx, []string{region}, nil, truffleaws.FilterOptions{})
	if err != nil {
		return mcp.NewToolResultError("instance type lookup failed: " + err.Error()), nil
	}
	var vcpus int32
	for _, r := range results {
		if strings.EqualFold(r.InstanceType, instanceType) {
			vcpus = r.VCPUs
			break
		}
	}
	if vcpus == 0 {
		return mcp.NewToolResultError(fmt.Sprintf("instance type %s not found in %s", instanceType, region)), nil
	}

	canLaunch, msg := client.CanLaunch(instanceType, vcpus, info, isSpot)

	quotaType := "On-Demand"
	if isSpot {
		quotaType = "Spot"
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Quota check: %s %s in %s\n", instanceType, quotaType, region))
	sb.WriteString(fmt.Sprintf("Instance family: %s  vCPUs required: %d\n\n", family, vcpus))

	quotaMap := info.OnDemand
	if isSpot {
		quotaMap = info.Spot
	}
	if q, ok := quotaMap[family]; ok {
		usage := info.Usage[family]
		sb.WriteString(fmt.Sprintf("Quota: %d vCPUs  In use: %d vCPUs  Available: %d vCPUs\n", q, usage, q-usage))
	}

	if canLaunch {
		sb.WriteString(fmt.Sprintf("\n✅ Launch allowed: %s", msg))
	} else {
		sb.WriteString(fmt.Sprintf("\n❌ Launch blocked: %s\n", msg))
		cmd := quotas.QuotaIncreaseCommand(region, family, vcpus*2, isSpot)
		if cmd != "" {
			sb.WriteString(fmt.Sprintf("\nTo request a quota increase:\n  %s\n", cmd))
		}
	}

	return mcp.NewToolResultText(sb.String()), nil
}
