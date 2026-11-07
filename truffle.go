package main

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	truffleaws "github.com/spore-host/truffle/pkg/aws"
	"github.com/spore-host/truffle/pkg/find"
	"github.com/spore-host/truffle/pkg/quotas"
)

// maxFindResults caps how many instance-type matches truffle_find renders. A
// broad query can match hundreds of type×region rows; truffle returns them in
// preference order, so the top slice is the useful part and the rest only bloat
// the tool result. The caller is told to narrow the query when it's clipped.
const maxFindResults = 25

// exactTypeMatcher builds an anchored, literal regexp matching one instance type.
//
// truffle's SearchInstanceTypes REQUIRES a non-nil matcher: a nil matcher panics
// (nil-pointer deref) inside its per-region goroutine at extractSpecificTypes,
// which — because this is an in-process stdio server — crashes the whole server
// and disconnects every tool, not just this call. So the spot-price and
// quota-check paths (which look up a single named type) must pass a real matcher.
// (Upstream nil-guard tracked in spore-host/truffle#106.)
//
// Anchoring with QuoteMeta also lets truffle's extractSpecificTypes recognise an
// exact type and push it into the DescribeInstanceTypes API-side InstanceTypes
// filter, instead of enumerating every instance type in the region and filtering
// in Go — far fewer/lighter API calls.
func exactTypeMatcher(instanceType string) *regexp.Regexp {
	return regexp.MustCompile("^" + regexp.QuoteMeta(instanceType) + "$")
}

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
		// A query of only unknown words (truffle#229) used to search the whole
		// catalog; truffle now refuses it, and its message already names the
		// words, suggests close ones and lists what find understands (#56).
		var uq *find.UnrecognizedQueryError
		if errors.As(err, &uq) {
			return mcp.NewToolResultError(uq.Error()), nil
		}
		return mcp.NewToolResultError(fmt.Sprintf("parse query: %v", err)), nil
	}

	// BuildCriteria gained an includeAZs parameter in truffle v0.53.0
	// (truffle#141): it toggles the per-type availability-zone lookup that the
	// search performs. Pass true to preserve the pre-bump behavior — the old
	// BuildCriteria() hardcoded IncludeAZs: true — so truffle_find keeps
	// returning AZ detail. (truffle's CLI defaults to this too; --skip-azs is
	// the opt-out and mcp exposes no such knob.)
	criteria, err := pq.BuildCriteria(true)
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
		msg := fmt.Sprintf("No instance types found matching %q in %s.", query, strings.Join(regions, ", "))
		hint := ""
		if pq.HasNoMatchingFamily() {
			// e.g. "intel h100": the constraints exclude each other (truffle#229).
			hint = "No instance family satisfies all of the query's constraints together; drop one, or add \"or\" to match any of them."
		}
		// No match: the existing message is the summary line (#75), and the
		// notes on how the query was read follow it.
		return withSummary(msg, findQueryNotes(pq)+hint), nil
	}

	// Cap the rendered results. A broad query ("gpu", "arm64") can match hundreds
	// of type×region rows; dumping them all bloats the tool result (and the spot
	// fetch below). Keep the top maxFindResults — truffle already returns them in
	// its preference order — and tell the caller to narrow the query for the rest.
	total := len(results)
	if len(results) > maxFindResults {
		results = results[:maxFindResults]
	}

	// Optionally fetch spot prices (only for the results we'll actually render).
	var spotPrices []truffleaws.SpotPriceResult
	if withSpot {
		spotPrices, _ = client.GetSpotPricing(ctx, results, truffleaws.SpotOptions{ShowSavings: true})
	}
	spotByType := make(map[string]truffleaws.SpotPriceResult)
	for _, sp := range spotPrices {
		spotByType[sp.InstanceType] = sp
	}

	return withSummary(findSummary(query, results, total, spotByType),
		findQueryNotes(pq)+renderFindResults(query, results, total, spotByType, pq)), nil
}

// findSummary is truffle_find's summary line (#75) for a search that matched.
// One instance type (in one or more regions) gets its specs, where it is
// offered and its prices; several get the count and the cheapest On-Demand
// rate among those shown.
func findSummary(query string, results []truffleaws.InstanceTypeResult, total int, spotByType map[string]truffleaws.SpotPriceResult) string {
	if len(results) == 0 {
		return fmt.Sprintf("No instance types found matching %q.", query)
	}
	single := total == len(results)
	for _, r := range results[1:] {
		if r.InstanceType != results[0].InstanceType {
			single = false
		}
	}
	if single {
		r := results[0]
		var sb strings.Builder
		fmt.Fprintf(&sb, "%s: %d vCPU, %s GiB, %s", r.InstanceType, r.VCPUs, gib(r.MemoryMiB), r.Architecture)
		if r.GPUs > 0 {
			fmt.Fprintf(&sb, ", %d× %s GPU", r.GPUs, r.GPUModel)
		}
		var where []string
		odPrice, odRegion := 0.0, ""
		for _, row := range results {
			where = append(where, regionWithAZs(row.Region, row.AvailableAZs))
			if row.OnDemandPrice > 0 && (odPrice == 0 || row.OnDemandPrice < odPrice) {
				odPrice, odRegion = row.OnDemandPrice, row.Region
			}
		}
		fmt.Fprintf(&sb, "; offered in %s", strings.Join(where, ", "))
		var prices []string
		if odPrice > 0 {
			p := fmt.Sprintf("$%.4f/hr On-Demand", odPrice)
			if len(results) > 1 {
				p += " in " + odRegion
			}
			prices = append(prices, p)
		}
		if sp, ok := spotByType[r.InstanceType]; ok && sp.SpotPrice > 0 {
			prices = append(prices, fmt.Sprintf("$%.4f/hr Spot", sp.SpotPrice))
		}
		if len(prices) > 0 {
			sb.WriteString("; " + strings.Join(prices, ", "))
		}
		return sb.String()
	}

	head := fmt.Sprintf("%d matches for %q", total, query)
	cheapest := "cheapest On-Demand"
	if total > len(results) {
		head += fmt.Sprintf(" (top %d shown)", len(results))
		cheapest = "cheapest shown On-Demand"
	}
	var best *truffleaws.InstanceTypeResult
	for i := range results {
		if p := results[i].OnDemandPrice; p > 0 && (best == nil || p < best.OnDemandPrice) {
			best = &results[i]
		}
	}
	if best == nil {
		return head + "; no On-Demand price available"
	}
	return fmt.Sprintf("%s; %s: %s at $%.4f/hr in %s", head, cheapest, best.InstanceType, best.OnDemandPrice, best.Region)
}

// gib renders MiB as GiB without a trailing ".0".
func gib(mib int64) string {
	return strings.TrimSuffix(fmt.Sprintf("%.1f", float64(mib)/1024.0), ".0")
}

// regionWithAZs is "us-west-2 (a, b, c)": the region and the zone letters of
// its AZs that offer the type, or the bare region when the AZs are unknown.
func regionWithAZs(region string, azs []string) string {
	if len(azs) == 0 {
		return region
	}
	letters := make([]string, 0, len(azs))
	for _, az := range azs {
		letters = append(letters, strings.TrimPrefix(az, region))
	}
	return fmt.Sprintf("%s (%s)", region, strings.Join(letters, ", "))
}

// spotSummary is truffle_spot_prices' summary line (#75): the lowest Spot
// price found, where, and its saving over On-Demand when known.
func spotSummary(instanceType string, prices []truffleaws.SpotPriceResult) string {
	if len(prices) == 0 {
		return fmt.Sprintf("No Spot price data available for %s.", instanceType)
	}
	low := prices[0]
	regions := map[string]bool{}
	for _, p := range prices {
		regions[p.Region] = true
		if p.SpotPrice < low.SpotPrice {
			low = p
		}
	}
	line := fmt.Sprintf("%s Spot: lowest $%.4f/hr in %s", instanceType, low.SpotPrice, low.AvailabilityZone)
	if low.OnDemandPrice > 0 {
		line += fmt.Sprintf(" (%.0f%% below On-Demand $%.4f/hr)", low.SavingsPercent, low.OnDemandPrice)
	}
	return line + fmt.Sprintf("; %d price(s) across %d region(s)", len(prices), len(regions))
}

// quotaSummary is truffle_quota_check's summary line (#75). quota and usage
// are the family's vCPU quota and usage; known is false when the quota could
// not be read, and then msg (CanLaunch's reason) says why.
func quotaSummary(instanceType, quotaType, region string, vcpus, quota, usage int32, known, canLaunch, increaseCmd bool, msg string) string {
	verdict := "❌"
	word := "blocked"
	if canLaunch {
		verdict, word = "✅", "allowed"
	}
	line := fmt.Sprintf("%s %s %s in %s: %s — needs %d vCPU", verdict, instanceType, quotaType, region, word, vcpus)
	if known {
		line += fmt.Sprintf(", %d of %d available", quota-usage, quota)
	} else if msg != "" {
		line += "; " + msg
	}
	if !canLaunch && increaseCmd {
		line += " (increase command below)"
	}
	return line
}

// findQueryNotes says how the query was read where that is not obvious, as
// truffle's CLI does: words it ignored, and a bare size in a GPU query taken
// as per-GPU memory. Empty when there is nothing to say.
func findQueryNotes(pq *find.ParsedQuery) string {
	var sb strings.Builder
	if unknown := pq.UnknownTerms(); len(unknown) > 0 {
		fmt.Fprintf(&sb, "Note: ignored unknown term(s): %s\n", strings.Join(unknown, ", "))
	}
	if note := pq.MemoryReadingNote(); note != "" {
		fmt.Fprintln(&sb, note)
	}
	if sb.Len() > 0 {
		sb.WriteString("\n")
	}
	return sb.String()
}

// renderFindResults formats truffle_find output. results is the (already-capped)
// slice to render; total is the full match count before capping, so the header
// can flag truncation. Pure/AWS-free so the cap + truncation-note behavior is
// unit-testable.
func renderFindResults(query string, results []truffleaws.InstanceTypeResult, total int, spotByType map[string]truffleaws.SpotPriceResult, pq *find.ParsedQuery) string {
	var sb strings.Builder
	if total > len(results) {
		fmt.Fprintf(&sb, "Found %d instance type(s) matching %q (showing the top %d — narrow the query for the rest):\n\n", total, query, len(results))
	} else {
		fmt.Fprintf(&sb, "Found %d instance type(s) matching %q:\n\n", total, query)
	}

	for _, r := range results {
		fmt.Fprintf(&sb, "**%s** (%s)\n", r.InstanceType, r.Region)
		fmt.Fprintf(&sb, "  vCPUs: %d  Memory: %.1f GiB  Arch: %s\n",
			r.VCPUs, float64(r.MemoryMiB)/1024.0, r.Architecture)
		if r.GPUs > 0 {
			fmt.Fprintf(&sb, "  GPUs: %d× %s (%s, %.0f GB VRAM)\n",
				r.GPUs, r.GPUModel, r.GPUManufacturer, float64(r.GPUMemoryMiB)/1024.0)
		}
		if r.OnDemandPrice > 0 {
			fmt.Fprintf(&sb, "  On-Demand: $%.4f/hr\n", r.OnDemandPrice)
		}
		if sp, ok := spotByType[r.InstanceType]; ok {
			fmt.Fprintf(&sb, "  Spot: $%.4f/hr (%.0f%% savings, %s)\n",
				sp.SpotPrice, sp.SavingsPercent, sp.AvailabilityZone)
		}
		if len(r.AvailableAZs) > 0 {
			fmt.Fprintf(&sb, "  Available AZs: %s\n", strings.Join(r.AvailableAZs, ", "))
		}
		// Human-readable match reasons ("GPU: A100 (80 GiB, training)", "memory ≥
		// 64 GiB") from truffle — tells the caller WHY each type matched, which is
		// far more useful to an assistant than the bare specs.
		if reasons := find.ExplainMatch(r, pq); len(reasons) > 0 {
			fmt.Fprintf(&sb, "  Why: %s\n", strings.Join(reasons, "; "))
		}
		sb.WriteString("\n")
	}

	return strings.TrimRight(sb.String(), "\n")
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

	// Search for this exact instance type first to get metadata. Pass an anchored
	// matcher (never nil — see exactTypeMatcher) so truffle uses the API-side type
	// filter and doesn't panic.
	results, err := client.SearchInstanceTypes(ctx, regions, exactTypeMatcher(strings.ToLower(instanceType)), truffleaws.FilterOptions{})
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
		return withSummary(fmt.Sprintf("Instance type %s not found in %s.", instanceType, strings.Join(regions, ", ")), ""), nil
	}

	spotPrices, err := client.GetSpotPricing(ctx, filtered, truffleaws.SpotOptions{ShowSavings: true})
	if err != nil {
		return mcp.NewToolResultError("spot price lookup failed: " + err.Error()), nil
	}
	if len(spotPrices) == 0 {
		return withSummary(spotSummary(instanceType, nil), ""), nil
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "Spot prices for %s:\n\n", instanceType)
	for _, sp := range spotPrices {
		fmt.Fprintf(&sb, "  %s (%s): $%.4f/hr", sp.Region, sp.AvailabilityZone, sp.SpotPrice)
		if sp.OnDemandPrice > 0 {
			fmt.Fprintf(&sb, "  [on-demand: $%.4f/hr, %.0f%% savings]", sp.OnDemandPrice, sp.SavingsPercent)
		}
		sb.WriteString("\n")
	}
	return withSummary(spotSummary(instanceType, spotPrices), sb.String()), nil
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
	// Anchored matcher (never nil — see exactTypeMatcher): API-side type filter,
	// and avoids the nil-matcher panic that would crash the whole server.
	results, err := trClient.SearchInstanceTypes(ctx, []string{region}, exactTypeMatcher(strings.ToLower(instanceType)), truffleaws.FilterOptions{})
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
	fmt.Fprintf(&sb, "Quota check: %s %s in %s\n", instanceType, quotaType, region)
	fmt.Fprintf(&sb, "Instance family: %s  vCPUs required: %d\n\n", family, vcpus)

	quotaMap := info.OnDemand
	if isSpot {
		quotaMap = info.Spot
	}
	q, known := quotaMap[family]
	// The usage CanLaunch weighed: Spot usage for a Spot check. This printed
	// On-Demand usage under a Spot quota until #75.
	usage := info.Usage[family]
	if isSpot {
		usage = info.SpotUsage[family]
	}
	if known {
		fmt.Fprintf(&sb, "Quota: %d vCPUs  In use: %d vCPUs  Available: %d vCPUs\n", q, usage, q-usage)
	}

	cmd := ""
	if canLaunch {
		fmt.Fprintf(&sb, "\n✅ Launch allowed: %s", msg)
	} else {
		fmt.Fprintf(&sb, "\n❌ Launch blocked: %s\n", msg)
		cmd = quotas.QuotaIncreaseCommand(region, family, vcpus*2, isSpot)
		if cmd != "" {
			fmt.Fprintf(&sb, "\nTo request a quota increase:\n  %s\n", cmd)
		}
	}

	return withSummary(quotaSummary(instanceType, quotaType, region, vcpus, q, usage, known, canLaunch, cmd != "", msg), sb.String()), nil
}
