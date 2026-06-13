# CLAUDE.md — spore-host-mcp

`spore-host-mcp` is the MCP server that gives AI assistants (Claude, Cursor)
access to spore.host tooling. Part of the spore.host suite.

## Versioning & changelog (required)

This project follows **[Semantic Versioning 2.0.0](https://semver.org/spec/v2.0.0.html)**
and keeps a **[Keep a Changelog](https://keepachangelog.com/en/1.1.0/)**-format
`CHANGELOG.md` at the repo root. (Spore.host-wide policy — every repo.)

**Every change that affects users updates `CHANGELOG.md`** in the same PR, under
`## [Unreleased]` in the right group (`Added` / `Changed` / `Deprecated` /
`Removed` / `Fixed` / `Security`; `Documentation` for docs-only). Describe the
user-visible effect; reference the issue/PR.

**On release:**

1. Promote `## [Unreleased]` → `## [X.Y.Z] - YYYY-MM-DD`, open a fresh
   `## [Unreleased]`, update the comparison links.
2. Pick `X.Y.Z` by SemVer (MAJOR breaking / MINOR feature / PATCH fix; pre-1.0
   breaking → MINOR).
3. Tag `vX.Y.Z` → the GoReleaser Release workflow builds and publishes.

GoReleaser auto-generates the GitHub Release notes from commits; `CHANGELOG.md`
is the curated, human-facing source of truth. Keep both.

## Build & test

- `go test ./...`
- `go vet ./...`
- `go build ./...`
