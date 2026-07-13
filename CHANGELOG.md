# docs-puller changelog

Notable user-facing changes are recorded per public release. Retrieval-quality
claims must include the measured fixture and metric delta; behavioral defaults
must name both the old and new behavior.

## Unreleased

No changes yet.

## v0.2.3 — 2026-07-13

### Fixed

- A malformed embedded source-hygiene policy now degrades to an empty policy
  with a warning instead of panicking inside the reusable library package.

### Changed

- Removed unreachable helpers, used equivalent typed record conversions, and
  cleared the current Go 1.26 staticcheck findings without changing CLI output
  or retrieval scoring.

### Verification

- `go build -tags sqlite_fts5 ./...`
- `go vet -tags sqlite_fts5 ./...`
- `go test -tags sqlite_fts5 -count=1 ./...`
- `go test -race -tags sqlite_fts5 -count=1 ./...`
- `go run honnef.co/go/tools/cmd/staticcheck@latest -tags sqlite_fts5 ./...`
- Go footgun audit: zero critical/high findings, including tests.
- `npm ci --prefix vscode-extension && npm run compile --prefix vscode-extension`

## v0.2.2 — 2026-07-13

### Fixed

- Top-level help now advertises the existing `version --expect VERSION`
  release gate instead of only showing `version --json`.

### Verification

- `go build -tags sqlite_fts5 ./...`
- `go vet -tags sqlite_fts5 ./...`
- `go test -tags sqlite_fts5 -count=1 ./...`
- `go test -race -tags sqlite_fts5 -count=1 ./...`
- `npm ci --prefix vscode-extension && npm run compile --prefix vscode-extension`

## v0.2.1 — 2026-07-13

### Fixed

- The nship release contract now verifies the versioned public Go module
  directly. It no longer risks resolving an older `docs-puller` earlier on the
  operator's `PATH` after `go install` writes to a different `GOBIN`.

## v0.2.0 — 2026-07-13

### Added

- `docs-puller version --json` publishes a stable executable contract with the
  module version, build identity, supported commands, and capabilities.
  `--expect VERSION` gives launch automation an exact, fail-closed release gate.
- `pull --llms-txt URL` supports both conventional link-index files and
  combined `===/path===` corpora such as xAI's native-Markdown export.
- Authoritative `--replace-source` pulls now refuse filtered, capped, or large
  deletion plans unless the operator explicitly passes `--allow-large-prune`.

### Changed

- The standalone repository is the canonical source for the public CLI and Go
  packages. The private build-tag command splice was removed; proprietary
  hosted services remain downstream in `nicos-tools`.
- Pulls avoid rewriting byte-identical documents, report unchanged counts, and
  repair missing FTS rows for successful unchanged paths.
- Authoritative replacement preserves last-known-good documents when a requested
  URL temporarily fails and removes stale Markdown files that are no longer in
  the source manifest.
- `eval-suite` reports fixture progress on stderr, keeping long evaluation runs
  observable without contaminating JSON or report output on stdout.

### Fixed

- Manifest path collisions now resolve deterministically by newest fetch time
  and URL tie-break. Every manifest write prunes stale duplicate-path entries,
  and `status` reports any remaining affected sources.
- Incremental FTS updates delete orphaned path rows before reinsertion, repairing
  indexes whose `docs` and `docs_path` tables were previously out of sync.

### Verification

- `go build -tags sqlite_fts5 ./...`
- `go vet -tags sqlite_fts5 ./...`
- `go test -tags sqlite_fts5 -count=1 ./...`
- `go test -race -tags sqlite_fts5 -count=1 ./...`
- `npm ci --prefix vscode-extension && npm run compile --prefix vscode-extension`
- Live one-document pull from `https://docs.x.ai/llms.txt`, followed by a healthy
  corpus status check.

## v0.1.0 — 2026-07-03

- Initial public open-core CLI release: local/vendor documentation mirroring,
  SQLite FTS5 search, profiles and version lanes, the HTTP/VS Code search
  surface, and reproducible retrieval evaluation.
