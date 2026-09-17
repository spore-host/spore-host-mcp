package main

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/spore-host/libs/catalog"
	spawnclient "github.com/spore-host/spawn/pkg/aws"
	"github.com/spore-host/spawn/pkg/ecrref"
	"github.com/spore-host/spawn/pkg/launcher"
	"github.com/spore-host/spawn/pkg/taskproto"
	truffleaws "github.com/spore-host/truffle/pkg/aws"
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
			mcp.Description("The TaskSpec as a JSON object (string). Required fields: task_id, command (argv array), resources (cpu/memory_gib/gpus/…), and lifecycle.ttl (e.g. \"4h\"). See spawn's docs/workflow-adapter-protocol-rfc.md."),
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

// mcpTaskFinder adapts truffle's SearchInstanceTypes to taskproto.InstanceFinder,
// mirroring spawn's own truffleFinder (cmd/task.go): it searches one region and
// projects each result to a taskproto.Candidate, looking up the on-demand price
// explicitly (SearchInstanceTypes doesn't populate it) so the sizer ranks on
// real price rather than picking the largest type on a tie.
type mcpTaskFinder struct {
	tc     *truffleaws.Client
	region string
}

func (f mcpTaskFinder) FindCandidates(ctx context.Context, req taskproto.ResourceRequest) ([]taskproto.Candidate, error) {
	matcher := regexp.MustCompile(`.*`)
	opts := truffleaws.FilterOptions{
		Architecture: req.Architecture,
		MinVCPUs:     req.CPU,
		MinMemory:    taskproto.EffectiveMemoryGiB(req),
	}
	results, err := f.tc.SearchInstanceTypes(ctx, []string{f.region}, matcher, opts)
	if err != nil {
		return nil, err
	}
	cands := make([]taskproto.Candidate, 0, len(results))
	for _, r := range results {
		price := r.OnDemandPrice
		if price <= 0 {
			if p, perr := f.tc.OnDemandPrice(ctx, r.InstanceType, f.region); perr == nil {
				price = p
			}
		}
		cands = append(cands, taskproto.Candidate{
			InstanceType:  r.InstanceType,
			Family:        r.InstanceFamily,
			VCPUs:         int(r.VCPUs),
			MemoryGiB:     float64(r.MemoryMiB) / 1024,
			GPUs:          int(r.GPUs),
			Architecture:  r.Architecture,
			OnDemandPrice: price,
		})
	}
	return cands, nil
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

	// Placement storage (attached volumes / EFS / FSx) needs the CLI's boot-time
	// mount-script machinery; not wired here. Reject clearly rather than launch a
	// task whose storage won't mount.
	if len(spec.Placement.Volumes) > 0 || spec.Placement.EFSID != "" || spec.Placement.FSxLustreID != "" {
		return mcp.NewToolResultError("placement storage (volumes / efs_id / fsx_lustre_id) is not supported via MCP; launch this task with the spawn CLI: spawn task run --spec <file>"), nil
	}

	client, err := spawnClient(ctx)
	if err != nil {
		return mcp.NewToolResultError("failed to connect to AWS: " + err.Error()), nil
	}
	if region == "" {
		region = client.Config().Region
	}
	if region == "" {
		return mcp.NewToolResultError("no region: pass region or configure a default AWS region"), nil
	}

	finder := mcpTaskFinder{tc: truffleaws.NewClientFromConfig(client.Config()), region: region}

	if dryRun {
		return renderTaskPlan(ctx, spec, finder, region)
	}

	return runTaskReal(ctx, client, spec, finder, region)
}

// renderTaskPlan sizes the task and renders the launch plan WITHOUT launching —
// the dry_run path. Mirrors spawn's renderTaskDryRun output.
func renderTaskPlan(ctx context.Context, spec *taskproto.TaskSpec, finder taskproto.InstanceFinder, region string) (*mcp.CallToolResult, error) {
	sized, err := taskproto.Size(ctx, finder, spec.Resources)
	if err != nil {
		return mcp.NewToolResultError("size task: " + err.Error()), nil
	}

	var sb strings.Builder
	sb.WriteString("DRY RUN — nothing will be launched.\n\n")
	sb.WriteString(fmt.Sprintf("Task:      %s\n", spec.TaskID))
	sb.WriteString(fmt.Sprintf("Command:   %s\n", strings.Join(spec.Command, " ")))
	if spec.Container != "" {
		sb.WriteString(fmt.Sprintf("Container: %s\n", spec.Container))
	}
	sb.WriteString(fmt.Sprintf("Region:    %s\n", region))
	sb.WriteString(fmt.Sprintf("Instance:  %s  (%d vCPU, %.0f GiB", sized.InstanceType, sized.VCPUs, sized.MemoryGiB))
	if sized.OnDemandPrice > 0 {
		sb.WriteString(fmt.Sprintf(", $%.4f/hr on-demand", sized.OnDemandPrice))
	}
	sb.WriteString(fmt.Sprintf(")\n           chosen as cheapest of %d matching type(s)\n", sized.Considered))
	sb.WriteString(fmt.Sprintf("AMI:       %s\n", taskAMIPlan(spec, sized.InstanceType)))
	purchase := spec.Resources.Purchase
	if purchase == "" {
		purchase = taskproto.PurchaseOnDemand
	}
	sb.WriteString(fmt.Sprintf("Purchase:  %s\n", purchase))
	sb.WriteString(fmt.Sprintf("TTL:       %s   on-complete: %s\n", spec.Lifecycle.TTL, spec.EffectiveOnComplete()))
	if spec.Lifecycle.CostLimit > 0 {
		sb.WriteString(fmt.Sprintf("Cost limit: $%.2f\n", spec.Lifecycle.CostLimit))
	}
	if d, err := time.ParseDuration(spec.Lifecycle.TTL); err == nil && sized.OnDemandPrice > 0 {
		sb.WriteString(fmt.Sprintf("Max cost:  ~$%.2f (on-demand rate × TTL; a completed task usually costs far less)\n", sized.OnDemandPrice*d.Hours()))
	}
	sb.WriteString("\nRe-run with dry_run=false to launch this task.")
	return mcp.NewToolResultText(sb.String()), nil
}

// runTaskReal launches a task for real: ensures the per-account results bucket,
// generates the on-instance wrapper, creates a scoped instance profile, and
// launches via launcher.Provision (keyless/SSM). Reproduces spawn's runTaskReal
// (cmd/task.go) using its exported library surface; placement storage is handled
// separately (rejected by the caller for MCP).
func runTaskReal(ctx context.Context, client *spawnclient.Client, spec *taskproto.TaskSpec, finder taskproto.InstanceFinder, region string) (*mcp.CallToolResult, error) {
	sized, err := taskproto.Size(ctx, finder, spec.Resources)
	if err != nil {
		return mcp.NewToolResultError("size task: " + err.Error()), nil
	}

	account, err := client.GetAccountID(ctx)
	if err != nil {
		return mcp.NewToolResultError("resolve account id: " + err.Error()), nil
	}
	resultsBucket := fmt.Sprintf("spawn-results-%s-%s", account, region)
	if err := client.CreateS3BucketIfNotExists(ctx, resultsBucket, region); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("ensure results bucket %s: %v", resultsBucket, err)), nil
	}

	wrapper := taskproto.GenerateWrapper(spec, resultsBucket, region)

	profile, err := client.CreateOrGetInstanceProfile(ctx, spawnclient.IAMRoleConfig{
		TrustServices:    []string{"ec2"},
		InlinePolicyJSON: taskStagingPolicy(s3Buckets(spec.Inputs), s3Buckets(spec.Outputs), resultsBucket, s3BucketsFromURIs(spec.Resources.S3ReadWrite)),
		Policies:         taskExtraPolicies(spec),
	})
	if err != nil {
		return mcp.NewToolResultError("create task instance profile: " + err.Error()), nil
	}

	cfg := taskLaunchConfig(spec, sized, region, profile, wrapper)

	auditLog("spawn_task_run", spec.TaskID, "", region)

	result, err := launcher.Provision(ctx, client, cfg, launcher.Options{})
	if err != nil {
		return mcp.NewToolResultError("launch task: " + err.Error()), nil
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("✅ Task launched: %s\n", spec.TaskID))
	sb.WriteString(fmt.Sprintf("Instance:   %s  (%s) in %s / %s\n", result.InstanceID, sized.InstanceType, result.Region, orDash(result.AvailabilityZone)))
	sb.WriteString(fmt.Sprintf("TTL:        %s   on-complete: %s\n", spec.Lifecycle.TTL, spec.EffectiveOnComplete()))
	sb.WriteString(fmt.Sprintf("Completion: s3://%s/tasks/%s/completion.json\n", resultsBucket, spec.TaskID))
	sb.WriteString(fmt.Sprintf("\nPoll for completion:\n  spawn task status %s --region %s\n", spec.TaskID, region))
	return mcp.NewToolResultText(sb.String()), nil
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// taskAMIPlan mirrors spawn's offline AMI classification (cmd/task.go): an
// explicit AMI wins; a GPU family gets the NVIDIA DLAMI; everything else the
// standard AL2023 for the architecture. Uses the same exported classifiers the
// real resolver (GetRecommendedAMI) uses, so the preview can't drift.
func taskAMIPlan(spec *taskproto.TaskSpec, instanceType string) string {
	if ami := strings.TrimSpace(spec.Placement.AMI); ami != "" {
		return ami + "  (explicit: placement.ami)"
	}
	arch := spawnclient.DetectArchitecture(instanceType)
	if spawnclient.DetectGPUInstance(instanceType) {
		return fmt.Sprintf("AL2023 GPU DLAMI — NVIDIA driver, %s (auto-selected for GPU instance %s)", arch, instanceType)
	}
	return fmt.Sprintf("AL2023 default — %s (auto-selected)", arch)
}

// taskLaunchConfig builds the aws.LaunchConfig for a task, reproducing spawn's
// cmd/task.go taskLaunchConfig. AMI / UserData / KeyName are left for
// launcher.Provision to fill (auto AMI, keyless/SSM, wrapper-as-command).
func taskLaunchConfig(spec *taskproto.TaskSpec, sized *taskproto.SizeResult, region, iamProfile, wrapper string) spawnclient.LaunchConfig {
	cfg := spawnclient.LaunchConfig{
		InstanceType:       sized.InstanceType,
		Region:             region,
		JobArrayCommand:    wrapper,
		Spot:               spec.Resources.Purchase == taskproto.PurchaseSpot,
		TTL:                spec.Lifecycle.TTL,
		OnComplete:         spec.EffectiveOnComplete(),
		CompletionFile:     "/tmp/SPAWN_COMPLETE",
		CompletionDelay:    "10s",
		Name:               spec.TaskID,
		IamInstanceProfile: iamProfile,
		Tags:               map[string]string{"spawn:task-id": spec.TaskID},
	}
	cfg.AMI = spec.Placement.AMI
	cfg.AvailabilityZone = spec.Placement.AvailabilityZone
	if spec.Resources.DiskGiB > 0 {
		cfg.RootVolumeSizeGiB = spec.Resources.DiskGiB
	}
	if spec.Lifecycle.CostLimit > 0 {
		cfg.CostLimit = spec.Lifecycle.CostLimit
	}
	return cfg
}

// taskExtraPolicies returns the extra IAM policy-template names a task needs: a
// private-ECR container image needs ecr:ReadOnly to pull. Mirrors cmd/task.go.
func taskExtraPolicies(spec *taskproto.TaskSpec) []string {
	if spec.Container != "" && ecrref.Account(spec.Container) != "" {
		return []string{"ecr:ReadOnly"}
	}
	return nil
}

// taskStagingPolicy builds a scoped S3 policy granting read on the input buckets
// and write on the output + results buckets, plus full read-write on any
// s3_read_write buckets. Reproduces spawn's cmd/task.go taskStagingPolicy.
func taskStagingPolicy(inputBuckets, outputBuckets []string, resultsBucket string, readWriteBuckets []string) string {
	readB := dedupeBuckets(inputBuckets)
	writeB := dedupeBuckets(append(append([]string{}, outputBuckets...), resultsBucket))
	rwB := dedupeBuckets(readWriteBuckets)

	var stmts []string
	if len(readB) > 0 {
		stmts = append(stmts, fmt.Sprintf(`{"Effect":"Allow","Action":["s3:GetObject","s3:GetObjectVersion"],"Resource":[%s]}`, bucketObjectARNs(readB)))
		stmts = append(stmts, fmt.Sprintf(`{"Effect":"Allow","Action":["s3:ListBucket","s3:GetBucketLocation"],"Resource":[%s]}`, bucketARNs(readB)))
	}
	stmts = append(stmts, fmt.Sprintf(`{"Effect":"Allow","Action":["s3:PutObject"],"Resource":[%s]}`, bucketObjectARNs(writeB)))
	if len(rwB) > 0 {
		stmts = append(stmts, fmt.Sprintf(`{"Effect":"Allow","Action":["s3:GetObject","s3:GetObjectVersion","s3:PutObject","s3:DeleteObject"],"Resource":[%s]}`, bucketObjectARNs(rwB)))
		stmts = append(stmts, fmt.Sprintf(`{"Effect":"Allow","Action":["s3:ListBucket","s3:GetBucketLocation"],"Resource":[%s]}`, bucketARNs(rwB)))
	}
	return `{"Version":"2012-10-17","Statement":[` + strings.Join(stmts, ",") + `]}`
}

func s3Buckets(manifests []taskproto.Manifest) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range manifests {
		for _, ep := range []string{m.Source, m.Destination} {
			if b := s3Bucket(ep); b != "" && !seen[b] {
				seen[b] = true
				out = append(out, b)
			}
		}
	}
	return out
}

func s3BucketsFromURIs(uris []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, u := range uris {
		if b := s3Bucket(u); b != "" && !seen[b] {
			seen[b] = true
			out = append(out, b)
		}
	}
	return out
}

func s3Bucket(uri string) string {
	const p = "s3://"
	if !strings.HasPrefix(uri, p) {
		return ""
	}
	rest := uri[len(p):]
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		return rest[:i]
	}
	return rest
}

func dedupeBuckets(buckets []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, b := range buckets {
		if b != "" && !seen[b] {
			seen[b] = true
			out = append(out, b)
		}
	}
	return out
}

func bucketARNs(buckets []string) string {
	arns := make([]string, len(buckets))
	for i, b := range buckets {
		arns[i] = fmt.Sprintf("%q", "arn:aws:s3:::"+b)
	}
	return strings.Join(arns, ",")
}

func bucketObjectARNs(buckets []string) string {
	arns := make([]string, len(buckets))
	for i, b := range buckets {
		arns[i] = fmt.Sprintf("%q", "arn:aws:s3:::"+b+"/*")
	}
	return strings.Join(arns, ",")
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
		sb.WriteString(fmt.Sprintf("  spawn app launch %s --ttl %s --instance-type %s", entry.Name, ttl, instanceType))
		if region != "" {
			sb.WriteString(" --region " + region)
		}
		if appVersion != "" {
			sb.WriteString(" --app-version " + appVersion)
		}
		if webPort > 0 {
			sb.WriteString(fmt.Sprintf(" --web-port %d", webPort))
		}
		sb.WriteString("\n\n")
	} else {
		sb.WriteString("DRY RUN — nothing will be launched.\n\n")
	}
	sb.WriteString(fmt.Sprintf("App:       %s — %s\n", entry.Name, entry.Description))
	sb.WriteString(fmt.Sprintf("Kind:      %s\n", kind))
	sb.WriteString(fmt.Sprintf("Region:    %s\n", region))
	sb.WriteString(fmt.Sprintf("Instance:  %s\n", instanceType))
	if entry.Containerized() {
		sb.WriteString(fmt.Sprintf("Image:     %s:%s\n", entry.Image, imageTag))
	}
	if kind == catalog.KindWeb {
		sb.WriteString(fmt.Sprintf("Web port:  %d (TLS-proxied on :443, token-gated)\n", entry.Port))
	}
	sb.WriteString(fmt.Sprintf("GPU:       %v\n", entry.GPU))
	if spawnclient.DetectGPUInstance(instanceType) {
		sb.WriteString("AMI:       AL2023 GPU DLAMI (NVIDIA driver; auto-selected for GPU instance)\n")
	} else {
		sb.WriteString(fmt.Sprintf("AMI:       AL2023 base — %s (resolved via SSM; DCV installed at boot)\n", spawnclient.DetectArchitecture(instanceType)))
	}
	sb.WriteString(fmt.Sprintf("TTL:       %s\n", ttl))
	return mcp.NewToolResultText(strings.TrimRight(sb.String(), "\n")), nil
}
