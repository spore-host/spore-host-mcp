# Changelog

All notable changes to **spore-host-mcp** (the MCP server for AI-assistant
access to spore.host tooling) are documented here.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.42.0] - 2026-10-20

### Added

- **`spawn_status` reports how a task ended, even after its instance is gone**
  (#63). When no instance matches, it reads the task's completion record from
  `s3://spawn-results-<account>-<region>/tasks/<task_id>/completion.json` and
  reports the state, exit code, why the task ended (from `retry_class` and
  `terminal_reason`: a non-zero exit, a staging or output-delivery failure, a
  TTL or cost-limit kill, and so on), the start and end times, the wall time
  and the cost. The cost is labelled an estimate of compute only: the record's
  own `cost_estimate` when it has one, otherwise the instance type's on-demand
  rate times the wall time while EC2 still lists the instance, with a static
  rate labelled as static, with its collection date and the reason. A
  terminated or running task instance gets the same task section. A new
  optional `results_prefix` argument finds the record of a task launched with a
  TaskSpec `results_prefix`, as `spawn task status --results-prefix` does. Only
  when there is neither an instance nor a record does it answer
  `no instance or task record named "…"`.
- **`spawn_status` prices a finished task whose instance EC2 no longer lists**
  (spawn#1260). A record written by spawn v0.139.0 or later names the
  instance type and purchase option, so the cost is now that type's rate times
  the wall time instead of "not derivable": the Spot rate for a Spot task (the
  highest current Spot price across the region's AZs, labelled as today's
  price, since the record names no AZ), the on-demand rate otherwise, with a
  static rate labelled as static with its collection date and the reason. When
  no Spot rate is found it falls back to on-demand and says that overstates it.
  It stays labelled an estimate of compute only. A record from an older spawn
  has neither field and is reported as before: its `cost_estimate`, else not
  derivable.

### Changed

- **Bumped spawn v0.131.0 → v0.139.0**, for the public `pkg/tasklaunch`
  (spawn#1255) that `spawn_task_run` now launches through, and for its shared
  completion-record reader and the record's `instance_type` and `purchase`
  fields (spawn#1260).
- **`spawn_status` reads completion records through spawn's one reader,
  `tasklaunch.ReadCompletion`** (spawn#1260), in place of this server's own
  copy of the resolve-prefix, build-key, fetch and parse chain, so the key it
  reads cannot drift from the one the wrapper writes (the spawn#715 class). A
  missing record is reported as before, with the location looked at.
- **`spawn_task_run`'s dry run is spawn's own plan** (`tasklaunch.Plan` and
  `Render`, the output of `spawn task run --dry-run`), so it now also shows the
  root disk, `/dev/shm`, the user-data size against EC2's limit, inputs and
  outputs, the container-floor note, and any warning or static-price label.
- **A successful `spawn_task_run` reports the purchase option that launched
  and the run ID**, and the follow-up `spawn task status` command carries
  `--results-prefix` when the spec set one.

### Fixed

- **`truffle_find` accepts an exact instance type** (truffle#263). `{"query": "r8g.2xlarge"}` used to fail with "unknown term" although the truffle CLI found it; truffle v0.62.0 moved the CLI's pattern handling into the library this tool calls. The bump also brings EFA support read from EC2 (truffle#253) and libs v0.51.2.
- **`spawn_task_run` falls back to On-Demand when Spot has no capacity** (#61).
  It called `launcher.Provision` once with an empty `launcher.Options{}`, so a
  spec with `resources.fallback: "on_demand"` failed on
  `InsufficientInstanceCapacity` instead of retrying, and every launch went out
  without the terminal-flush hook that writes a completion record on a TTL kill
  (the spawn#1144 class). It now launches through spawn's `tasklaunch.Launch`,
  which retries once On-Demand with the same, populated options.
- **`spawn_task_run` sizes a container task to the container memory floor**
  (#60). It sized with `spec.Resources`, bypassing spawn#1180's 2 GiB floor, so
  a CPU-only container task could land on a 0.5 GiB box where the boot-time
  Docker install is OOM-killed. Sizing is now spawn's, with
  `spec.SizingRequest()`, on both the dry run and the real launch.
- **`spawn_task_run`'s completion record reaches S3** (#64). The wrapper was
  given a bare bucket name as its results prefix, so it wrote
  `completion.json`, `.exitcode` and `command.log` to the instance's local disk,
  deleted at termination, while the tool reported an S3 path nothing wrote. The
  prefix is now spawn's resolved `EffectiveResultsPrefix`, the spec's
  `results_prefix` is honoured, only spawn's own default bucket is ever
  created, and the reported `Completion:` path is the one the wrapper writes.
- **`spawn_list state=all` includes terminated instances again** (#63). It
  mapped `all` to spawn's `""`, which since spawn#527 means non-terminated
  only, so a task vanished from the list the moment its instance terminated.
  It now passes spawn's `"all"`.
- **`spawn_status` finds a recently terminated instance by name or ID** (#63).
  It searched non-terminated instances only, so a finished task read as a typo.
  `spawn_stop`, `spawn_terminate` and `spawn_extend` still resolve against
  non-terminated instances only, so an old terminated instance sharing a name
  can neither make a live one's name ambiguous nor be acted on.
- **The handler tests no longer reach AWS.** The `spawn_*` smoke tests called
  the real AWS API whenever the environment had credentials; every spawn tool
  test now runs against an in-process fake.

## [0.41.0] - 2026-10-17

### Security

- **Rebuilt against Go 1.26.9 and `golang.org/x/net` v0.60.0** for the Go
  vulnerabilities disclosed 2026-10-09: GO-2026-6605, 6607, 6608, 6610, 6611,
  6612, 6613 and 6617, across `net/http`, `crypto/tls`, `mime/multipart`,
  `net/http/httputil`, `net/textproto` and x/net's HTTP/2 implementation. This
  server reaches several of them through `server.ServeStdio`, so they were
  reported as called rather than merely present. The CI Go pin moves
  1.26.8 → 1.26.9 in all three workflows.

### Fixed

- **`truffle_find` explains a query it cannot read instead of failing with
  `parse query: ...`** (#56). A query of only unknown words (`zzqx`,
  `gravitn xlrage`) used to search and price the whole catalog; truffle now
  refuses it (truffle#229), and `truffle_find` returns truffle's own message,
  which names the words, suggests close ones and lists the kinds of term it
  understands. A query that runs but ignored a word now says which, a bare size
  in a GPU query says it was read as per-GPU memory, and a query whose
  constraints exclude each other (`intel h100`) says so when nothing matches.
- **CI now runs golangci-lint, and the 107 findings it reported are fixed**
  (#54). All were staticcheck QF1012 (`sb.WriteString(fmt.Sprintf(...))`),
  now `fmt.Fprintf(&sb, ...)` across `lagotto.go`, `launch.go`, `spawn.go` and
  `truffle.go`; tool output is byte-for-byte unchanged. A new `Lint` CI job
  installs golangci-lint 2.14.0, verified against a pinned SHA256, and runs
  `make lint` from a new `Makefile` (which also provides `build`, `test`, `vet`
  and the `changelog-fix` target `CLAUDE.md` already referred to).
  `TestCIGatesLint` fails if the gate, the version pin or the checksum check is
  removed, or if CI's version drifts from the Makefile's `GOLANGCI_LINT_VERSION`.

- **CI action pins now say which version they actually run** (#52).
  `actions/checkout@3d3c42e…` was commented `# v6` in every workflow but is
  v7.0.1; likewise `goreleaser-action` `# v7` is v7.2.3 and the `trivy-action`
  pin commented `# master (pinned)` is v0.36.0. The SHAs were right and are
  unchanged; only the comments were wrong — but those comments are what
  reviewers read and what Dependabot rewrites. A new `workflow_pins_test.go` now
  fails on any `uses:` not pinned to a full SHA with a `# vX.Y.Z` comment, and
  checks each comment against `scripts/action-pins.txt`, a table of what every
  pinned SHA really is, regenerated from GitHub by
  `scripts/refresh-action-pins.sh` (run it, and commit the table, whenever a pin
  changes — including on a Dependabot bump).
- **Task launches now stamp a run identity and detect a GPU from the sized instance
  type.** Both were missing because this repo was pinned to `spawn` v0.111.0 and could
  not move: `taskproto.GenerateWrapper` gained `gpu bool` in spawn **v0.111.1** — a
  breaking signature change in a PATCH release, filed as spawn#679 — and `runID string`
  in v0.112.0. A Dependabot bump had therefore been failing CI for six days, with
  nothing pointing at the cause.
  The consequences of the two missing arguments are not cosmetic. An empty `run_id`
  reads downstream as **unattributable**: re-running a `task_id` overwrites the same
  `tasks/<task_id>/completion.json` key, so a record left by a previous attempt becomes
  indistinguishable from this one's, and a waiter reports an already-fixed task as still
  failing (spawn#608). And `gpu=false` on a GPU instance silently drops `--gpus all`
  from the container run, so a workload sees no GPU on hardware that has one
  (spawn#601/#606).
  Both values are positional arguments that compile fine when wrong, so they are now
  produced by a named `wrapperIdentity` helper with a test for each: that the run id is
  non-empty **and differs between attempts**, and that GPU-ness follows the sized
  instance type rather than the spec (a task may ask only via `families` and still be
  sized onto a GPU box).

### Changed

- **Builds against spawn v0.131.0, truffle v0.59.0, lagotto v0.64.0 and libs
  v0.51.1** (from spawn v0.126.1, truffle v0.58.0, lagotto v0.63.1 and libs
  v0.50.0). lagotto v0.64.0's one breaking change, `ListMatchHistory` requiring
  a watch ID, does not affect spore-host-mcp, which does not call it.
- **Dependencies caught up to current**: `spawn` 0.111.0 → 0.126.1, `mcp-go` 1.0.0 →
  1.1.1, `libs` 0.49.0 → 0.50.0, `truffle` 0.56.0 → 0.58.0, plus the transitive AWS
  SDK and protobuf updates.

- **Migrated to `taskproto.WrapperOptions`** (spawn#764, this repo's #48).
  `taskproto.GenerateWrapper` now takes an options struct and returns an error
  instead of five positional arguments, so the call in `launch.go` was updated and
  the error is surfaced as a tool error. No behaviour change: the run id and GPU
  flag `wrapperIdentity` already computed are passed through as named fields.
  This closes the loop on spawn#679, which was filed *because of this repo* — a
  positional signature change had made `GenerateWrapper(spec, bucket, region,
  false, "")` the quickest way to recompile, which emitted an empty `run_id` and
  dropped `--gpus all` while passing every test. The new signature makes that
  exact edit impossible: an omitted `RunID` is now an error, not a silent default.

### Added

- **The changelog policy is now enforced in CI rather than by habit.** A PR that changes
  non-test Go source without touching `CHANGELOG.md` fails, and `changelog_test.go`
  checks `[Unreleased]` for duplicate group headings, unknown group names, entries
  outside a group, and releases missing a compare link.
  This policy has been suite-wide for a while but only `spawn` enforced it — where it
  immediately earned its keep, catching a duplicate `### Fixed` **four times in one
  session** and a PR that had merged with no entry at all (found only at the next
  release, against an empty `[Unreleased]`, with the entries reconstructed from the diff
  at tag time).
  `scripts/changelog-consolidate.py` (and `make changelog-fix` where there's a Makefile)
  merges duplicate groups mechanically, because two PRs each adding their own
  `### Fixed` is a routine conflict that merges cleanly for git and badly for the format
  — not a mistake worth hand-fixing each time.

## [0.40.0] - 2026-09-16

### Added
- **lagotto tools** (read + create), so an assistant can inspect and start
  capacity watches:
  - `lagotto_list` — list the caller's watches, surfacing each watch's project,
    owner (short ARN), status, instance-type pattern, regions, action, spot, and
    the derived **wait-to-acquire** / **time-to-give-up** (v0.58.0 #139). Optional
    `project` filter (reuses the poller's `WatchFilter`); `all` shows every state
    (default: active only). Read-only, owner-scoped.
  - `lagotto_status` — full details for one watch id (project/owner, pattern,
    regions, spot/max-price, status, timings, last match). Owner-scoped: a watch
    the caller doesn't own returns the same "not found" as a missing one (no
    existence oracle), mirroring the lagotto CLI.
  - `lagotto_watch` — create a capacity watch. Implements the `notify` and `hold`
    actions (pattern, regions, project, ttl, spot, max_price, notify channels);
    auto-creates the backing DynamoDB tables on first use like the CLI. `action=
    spawn` is deferred (it needs a full spawn launch-config) and rejected with a
    pointer to the CLI; SageMaker/fleet/AZ knobs are also out of scope for the tool.
- **spawn launch tools** — these create real, **BILLABLE** EC2 instances, so both
  carry cost guardrails: a **TTL is mandatory** (the call is rejected if the
  spec/params carry none) and a **`dry_run`** flag resolves/plans without launching.
  The billable nature is stated in each tool's description.
  - `spawn_task_run` — launch a task from a TaskSpec (JSON). Parsing enforces
    `lifecycle.ttl`; `dry_run=true` sizes the cheapest fitting instance and prints
    the plan (no launch). A real launch reproduces `spawn task run` via the exported
    library surface (results bucket, on-instance wrapper, scoped S3 instance
    profile, `launcher.Provision`). Placement storage (attached volumes / EFS / FSx)
    is deferred to the CLI.
  - `spawn_app_launch` — resolve and plan an app launch (kind application /
    desktop / web) from the catalog: validates the app, kind/web-port, instance
    type, base AMI, and the mandatory TTL, and previews the plan. The real
    DCV/web/session launch is intentionally deferred to the human-gated `spawn app
    launch` CLI (its orchestration lives in spawn's unexported command layer);
    `dry_run` and non-dry-run both return a plan.

### Changed
- Bumped `github.com/spore-host/spawn` (0.110.0 → **0.111.0**) and added
  `github.com/spore-host/lagotto` **0.58.0** as a new dependency for the lagotto
  tools (pulling in `libs/catalog` for `spawn_app_launch` and refreshed AWS SDK v2
  service modules transitively).
- Migrated `github.com/mark3labs/mcp-go` from v0.58.0 to **v1.0.0** (the major
  release adding support for the 2026-07-28 MCP specification, supersedes
  Dependabot #38). The v1.0.0 API surface used by this server — `MCPServer`,
  `AddTool`, `NewTool`/`WithString`/`WithBoolean`/`Description`/`Required`/
  `DefaultString`/`DefaultBool`, `CallToolRequest.GetArguments`,
  `NewToolResultText`/`NewToolResultError`, `ServeStdio` — is source-compatible
  with v0.58.0, so no handler code changed. All 8 tools
  (`spawn_{list,status,stop,terminate,extend}`, `truffle_{find,quota_check,spot_prices}`)
  continue to work unchanged.
- Dependency maintenance: bumped `github.com/spore-host/spawn` (0.102.0 →
  0.110.0) and `github.com/spore-host/truffle` (0.53.0 → 0.56.0) to the current
  suite releases, pulling in `libs` v0.49.0 and refreshed AWS SDK v2 modules
  (indirect) transitively. No spawn/truffle API changes affected the server;
  no behavior change.

## [0.39.3] - 2026-09-09

### Security
- Bumped `google.golang.org/grpc` v1.83.1 → v1.83.2 (indirect), fixing
  CVE-2026-84445 (HIGH) — a gRPC-Go xDS-server DoS via crash. Trivy flagged
  v1.83.1, itself a prior grpc CVE fix.

### Changed
- Dependency maintenance: bumped `github.com/mark3labs/mcp-go` (0.57.0 →
  0.58.0), `github.com/spore-host/spawn` (0.98.0 → 0.102.0) and
  `github.com/spore-host/truffle` (0.48.1 → 0.53.0). Adapted the `truffle_find`
  handler to truffle's new `ParsedQuery.BuildCriteria(includeAZs bool)`
  signature (truffle#141), passing `true` to preserve the prior
  availability-zone lookup behavior. No behavior change to the server.

## [0.39.2] - 2026-08-11

### Changed
- Dependency maintenance: bumped `github.com/mark3labs/mcp-go` (0.49.0 →
  0.57.0) and 5 GitHub Actions to their current releases (no API or behavior
  change).

## [0.39.1] - 2026-08-07

### Changed
- Bumped the `spawn` (v0.77.0 → v0.98.0) and `truffle` (v0.44.0 → v0.48.1)
  dependencies to the current suite releases, pulling in `libs` v0.43.3
  (indirect) transitively. No behavior change to the server.
- CI moved off the self-hosted orion runner fleet onto `ubuntu-latest`. The
  fleet (colima/Docker on orion.local) is being decommissioned org-wide; no
  behavior change to the tool.

### Security
- **Added a Dependabot config, so the SHA-pinned actions and Go deps get bumped (#28).**
  Every action here is pinned to a commit SHA, which closes the mutable-tag hole
  but opens a staleness one: a SHA never moves — including past a security fix —
  and unlike `@v6` nothing updates it for you. Pinning is only safe if something
  bumps the pins, and nothing did. `actions/checkout@v6` had already moved
  upstream while this repo went on pinning the older commit, silently.

  This matters most for `release.yaml`, which pins the release-signing actions
  (`goreleaser-action`, `cosign-installer`, `attest-build-provenance`) — the
  supply-chain machinery. A frozen `cosign-installer` means releases keep getting
  signed by an old cosign, and cosign 3.x already changed its CLI surface.

  Weekly, grouped, with a 7-day cooldown so a freshly-published tag isn't proposed
  the day it ships. The actions group pattern is `*` rather than `actions/*`
  precisely because those signing actions aren't under `actions/`. Two tests
  enforce coverage — every action in every workflow must be matched by a group
  pattern, and every `go.mod` must be watched — so adding one without wiring it up
  fails CI instead of going unnoticed. CI-only; no change to the server.

### Changed
- **CI now fails on unformatted code (#26).** Nothing did before: the workflow had
  no formatting step, and this repo has no Makefile, so there wasn't even a local
  `fmt` target anyone might run. The tree happened to be clean; the gate is what
  keeps it that way. (The sibling repos drifted for months under the same
  configuration — spawn had 3 files unformatted on `main`, truffle 7.)

  The gate reports drift instead of fixing it, with offenders listed and diffed,
  because `gofmt -w` rewrites files and exits 0 — a check built on it could only
  ever report success. No behavior change; the server is untouched.

### Documentation
- README now documents the `spawn_terminate` two-phase `confirm=true` flow and
  its ambiguous-name refusal (the server's core safety behavior, previously only
  in the in-protocol tool description), the deliberate no-launch boundary, and
  the credential/config options (`SPORE_PROFILE`/`SPORE_REGION` +
  `~/.config/spore/config.toml`) added with the shared config base.

## [0.39.0] - 2026-07-22

### Added
- `truffle_find` now shows a **"Why:"** line per result — human-readable match
  reasons (e.g. `GPU family: NVIDIA; Memory: 1152 GiB >= 40 GiB`) from truffle's
  `ExplainMatch` — so an assistant can see why each type matched, not just its
  specs.

### Changed
- `truffle_find` caps output at the top 25 matches (in truffle's preference
  order) and flags truncation with a "narrow the query" hint. A broad query
  ("gpu", "arm64") previously rendered every type×region row, bloating the tool
  result and the spot-price fetch.

## [0.38.0] - 2026-07-22

### Added
- `spawn_status` now shows the absolute reap **Deadline** (from the authoritative
  `spawn:ttl-deadline` tag) and how far off it is, so the actual death time is
  visible rather than inferred from the relative TTL (#21).
- Unit tests for the two safety-critical behaviors that previously had none (#21):
  `spawn_terminate`/`spawn_stop` **ambiguous-name refusal** (`selectInstance`) and
  `spawn_extend`'s **deadline-forward + expired-deadline floor**
  (`computeExtendedDeadline`), both now pure/AWS-free and directly tested.

### Fixed
- **`truffle_spot_prices` and `truffle_quota_check` crashed the entire server**
  (#21). Both passed a `nil` matcher to truffle's `SearchInstanceTypes`, which
  dereferences it inside a per-region goroutine (`extractSpecificTypes`) → SIGSEGV.
  Because this is an in-process stdio server, that panic killed the whole process
  and disconnected **all** tools, not just the failing call. Both now pass an
  anchored, literal single-type matcher. As a bonus this lets truffle use the
  `DescribeInstanceTypes` API-side type filter instead of enumerating every
  instance type in the region, so these tools are also much faster.

### Changed
- `truffle_spot_prices` / `truffle_quota_check` look up the requested type via an
  API-side filter (anchored matcher) rather than a full region enumeration (#21).

## [0.37.0] - 2026-07-22

### Security
- **Bump `google.golang.org/grpc` → 1.82.1** (indirect) — resolves
  GHSA-hrxh-6v49-42gf (gRPC-Go xDS RBAC / HTTP/2, HIGH).
- **Release artifacts are now signed** with keyless cosign (Sigstore) + SLSA build
  provenance (#19). The release signs `checksums.txt` (which lists every
  archive/package hash) with the workflow's GitHub OIDC identity — no long-lived
  key — publishing `checksums.txt.bundle`, and attests build provenance. Verify a
  download with `cosign verify-blob --bundle` (see docs: "Verify a download").
  Takes effect from the next tagged release.

### Added
- **Honors the shared spore.host config base.** Bumped spawn (v0.76.0 → v0.77.0)
  and truffle (v0.43.0 → v0.44.0), which adopted `libs/sporeconfig`. The MCP
  server now resolves the AWS profile/region the same way as the rest of the
  suite — `SPORE_PROFILE`/`SPORE_REGION` (and `AWS_PROFILE`/`AWS_REGION`) plus the
  `[spore]` table of `~/.config/spore/config.toml` — through spawn's and truffle's
  client constructors, with no MCP-side config code. Unset = unchanged (ambient
  AWS chain).

### Fixed
- `spawn_list` with `state=all` now works (#12). The value was passed straight
  through as an EC2 `instance-state-name` filter — no such state, so it matched
  nothing and hid stopped (still-EBS-billing) instances. `all` now maps to "no
  state filter", and the state arg is validated against `running`/`stopped`/`all`.

### Changed
- Bumped the `spawn` (v0.36.1 → v0.76.0) and `truffle` (v0.36.1 → v0.43.0)
  dependencies up to the current suite; they had drifted well behind (#13).

### Security
- Pinned all GitHub Actions to commit SHAs (were mutable `@vN`/`@master` tags),
  matching the suite-wide convention and clearing the Semgrep
  mutable-action-tag findings.

### Added
- Test coverage for the `spawn_extend`/`spawn_status`/`spawn_stop` lifecycle
  handlers (#13): `spawn_extend` rejects a non-Go-duration TTL like `7d` before any
  AWS call, and status/stop have smoke coverage that they return cleanly without
  panicking. Previously none of the spawn lifecycle handlers were tested.

### Documentation
- Corrected the false "no credentials required" claim on the truffle tools — they
  call the EC2 and Service Quotas APIs and require AWS credentials (#13).
- `spawn_extend`'s `ttl` description no longer advertises `7d`: the handler (like
  the spawn CLI's `extend`) parses Go duration units only, so `7d` was rejected;
  the description now says h/m/s (e.g. `168h` for a week) (#13).

### Security
- `spawn_terminate` is no longer a one-shot destructive call (#12). It now
  requires `confirm=true` (the first call previews the exact instance), refuses
  an **ambiguous name** that matches more than one instance (requires the
  instance ID), and audit-logs every termination to stderr — matching the CLI.
  `findInstance` previously returned the first case-insensitive name match, so
  an ambiguous name could silently terminate an arbitrary instance.

### Documentation
- README "Tools exposed" list now matches the registered tools: `truffle_find`,
  `truffle_spot_prices`, `truffle_quota_check`, `spawn_list`, `spawn_status`,
  `spawn_stop`, `spawn_terminate`, `spawn_extend`. Removes tools that were never
  registered (`truffle_search`, `spawn_launch`, `spawn_connect`) and fixes
  `truffle_spot` → `truffle_spot_prices`.

### Security
- Semgrep SAST is now **enforcing** in CI (`--config=auto --error`) rather than
  report-only (#368). The scan was already clean — no findings to triage.

### Fixed
- `spawn_extend` now writes the authoritative `spawn:ttl-deadline` tag (pushing
  the absolute deadline forward, anchored to launch), not just `spawn:ttl`.
  Previously the extend was a **silent no-op** — spored ignores `spawn:ttl` for
  current instances, so the instance still terminated at its original deadline
  despite the "✅ TTL updated" message. Mirrors the `spawn extend` CLI (#11).
- `spawn_extend` floors the new deadline at `now + requested-duration`, so an
  already-expired `spawn:ttl-deadline` can't set a deadline in the past and reap
  the instance the moment you extend it (spore-host#374).
- Clarified the `spawn_extend` invalid-TTL error message (it garbled the
  day/week-suffix guidance).

## [0.36.1]

Baseline. Earlier history is in the
[GitHub Releases](https://github.com/spore-host/spore-host-mcp/releases) and the
[commit log](https://github.com/spore-host/spore-host-mcp/commits/main).

---

[Unreleased]: https://github.com/spore-host/spore-host-mcp/compare/v0.42.0...HEAD
[0.42.0]: https://github.com/spore-host/spore-host-mcp/compare/v0.41.0...v0.42.0
[0.41.0]: https://github.com/spore-host/spore-host-mcp/compare/v0.40.0...v0.41.0
[0.40.0]: https://github.com/spore-host/spore-host-mcp/compare/v0.39.3...v0.40.0
[0.39.3]: https://github.com/spore-host/spore-host-mcp/compare/v0.39.2...v0.39.3
[0.39.2]: https://github.com/spore-host/spore-host-mcp/compare/v0.39.1...v0.39.2
[0.39.1]: https://github.com/spore-host/spore-host-mcp/compare/v0.39.0...v0.39.1
[0.39.0]: https://github.com/spore-host/spore-host-mcp/compare/v0.38.0...v0.39.0
[0.38.0]: https://github.com/spore-host/spore-host-mcp/compare/v0.37.0...v0.38.0
[0.37.0]: https://github.com/spore-host/spore-host-mcp/compare/v0.36.1...v0.37.0
[0.36.1]: https://github.com/spore-host/spore-host-mcp/releases/tag/v0.36.1
