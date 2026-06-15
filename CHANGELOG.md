# Changelog

All notable changes to **spore-host-mcp** (the MCP server for AI-assistant
access to spore.host tooling) are documented here.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Fixed
- `spawn_extend` now writes the authoritative `spawn:ttl-deadline` tag (pushing
  the absolute deadline forward, anchored to launch), not just `spawn:ttl`.
  Previously the extend was a **silent no-op** — spored ignores `spawn:ttl` for
  current instances, so the instance still terminated at its original deadline
  despite the "✅ TTL updated" message. Mirrors the `spawn extend` CLI (#11).
- Clarified the `spawn_extend` invalid-TTL error message (it garbled the
  day/week-suffix guidance).

## [0.36.1]

Baseline. Earlier history is in the
[GitHub Releases](https://github.com/spore-host/spore-host-mcp/releases) and the
[commit log](https://github.com/spore-host/spore-host-mcp/commits/main).

---

[Unreleased]: https://github.com/spore-host/spore-host-mcp/compare/v0.36.1...HEAD
[0.36.1]: https://github.com/spore-host/spore-host-mcp/releases/tag/v0.36.1
