package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/spore-host/libs/catalog"
	spawnclient "github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/tasklaunch"
	"github.com/spore-host/spawn/pkg/taskproto"
)

// registerLaunchTools registers the two spawn LAUNCH tools. Both create real,
// billable EC2 instances, so both carry cost guardrails: a TTL is mandatory
// (rejected up front if absent) and a dry_run flag resolves/plans without
// launching. The billable nature is stated in each tool Description so an
// assistant surfaces it to the user.
func registerLaunchTools(s *server.MCPServer) {
	s.AddTool(mcp.NewTool("spawn_task_run",
		mcp.WithDescription("Launch a spawn TASK from a TaskSpec (the workflow-adapter contract). ⚠️ Creates a real, BILLABLE EC2 instance: it sizes the cheapest instance type that fits the resource request, launches an ephemeral instance that stages inputs from S3, runs the command, stages outputs back, and self-terminates on completion / TTL. The TaskSpec MUST include lifecycle.ttl (a death clock) or the call is rejected. Set dry_run=true to size and preview the plan WITHOUT launching (no AWS launch, no cost)."),
		mcp.WithString("spec",
			mcp.Description("The TaskSpec as a JSON object (string). Required fields: task_id, command (argv array), resources (cpu/memory_gib/gpus/…), and lifecycle.ttl (e.g. \"4h\"). In a region with no default VPC, set placement.subnet_id to a subnet- id; without it the plan fails and lists subnets to use. See spawn's docs/workflow-adapter-protocol-rfc.md."),
			mcp.Required(),
		),
		mcp.WithString("region",
			mcp.Description("AWS region to launch in. Omit to use the configured default region."),
		),
		mcp.WithBoolean("dry_run",
			mcp.Description("When true, size and preview the task without launching anything (no billable resources created). Strongly recommended before a real launch."),
			mcp.DefaultBool(false),
		),
	), handleSpawnTaskRun)

	s.AddTool(mcp.NewTool("spawn_app_launch",
		mcp.WithDescription("Launch a spawn APP from the catalog — kind application (a GUI app over DCV), desktop (a bare Linux desktop over DCV), or web (an app serving its own web UI, TLS-proxied). ⚠️ A real launch creates a real, BILLABLE EC2 instance. A TTL is MANDATORY (rejected if absent). Set dry_run=true (recommended) to resolve the app, kind, instance type, base AMI, and TTL and preview the plan WITHOUT launching. NOTE: a non-dry-run real launch is intentionally not performed by this MCP tool — the DCV/web/session orchestration is deferred to the `spawn app launch` CLI (human-gated); this tool validates and plans the launch."),
		mcp.WithString("app",
			mcp.Description("Catalog app name or alias (e.g. paraview, jupyter, code-server). Run the spawn CLI 'spawn app list' to see available apps."),
			mcp.Required(),
		),
		mcp.WithString("ttl",
			mcp.Description("Instance TTL / death clock, Go duration units h/m/s (e.g. 4h, 24h). MANDATORY — the call is rejected without it."),
			mcp.Required(),
		),
		mcp.WithString("instance_type",
			mcp.Description("EC2 instance type override. Omit to use the app's default family (<family>.xlarge)."),
		),
		mcp.WithString("app_version",
			mcp.Description("Image tag / app version to launch (containerized apps only). Omit for the catalog default."),
		),
		mcp.WithString("region",
			mcp.Description("AWS region to launch in. Omit to use the configured default region."),
		),
		mcp.WithNumber("web_port",
			mcp.Description("For a BYO web app: the container HTTP port to proxy. Forces kind=web when > 0."),
			mcp.DefaultNumber(0),
		),
		mcp.WithBoolean("dry_run",
			mcp.Description("When true (recommended), resolve and preview the launch plan without launching. A real launch is deferred to the CLI regardless."),
			mcp.DefaultBool(false),
		),
	), handleSpawnAppLaunch)
}

// newTaskDeps builds what spawn_task_run plans and launches with, and resolves
// the region. It is a variable so tests can hand the real handler fake
// dependencies; nothing else assigns it.
//
// The dependencies are spawn's own (tasklaunch.NewDeps): the same finder, the
// same account-side calls and the same launcher.Provision the `spawn task run`
// CLI uses. Progress and warnings, including the label on any static price the
// finder ranked on and the Spot-to-On-Demand fallback notice, go to notes.
var newTaskDeps = func(ctx context.Context, region string, notes io.Writer) (tasklaunch.Deps, string, error) {
	client, err := spawnClient(ctx)
	if err != nil {
		return tasklaunch.Deps{}, "", fmt.Errorf("failed to connect to AWS: %w", err)
	}
	if region == "" {
		region = client.Config().Region
	}
	if region == "" {
		return tasklaunch.Deps{}, "", fmt.Errorf("no region: pass region or configure a default AWS region")
	}
	return tasklaunch.NewDeps(client, region, notes), region, nil
}

func handleSpawnTaskRun(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	specJSON, _ := args["spec"].(string)
	region, _ := args["region"].(string)
	dryRun, _ := args["dry_run"].(bool)

	if strings.TrimSpace(specJSON) == "" {
		return mcp.NewToolResultError("spec is required (a TaskSpec JSON object)"), nil
	}

	// Parse + validate. taskproto.Validate REQUIRES lifecycle.ttl — surface that as
	// an explicit cost-safety rejection so the caller (and user) understand a task
	// must carry a death clock before it can create a billable instance.
	spec, err := taskproto.ParseSpec([]byte(specJSON))
	if err != nil {
		if strings.Contains(err.Error(), "lifecycle.ttl") {
			return mcp.NewToolResultError("TTL required: the TaskSpec must set lifecycle.ttl (e.g. \"4h\") — a task launches a billable EC2 instance and must have a death clock. (" + err.Error() + ")"), nil
		}
		return mcp.NewToolResultError(err.Error()), nil
	}

	// Placement storage (attached volumes / EFS / FSx) is not offered through
	// MCP. Reject clearly rather than launch it from here.
	if len(spec.Placement.Volumes) > 0 || spec.Placement.EFSID != "" || spec.Placement.FSxLustreID != "" {
		return mcp.NewToolResultError("placement storage (volumes / efs_id / fsx_lustre_id) is not supported via MCP; launch this task with the spawn CLI: spawn task run --spec <file>"), nil
	}

	var notes bytes.Buffer
	deps, region, err := newTaskDeps(ctx, region, &notes)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	if dryRun {
		return planTask(ctx, deps, spec, region, &notes)
	}
	return launchTask(ctx, deps, spec, region, &notes)
}

// planTask is the dry run: spawn's own Plan and Render, the output of
// `spawn task run --dry-run`, plus the plan's warnings and any notes the finder
// wrote (a static-price label, for one). Nothing is created.
func planTask(ctx context.Context, deps tasklaunch.Deps, spec *taskproto.TaskSpec, region string, notes *bytes.Buffer) (*mcp.CallToolResult, error) {
	plan, err := tasklaunch.Plan(ctx, deps, spec, region)
	if err != nil {
		return mcp.NewToolResultError("plan task: " + err.Error()), nil
	}
	var sb strings.Builder
	tasklaunch.Render(&sb, plan)
	for _, w := range plan.Warnings {
		sb.WriteString(w)
	}
	writeNotes(&sb, notes)
	sb.WriteString("\nRe-run with dry_run=false to launch this task.")
	return mcp.NewToolResultText(sb.String()), nil
}

// launchTask launches the task through spawn's tasklaunch.Launch, the code
// behind `spawn task run`: it sizes with the container memory floor (#60),
// retries a Spot capacity failure On-Demand with the same launcher.Options when
// the spec asks for it (#61), and writes the task's records under the resolved
// results prefix, honouring the spec's results_prefix and creating only
// spawn's own bucket (#64). Everything reported here is read from the result,
// so the reported completion path is the one the instance writes.
func launchTask(ctx context.Context, deps tasklaunch.Deps, spec *taskproto.TaskSpec, region string, notes *bytes.Buffer) (*mcp.CallToolResult, error) {
	auditLog("spawn_task_run", spec.TaskID, "", region)

	res, err := tasklaunch.Launch(ctx, deps, spec, region)
	if err != nil {
		var sb strings.Builder
		sb.WriteString(err.Error())
		writeNotes(&sb, notes)
		return mcp.NewToolResultError(sb.String()), nil
	}

	purchase := "On-Demand"
	if res.Spot {
		purchase = "Spot"
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "✅ Task launched: %s\n", res.TaskID)
	fmt.Fprintf(&sb, "Instance:   %s  (%s, %s) in %s / %s\n", res.InstanceID, res.InstanceType, purchase, res.Region, orDash(res.AvailabilityZone))
	fmt.Fprintf(&sb, "Run ID:     %s\n", res.RunID)
	fmt.Fprintf(&sb, "TTL:        %s   on-complete: %s\n", spec.Lifecycle.TTL, spec.EffectiveOnComplete())
	fmt.Fprintf(&sb, "Completion: %s\n", completionURI(res))
	writeNotes(&sb, notes)
	fmt.Fprintf(&sb, "\nPoll for completion with spawn_status %s, or:\n  spawn task status %s --region %s", res.TaskID, res.TaskID, res.Region)
	if spec.ResultsPrefix != "" {
		fmt.Fprintf(&sb, " --results-prefix %s", spec.ResultsPrefix)
	}
	sb.WriteString("\n")
	return mcp.NewToolResultText(sb.String()), nil
}

// completionURI is where the launched task's completion record lands: the
// resolved results prefix tasklaunch handed the wrapper, plus completion.json.
func completionURI(res *tasklaunch.Result) string {
	return res.ResultsPrefix + "/completion.json"
}

// writeNotes appends what spawn wrote to the notes writer, if anything.
func writeNotes(sb *strings.Builder, notes *bytes.Buffer) {
	if n := strings.TrimSpace(notes.String()); n != "" {
		sb.WriteString("\n" + n + "\n")
	}
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func handleSpawnAppLaunch(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	appArg, _ := args["app"].(string)
	ttl, _ := args["ttl"].(string)
	instanceType, _ := args["instance_type"].(string)
	appVersion, _ := args["app_version"].(string)
	region, _ := args["region"].(string)
	webPortF, _ := args["web_port"].(float64)
	dryRun, _ := args["dry_run"].(bool)
	webPort := int(webPortF)

	if strings.TrimSpace(appArg) == "" {
		return mcp.NewToolResultError("app is required"), nil
	}
	// COST-SAFETY: a launch creates a billable instance, so a TTL is mandatory and
	// validated up front (Go duration units h/m/s).
	if strings.TrimSpace(ttl) == "" {
		return mcp.NewToolResultError("TTL required: pass ttl (e.g. 4h) — an app launch creates a billable EC2 instance and must have a death clock"), nil
	}
	if _, err := time.ParseDuration(ttl); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("invalid ttl %q — use Go duration units (h/m/s), e.g. 4h, 24h", ttl)), nil
	}

	entry, ok := catalog.Lookup(appArg)
	if !ok {
		return mcp.NewToolResultError(fmt.Sprintf("application %q not found in catalog — run 'spawn app list' to see available apps", appArg)), nil
	}

	// Resolve kind + validate a web app has a port (mirrors cmd/app.go).
	kind := entry.Kind()
	if webPort > 0 {
		kind = catalog.KindWeb
		entry.Port = webPort
	}
	if kind == catalog.KindWeb && entry.Port <= 0 {
		return mcp.NewToolResultError(fmt.Sprintf("%s is a web app but no port is set — pass web_port <n>", entry.Name)), nil
	}

	// Resolve default instance type from the app's preferred family.
	if instanceType == "" {
		if len(entry.InstanceFamilies) == 0 {
			return mcp.NewToolResultError(fmt.Sprintf("no instance families defined for %s in catalog", entry.Name)), nil
		}
		instanceType = entry.InstanceFamilies[0] + ".xlarge"
	}

	// Validate the requested version / that the app is launchable (mirrors the CLI
	// resolution order): a desktop kind needs no image; a containerized app resolves
	// its tag; an image-less non-desktop app can't launch.
	imageTag := ""
	if kind != catalog.KindDesktop {
		switch {
		case entry.Containerized():
			tag, err := entry.ResolveTag(appVersion)
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			imageTag = tag
		case appVersion != "":
			return mcp.NewToolResultError(fmt.Sprintf("app_version is not supported for %s (not a containerized app)", entry.Name)), nil
		default:
			return mcp.NewToolResultError(fmt.Sprintf("no image configured for %s — launch it with the spawn CLI (supply --image or a catalog overlay)", entry.Name)), nil
		}
	}

	if region == "" {
		if client, err := spawnClient(ctx); err == nil {
			if cfg, cerr := client.GetConfig(ctx); cerr == nil && cfg.Region != "" {
				region = cfg.Region
			}
		}
	}
	if region == "" {
		region = "us-east-1"
	}

	var sb strings.Builder
	if !dryRun {
		sb.WriteString("PLAN ONLY — this MCP tool does not perform the real DCV/web launch.\n")
		sb.WriteString("Run it (human-gated) via the CLI:\n")
		fmt.Fprintf(&sb, "  spawn app launch %s --ttl %s --instance-type %s", entry.Name, ttl, instanceType)
		if region != "" {
			sb.WriteString(" --region " + region)
		}
		if appVersion != "" {
			sb.WriteString(" --app-version " + appVersion)
		}
		if webPort > 0 {
			fmt.Fprintf(&sb, " --web-port %d", webPort)
		}
		sb.WriteString("\n\n")
	} else {
		sb.WriteString("DRY RUN — nothing will be launched.\n\n")
	}
	fmt.Fprintf(&sb, "App:       %s — %s\n", entry.Name, entry.Description)
	fmt.Fprintf(&sb, "Kind:      %s\n", kind)
	fmt.Fprintf(&sb, "Region:    %s\n", region)
	fmt.Fprintf(&sb, "Instance:  %s\n", instanceType)
	if entry.Containerized() {
		fmt.Fprintf(&sb, "Image:     %s:%s\n", entry.Image, imageTag)
	}
	if kind == catalog.KindWeb {
		fmt.Fprintf(&sb, "Web port:  %d (TLS-proxied on :443, token-gated)\n", entry.Port)
	}
	fmt.Fprintf(&sb, "GPU:       %v\n", entry.GPU)
	if spawnclient.DetectGPUInstance(instanceType) {
		sb.WriteString("AMI:       AL2023 GPU DLAMI (NVIDIA driver; auto-selected for GPU instance)\n")
	} else {
		fmt.Fprintf(&sb, "AMI:       AL2023 base — %s (resolved via SSM; DCV installed at boot)\n", spawnclient.DetectArchitecture(instanceType))
	}
	fmt.Fprintf(&sb, "TTL:       %s\n", ttl)
	return mcp.NewToolResultText(strings.TrimRight(sb.String(), "\n")), nil
}
