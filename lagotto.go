package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/google/uuid"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/spore-host/lagotto/pkg/watcher"
	spawnclient "github.com/spore-host/spawn/pkg/aws"
)

// Default lagotto DynamoDB table names — the same defaults the lagotto CLI uses
// (cmd/root.go: --watches-table / --history-table). The MCP server has no flag
// layer, so it reads/writes the conventional tables directly.
const (
	lagottoWatchesTable = "lagotto-watches"
	lagottoHistoryTable = "lagotto-match-history"
)

func registerLagottoTools(s *server.MCPServer) {
	s.AddTool(mcp.NewTool("lagotto_list",
		mcp.WithDescription("List your lagotto capacity watches. Returns each watch's project, owner, status, instance-type pattern, regions, action, and (when it matched or gave up) the derived wait-to-acquire / time-to-give-up. Read-only. Only watches created by the calling AWS identity are shown."),
		mcp.WithString("project",
			mcp.Description("Only show watches with this project label."),
		),
		mcp.WithBoolean("all",
			mcp.Description("Show watches in all states. When false (default) only active watches are listed."),
			mcp.DefaultBool(false),
		),
	), handleLagottoList)

	s.AddTool(mcp.NewTool("lagotto_status",
		mcp.WithDescription("Show the full details of one lagotto watch by its ID, including project, owner, pattern, regions, action, spot/max-price, status, and the derived wait-to-acquire / time-to-give-up timings. Read-only. You must be the watch's owner."),
		mcp.WithString("watch_id",
			mcp.Description("The watch ID (e.g. w-1a2b3c4d)."),
			mcp.Required(),
		),
	), handleLagottoStatus)

	s.AddTool(mcp.NewTool("lagotto_watch",
		mcp.WithDescription("Create a lagotto capacity watch: lagotto polls for the requested instance-type capacity across regions and takes an action when it's found. Supports action=notify (send a notification only — nothing billable) and action=hold (place an On-Demand Capacity Reservation to HOLD the capacity — this reserves billable EC2 capacity for as long as the reservation is held). action=spawn is not supported here (it needs a full spawn launch-config); use the lagotto CLI for that. The watch stops when it matches or its TTL elapses."),
		mcp.WithString("pattern",
			mcp.Description("Instance-type pattern to watch: an exact type (g5.xlarge), a wildcard (p5.*), or a comma-separated list (g6.4xlarge,g6.2xlarge) matching ANY listed rung."),
			mcp.Required(),
		),
		mcp.WithString("regions",
			mcp.Description("Comma-separated AWS regions to watch (e.g. us-east-1,us-west-2). Omit to watch all enabled regions."),
		),
		mcp.WithString("action",
			mcp.Description("Action on match: notify (default) or hold. hold reserves billable capacity via an On-Demand Capacity Reservation."),
			mcp.DefaultString("notify"),
		),
		mcp.WithString("project",
			mcp.Description("Optional project label for scoping (a shared-account poller can service only its own project's watches)."),
		),
		mcp.WithString("ttl",
			mcp.Description("How long to keep watching before giving up, e.g. 24h, 7d, 1w (default 24h). Units w/d/h/m/s."),
			mcp.DefaultString("24h"),
		),
		mcp.WithBoolean("spot",
			mcp.Description("Watch for Spot capacity instead of On-Demand."),
			mcp.DefaultBool(false),
		),
		mcp.WithNumber("max_price",
			mcp.Description("Maximum acceptable price per hour (0 = any)."),
			mcp.DefaultNumber(0),
		),
		mcp.WithString("notify",
			mcp.Description("Comma-separated notification channels for action=notify, each type:target — email:user@example.com, sns:arn:aws:sns:..., or webhook:https://..."),
		),
	), handleLagottoWatch)
}

// lagottoContext resolves an AWS config (via the same spawn client the rest of
// the server uses, so credential resolution is identical), a lagotto Store bound
// to the conventional tables, and the caller's ARN (watches are owner-scoped).
func lagottoContext(ctx context.Context) (*watcher.Store, awssdk.Config, string, error) {
	client, err := spawnclient.NewClient(ctx)
	if err != nil {
		return nil, awssdk.Config{}, "", err
	}
	cfg := client.Config()
	identity, err := sts.NewFromConfig(cfg).GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return nil, awssdk.Config{}, "", fmt.Errorf("get caller identity: %w", err)
	}
	store := watcher.NewStore(cfg, lagottoWatchesTable, lagottoHistoryTable)
	return store, cfg, awssdk.ToString(identity.Arn), nil
}

// shortOwner renders a watch's UserID (the creator's caller ARN) as the compact
// trailing resource segment — e.g. "user/alice" — mirroring the lagotto CLI's
// list output. Returns "-" for an empty owner (legacy watches).
func shortOwner(arn string) string {
	if arn == "" {
		return "-"
	}
	if i := strings.LastIndex(arn, ":"); i >= 0 && i+1 < len(arn) {
		return arn[i+1:]
	}
	return arn
}

func displayRegions(regions []string) string {
	if len(regions) == 0 {
		return "(all enabled)"
	}
	return strings.Join(regions, ", ")
}

func handleLagottoList(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	project, _ := args["project"].(string)
	all, _ := args["all"].(bool)

	store, _, caller, err := lagottoContext(ctx)
	if err != nil {
		return mcp.NewToolResultError("failed to connect to AWS: " + err.Error()), nil
	}

	var statusFilter watcher.WatchStatus
	if !all {
		statusFilter = watcher.StatusActive
	}

	watches, err := store.ListWatchesByUser(ctx, caller, statusFilter)
	if err != nil {
		return mcp.NewToolResultError("list watches: " + err.Error()), nil
	}

	// Reuse the poller's filter logic for --project (the same code the CLI uses),
	// rather than re-implementing the label match.
	filter := &watcher.WatchFilter{Project: project}
	if !filter.Empty() {
		kept := watches[:0]
		for _, w := range watches {
			if filter.Matches(&w) {
				kept = append(kept, w)
			}
		}
		watches = kept
	}

	if len(watches) == 0 {
		scope := "active"
		if all {
			scope = "any-state"
		}
		if project != "" {
			return mcp.NewToolResultText(fmt.Sprintf("No %s watches found for project %q.", scope, project)), nil
		}
		return mcp.NewToolResultText(fmt.Sprintf("No %s watches found.", scope)), nil
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Found %d watch(es):\n\n", len(watches)))
	for i := range watches {
		w := &watches[i]
		w.ComputeDurations()
		action := string(w.Action)
		if w.DesiredCount > 0 {
			action = fmt.Sprintf("%s×%d", w.Action, w.DesiredCount)
		}
		sb.WriteString(fmt.Sprintf("• %s  [%s]\n", w.WatchID, w.Status))
		sb.WriteString(fmt.Sprintf("  Project: %s  Owner: %s\n", dashIfEmpty(w.Project), shortOwner(w.UserID)))
		sb.WriteString(fmt.Sprintf("  Pattern: %s  Regions: %s\n", w.InstanceTypePattern, displayRegions(w.Regions)))
		sb.WriteString(fmt.Sprintf("  Action: %s  Spot: %v", action, w.Spot))
		if w.MaxPrice > 0 {
			sb.WriteString(fmt.Sprintf("  Max price: $%.4f/hr", w.MaxPrice))
		}
		sb.WriteString("\n")
		if d, ok := w.WaitToAcquire(); ok {
			sb.WriteString(fmt.Sprintf("  Wait to acquire: %s\n", watcher.FormatWait(d)))
		}
		if d, ok := w.TimeToGiveUp(); ok {
			sb.WriteString(fmt.Sprintf("  Time to give up: %s\n", watcher.FormatWait(d)))
		}
		sb.WriteString(fmt.Sprintf("  Expires: %s\n\n", w.ExpiresAt.Format(time.RFC3339)))
	}
	return mcp.NewToolResultText(strings.TrimRight(sb.String(), "\n")), nil
}

func dashIfEmpty(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func handleLagottoStatus(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	watchID, _ := args["watch_id"].(string)
	if strings.TrimSpace(watchID) == "" {
		return mcp.NewToolResultError("watch_id is required"), nil
	}

	store, _, caller, err := lagottoContext(ctx)
	if err != nil {
		return mcp.NewToolResultError("failed to connect to AWS: " + err.Error()), nil
	}

	w, err := store.GetWatch(ctx, watchID)
	if err != nil {
		return mcp.NewToolResultError("get watch: " + err.Error()), nil
	}
	// Owner-scope: a watch ID is guessable and the tables can be shared, so return
	// the SAME "not found" for a watch the caller doesn't own (no existence oracle),
	// mirroring the lagotto CLI's getWatchOwned.
	if w == nil || (w.UserID != "" && caller != "" && w.UserID != caller) {
		return mcp.NewToolResultError(fmt.Sprintf("watch %s not found", watchID)), nil
	}

	w.ComputeDurations()

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Watch:    %s\n", w.WatchID))
	sb.WriteString(fmt.Sprintf("Status:   %s\n", w.Status))
	if w.Project != "" {
		sb.WriteString(fmt.Sprintf("Project:  %s\n", w.Project))
	}
	if w.UserID != "" {
		sb.WriteString(fmt.Sprintf("Owner:    %s\n", w.UserID))
	}
	sb.WriteString(fmt.Sprintf("Pattern:  %s\n", w.InstanceTypePattern))
	sb.WriteString(fmt.Sprintf("Regions:  %s\n", displayRegions(w.Regions)))
	if len(w.AvailabilityZones) > 0 {
		sb.WriteString(fmt.Sprintf("AZs:      %s\n", strings.Join(w.AvailabilityZones, ", ")))
	}
	sb.WriteString(fmt.Sprintf("Spot:     %v\n", w.Spot))
	if w.MaxPrice > 0 {
		sb.WriteString(fmt.Sprintf("Max price: $%.4f/hr\n", w.MaxPrice))
	}
	sb.WriteString(fmt.Sprintf("Action:   %s\n", w.Action))
	if w.DesiredCount > 0 {
		sb.WriteString(fmt.Sprintf("Fleet:    maintain %d worker(s)\n", w.DesiredCount))
		if w.CompletionCondition != "" {
			sb.WriteString(fmt.Sprintf("Until:    %s\n", w.CompletionCondition))
		}
	}
	sb.WriteString(fmt.Sprintf("Created:  %s\n", w.CreatedAt.Format(time.RFC3339)))
	sb.WriteString(fmt.Sprintf("Expires:  %s\n", w.ExpiresAt.Format(time.RFC3339)))
	if !w.LastPolledAt.IsZero() {
		sb.WriteString(fmt.Sprintf("Last polled: %s\n", w.LastPolledAt.Format(time.RFC3339)))
	}
	sb.WriteString(fmt.Sprintf("Matches:  %d\n", w.MatchCount))
	if d, ok := w.WaitToAcquire(); ok {
		sb.WriteString(fmt.Sprintf("Wait to acquire: %s\n", watcher.FormatWait(d)))
	}
	if d, ok := w.TimeToGiveUp(); ok {
		sb.WriteString(fmt.Sprintf("Time to give up: %s\n", watcher.FormatWait(d)))
	}
	if m := w.LastMatch; m != nil {
		sb.WriteString("\nLast match:\n")
		sb.WriteString(fmt.Sprintf("  Instance: %s\n", m.InstanceType))
		sb.WriteString(fmt.Sprintf("  Region:   %s\n", m.Region))
		sb.WriteString(fmt.Sprintf("  AZ:       %s\n", m.AvailabilityZone))
		sb.WriteString(fmt.Sprintf("  Price:    $%.4f/hr\n", m.Price))
		sb.WriteString(fmt.Sprintf("  Spot:     %v\n", m.IsSpot))
		sb.WriteString(fmt.Sprintf("  Action:   %s\n", m.ActionTaken))
		if m.InstanceID != "" {
			sb.WriteString(fmt.Sprintf("  Instance ID: %s\n", m.InstanceID))
		}
	}
	return mcp.NewToolResultText(sb.String()), nil
}

// parseNotifyChannels parses "type:target" notification specs, mirroring the
// lagotto CLI (cmd/watch.go). Returns an error naming the bad entry.
func parseNotifyChannels(raw string) ([]watcher.NotifyChannel, error) {
	var channels []watcher.NotifyChannel
	for _, s := range strings.Split(raw, ",") {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		i := strings.IndexByte(s, ':')
		if i < 0 {
			return nil, fmt.Errorf("invalid notify format %q: expected type:target (e.g. email:user@example.com)", s)
		}
		ch := watcher.NotifyChannel{Type: s[:i], Target: s[i+1:]}
		switch ch.Type {
		case "email", "sns":
		case "webhook":
			if err := watcher.ValidateWebhookURL(ch.Target); err != nil {
				return nil, fmt.Errorf("invalid webhook URL: %w", err)
			}
		default:
			return nil, fmt.Errorf("invalid notify type %q: must be email, webhook, or sns", ch.Type)
		}
		channels = append(channels, ch)
	}
	return channels, nil
}

func parseRegionList(s string) []string {
	var regions []string
	for _, r := range strings.Split(s, ",") {
		if r = strings.TrimSpace(r); r != "" {
			regions = append(regions, r)
		}
	}
	return regions
}

func handleLagottoWatch(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	pattern, _ := args["pattern"].(string)
	regionsStr, _ := args["regions"].(string)
	actionStr, _ := args["action"].(string)
	project, _ := args["project"].(string)
	ttlStr, _ := args["ttl"].(string)
	spot, _ := args["spot"].(bool)
	maxPrice, _ := args["max_price"].(float64)
	notifyStr, _ := args["notify"].(string)

	if strings.TrimSpace(pattern) == "" {
		return mcp.NewToolResultError("pattern is required"), nil
	}
	if actionStr == "" {
		actionStr = "notify"
	}
	if ttlStr == "" {
		ttlStr = "24h"
	}

	// EC2-only via MCP: SageMaker watches need a job definition file, which is out
	// of scope for this tool.
	service := watcher.ServiceEC2
	if err := watcher.ValidateWatchPattern(service, pattern); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	// Only notify + hold are supported here. spawn needs a full launch-config
	// (file/S3/stdin parsing + self-contained resolution) — defer that to the CLI.
	action := watcher.ActionMode(actionStr)
	switch action {
	case watcher.ActionNotify, watcher.ActionHold:
	case watcher.ActionSpawn:
		return mcp.NewToolResultError("action=spawn is not supported via MCP (it requires a spawn launch-config); create it with the lagotto CLI: lagotto watch <pattern> --action spawn --spawn-config <file>"), nil
	default:
		return mcp.NewToolResultError(fmt.Sprintf("invalid action %q: must be notify or hold", actionStr)), nil
	}

	// Accept both Go durations (24h) and lagotto's short form (7d, 1w).
	ttl, err := time.ParseDuration(ttlStr)
	if err != nil {
		ttl, err = watcher.ParseDuration(ttlStr)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("invalid ttl %q: %v", ttlStr, err)), nil
		}
	}

	channels, err := parseNotifyChannels(notifyStr)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	store, _, caller, err := lagottoContext(ctx)
	if err != nil {
		return mcp.NewToolResultError("failed to connect to AWS: " + err.Error()), nil
	}

	now := time.Now().UTC()
	expiresAt := now.Add(ttl)
	w := &watcher.Watch{
		WatchID:             "w-" + uuid.New().String()[:8],
		UserID:              caller,
		Project:             project,
		Status:              watcher.StatusActive,
		Service:             service,
		InstanceTypePattern: pattern,
		Regions:             parseRegionList(regionsStr),
		Spot:                spot,
		MaxPrice:            maxPrice,
		Action:              action,
		NotifyChannels:      channels,
		CreatedAt:           now,
		UpdatedAt:           now,
		ExpiresAt:           expiresAt,
		TTLTimestamp:        expiresAt.Unix(),
	}

	// Auto-create the backing tables on first use (zero-setup), like the CLI.
	if _, err := store.EnsureTables(ctx); err != nil {
		return mcp.NewToolResultError("ensure tables: " + err.Error()), nil
	}
	if err := store.PutWatch(ctx, w); err != nil {
		return mcp.NewToolResultError("create watch: " + err.Error()), nil
	}

	auditLog("lagotto_watch", w.WatchID, "", "")

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("✅ Created watch %s\n", w.WatchID))
	sb.WriteString(fmt.Sprintf("  Pattern: %s\n", w.InstanceTypePattern))
	sb.WriteString(fmt.Sprintf("  Regions: %s\n", displayRegions(w.Regions)))
	if w.Project != "" {
		sb.WriteString(fmt.Sprintf("  Project: %s\n", w.Project))
	}
	sb.WriteString(fmt.Sprintf("  Spot:    %v\n", w.Spot))
	sb.WriteString(fmt.Sprintf("  Action:  %s\n", w.Action))
	if w.Action == watcher.ActionHold {
		sb.WriteString("  Note:    hold reserves billable On-Demand capacity on match.\n")
	}
	sb.WriteString(fmt.Sprintf("  Expires: %s\n", w.ExpiresAt.Format(time.RFC3339)))
	return mcp.NewToolResultText(sb.String()), nil
}
