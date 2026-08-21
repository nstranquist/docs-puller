# docs-puller changelog

Notable user-facing changes are recorded per public release. Retrieval-quality
claims must include the measured fixture and metric delta; behavioral defaults
must name both the old and new behavior.

## Unreleased

## v0.8.0 — 2026-08-21

### Added

- Route Mintlify Jupiter developer docs (`developers.jup.ag/docs`) into the `jupiter` source, including native `.md` pages and OpenAPI YAML mirrors.
- Publish AI-facing project summaries at `ai-info.md` and `llms.txt`, with generated copies on the demo site.

### Changed

- Refresh the reviewed 24-page sample to the Go 1.27 language specification without changing its source or document counts.

### Fixed

- Use the supported `nstranquist.workers.dev` hostname for the public demo and its generated discovery links.
- Verify the public GitHub repository in the launch contract instead of treating `go install` as a web deployment.
- Build the public demo from reviewed snapshot bytes and report live upstream drift without changing production.
- Give the three-page browser budget proof enough time to retain evidence on busy hosts.
- Keep source-repository launch proof independent from versioned install checks in the release synchronizer.
- Keep canonical docs from a named leading source ahead of duplicate off-source workspace pages.
- Document the complete account-pinned Worker deployment and the real corpus cutover boundary.

## v0.7.6 — 2026-08-19

### Fixed

- Publish GitHub releases from verified drafts and require release immutability before success.

## v0.7.5 — 2026-08-19

### Fixed

- Make the overflowing document preview keyboard-scrollable in Safari and cover the real overflow path in browser accessibility tests.

## v0.7.4 — 2026-08-19

### Fixed

- Canonicalize demo source manifests so clean builds produce identical root filesystem archives.

## v0.7.3 — 2026-08-19

### Fixed

- Require the exact patched Go toolchain when the release manifest declares one.
- Expose the public live demo in the README and typed Product Passport.

## v0.7.2 — 2026-08-19

### Fixed

- Keep the portable logical-path assertion platform independent in Windows CI.

## v0.7.1 — 2026-08-19

### Fixed

- Corpus identifiers now use slash-separated paths on every operating system.
  Manifests, scan results, FTS5 rows, source indexes, and HTTP responses no
  longer expose Windows filesystem separators.
- Read-only SQLite connections now use rooted Windows file URIs. Windows demo,
  search, status, evaluation, and batch-search paths can open the FTS5 index.
- Explicit `HOME` environments now work consistently on Windows, including
  config initialization, profile matching, and state paths.
- Windows now uses its native executable and directory-durability semantics.
  POSIX-only test fixtures are skipped only where the fixture itself requires a
  POSIX shell or executable mode bits.

### Security and release engineering

- The minimum and release-build Go toolchain is pinned to 1.26.6, which removes
  the actionable standard-library findings reported against 1.26.5.
- Windows runtime behavior is checked in the public operating-system matrix;
  deterministic Linux demo binaries and the container remain reproducible.

## v0.7.0 — 2026-08-19

### Added

- Serve bounded document windows around matched lines.

### Fixed

- The public demo previews matched excerpts without exceeding the Worker response limit.

## v0.6.0 — 2026-08-19

### Added

- Added `docs-puller demo`, an isolated three-document proof that needs no API
  key and does not change the normal corpus.
- Added install, first-hour, search, architecture, security, troubleshooting,
  and uninstall guides for the complete local CLI adopter journey.
- Added native Markdown mirroring for Unity documentation.
- Added a secure VS Code search client and a deterministic v0.3.0 VSIX package.
- Added a public engineering case study and a typed product passport.
- Added the tested public demo site and bounded Cloudflare Worker API.
  It uses the 24-document public sample. Deployment proof remains separate
  from source readiness.
- Added final frozen holdout, PostHog, and Unity retrieval fixtures.

### Changed

- Natural-language search now adds a source-scoped lexical recall tier and
  reciprocal-rank fusion. Boilerplate-only variant selectors do not receive
  semantic body credit.
- The 151-query main BM25 regression set changed from 83.44% to 84.11% Hit@1,
  from 99.34% to 100% Hit@5, and from 0.8945 to 0.9000 MRR on the refreshed
  maintainer corpus.
- The final 35-query frozen holdout changed from 42.86% to 45.71% Hit@1 while
  keeping 94.29% Hit@5; MRR changed from 0.6545 to 0.6736.
- The `docs-puller serve` search page now has a landing state with corpus
  counts, cleaner result cards, Markdown-stripped snippets, and a dark/light
  toggle. Filesystem roots are not shown on the page.
- Point README images at `raw.githubusercontent.com`. GitHub currently
  rewrites relative image paths to `/raw/main/...` URLs that return 404.

### Fixed

- Restored Firecrawl PDF routing and hardened provider URL and credential
  handling.
- Search index updates now repair missing FTS path entries during ingestion.
- Refreshed direct Go dependencies and removed the known vulnerability finding.

### Security

- An unspecified or wildcard HTTP bind now requires authentication. Loopback
  use keeps the local-first default.
- Added fuzz coverage for manifest parsing, configuration parsing, query
  construction, and locale-variant deduplication.
- The VS Code client validates runtime identity, confines filesystem paths, and
  preserves the server authentication boundary.

### Release engineering

- Added deterministic archives for six macOS, Linux, and Windows targets,
  SHA-256 checksums, a CycloneDX SBOM, provenance, and reproducibility checks.
- Added pinned GitHub Actions, secret scanning, verified artifact attestations,
  and fail-closed release publication.
- Added `make release-ready`, public-sample, retrieval-regression, held-out,
  release-contract, package, and clean-clone gates.

### Verification

- `go test -tags sqlite_fts5 ./...`
- `go vet -tags sqlite_fts5 ./...`
- `make verify-public-sample`
- `make verify-retrieval-regression`
- `make verify-held-out`
- `make release-ready RELEASE_VERSION=v0.6.0`

## v0.5.0 — 2026-07-22

### Added

- Added compact human and JSON help projections so agents can discover the
  public verb surface without loading the full command reference.
- Batch search now preserves per-query intent, caller, run-context, source,
  and retrieval-mode telemetry using the same bounded local log as individual
  searches.
- Added repeatable local `make publish-ready`, verification, help-size, and
  isolated smoke gates for contributors and release preparation.

### Verification

- `make publish-ready`
- `make help-sizes`
- `make smoke`

## v0.4.0 — 2026-07-14

### Added

- Search telemetry now records caller and run-context provenance, classifies
  real versus synthetic traffic, reports a class-aware summary, and defaults
  fixture export to real dogfood queries.
- Added a public hosted Team design-partner offer and structured GitHub intake
  form while keeping the proprietary service outside the OSS repository.

### Fixed

- Source-scoped hybrid reranking now filters both SQLite and flat-sidecar
  embedding candidates before top-k selection.
- Embedding refreshes prune deleted or renamed whole-document vectors for the
  selected source and model, then rebuild the model-wide flat sidecar.
- Telemetry fixture filters reject invalid traffic classes, while summaries
  reconcile malformed legacy classes instead of dropping their counts.

### Verification

- `go build -tags sqlite_fts5 ./...`
- `go vet -tags sqlite_fts5 ./...`
- `go test -tags sqlite_fts5 ./...`
- `go test -race -tags sqlite_fts5 -count=1 ./...`
- `go run honnef.co/go/tools/cmd/staticcheck@latest -tags sqlite_fts5 ./...`
- `gitleaks detect --source . --no-git --redact --exit-code 1`
- `npm ci --prefix vscode-extension && npm run compile --prefix vscode-extension`
- Six-fixture local eval sweep, including Blender Hit@1/5 100% and MRR 1.000.
- Frozen 24-query public sample replay: Hit@1 95.8%, Hit@5 100%, MRR 0.979.

## v0.3.1 — 2026-07-14

### Fixed

- Upgraded the nship launch contract to schema v2 with explicit managed
  runtime policy for the public install and credential-free read policy for
  release and distribution verification.

## v0.3.0 — 2026-07-14

### Added

- `pull --git-repo URL` mirrors documentation from generic Git hosts with the
  same shallow-cache refresh behavior as GitHub ingestion. `--origin-base`
  maps mirrored source paths back to canonical published documentation URLs.
- Local and repository ingestion now accepts reStructuredText (`.rst`) and
  normalizes headings, Sphinx roles, links, figures, admonitions, and code
  blocks into agent-readable Markdown without a Python/docutils dependency.
- Added a source-scoped Blender Manual retrieval fixture. On the complete
  2,222-page Blender 5.1 corpus, FTS5 scores Hit@1/3/5 100%, MRR 1.000, and
  p50 10 ms across 40 modeling, scripting, compositing, animation, simulation,
  rendering, and import/export workflow queries (up from the initial nine-query
  acceptance set).

### Fixed

- Reference-repository crawling now excludes known scaffold and stub narrative
  from searchable output, and final source replacement uses source-scoped FTS5
  paths instead of a full-corpus manifest scan.
- `--replace-source` is now honored by local and repository ingestion, so a
  refreshed source prunes removed documents with the existing large-prune
  safety guard instead of leaving stale files searchable.
- Local and repository ingest logs now report document URLs, written outputs,
  and byte-identical unchanged outputs separately instead of labeling every
  walked document as freshly pulled.
- Replacement pruning now validates URL-derived paths, refuses traversal and
  symlink-parent escapes, and reconciles sparse Git checkouts when `--subdir`
  changes or is removed.
- RST normalization now consumes common directive options instead of leaking
  `:option:` metadata into generated Markdown.
- Strict profile search accepts typed nil profiles safely, exact title matches
  remain ranking invariants, and Blender source queries receive a
  source-appropriate term rewrite instead of generic documentation noise.
- Normal FTS5 search and readiness checks use read-only SQLite connections, so
  concurrent searches no longer contend with an authoritative pull for a
  write lock.

### Verification

- `go test -tags sqlite_fts5 ./...`
- `go test -race -tags sqlite_fts5 ./...`
- `go vet -tags sqlite_fts5 ./...`
- `go build -tags sqlite_fts5 ./...`
- `go run honnef.co/go/tools/cmd/staticcheck@latest -tags sqlite_fts5 ./...`
- `gitleaks detect --source . --no-git --redact --exit-code 1`
- `npm ci --prefix vscode-extension && npm run compile --prefix vscode-extension`
- `docs-puller eval --fixture eval/blender.yaml --check-fixture`
- `docs-puller eval --fixture eval/blender.yaml --profile blender-workspace --strict --record-run`
- Replayable 24-query live-page sample: Hit@1 95.8%, Hit@5 100%, MRR 0.979.
- Live authoritative pull from `projects.blender.org/blender/blender-manual.git`
  at `blender-v5.1-release`: 2,222 pages ingested and FTS5 corpus status green.

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
