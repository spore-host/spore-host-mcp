# Changelog

All notable changes to **spore-host-mcp** (the MCP server for AI-assistant
access to spore.host tooling) are documented here.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Fixed
- `spawn_list` with `state=all` now works (#12). The value was passed straight
  through as an EC2 `instance-state-name` filter — no such state, so it matched
  nothing and hid stopped (still-EBS-billing) instances. `all` now maps to "no
  state filter", and the state arg is validated against `running`/`stopped`/`all`.

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

[Unreleased]: https://github.com/spore-host/spore-host-mcp/compare/v0.36.1...HEAD
[0.36.1]: https://github.com/spore-host/spore-host-mcp/releases/tag/v0.36.1
