# docs-puller

[![ci](https://github.com/nstranquist/docs-puller/actions/workflows/ci.yml/badge.svg)](https://github.com/nstranquist/docs-puller/actions/workflows/ci.yml)

`docs-puller` mirrors vendor, reference, and local project docs into Markdown, builds a local SQLite FTS5 index, and gives agents a fast private search surface with reproducible retrieval evals.

This is the **open-core local CLI**. The hosted Team tier (multi-tenant control plane, billing, managed corpora) is proprietary. See [OPEN-CORE.md](OPEN-CORE.md) for the commercial boundary.

This repository is the canonical source for the CLI and its public Go packages.
Downstream tools should consume the executable contract instead of copying this
source tree. `docs-puller version --json` reports the build identity, supported
commands, and stable capabilities for adapters such as `ndev docs`; release
automation can fail closed with `docs-puller version --expect v0.3.1`.

## Install

Requirements: Go 1.26+.

```sh
go install github.com/nstranquist/docs-puller@latest
```

Or from a checkout:

```sh
git clone https://github.com/nstranquist/docs-puller.git
cd docs-puller
go install .
```

## Five-Minute Smoke

This creates a tiny local corpus, indexes it, checks health, then searches it.

```sh
tmp="$(mktemp -d)"
mkdir -p "$tmp/input"
printf '# PurpleWidget setup\n\nRun `purplewidget init` to configure the local docs mirror.\n' > "$tmp/input/setup.md"

docs-puller pull --local "$tmp/input" --name smoke --out "$tmp/corpus"
docs-puller reindex --out "$tmp/corpus"
docs-puller status --out "$tmp/corpus" --check
docs-puller search "purplewidget init" --out "$tmp/corpus" --source smoke --limit 1 --json
```

The search result should include `setup.md`.

## Core Commands

```sh
docs-puller pull --from urls.md --out ~/code/docs
docs-puller pull --llms-txt https://docs.x.ai/llms.txt --replace-source --out ~/code/docs
docs-puller pull-url https://example.com/docs/page --out ~/code/docs
docs-puller pull --local ~/projects/my-app --name my-app --out ~/code/docs
docs-puller pull-local-batch --source app=~/projects/my-app --source docs=~/code/docs --out ~/code/docs
docs-puller pull --github-repo owner/repo --name repo-docs --out ~/code/docs
docs-puller pull --git-repo https://projects.example.com/team/manual.git --ref release-1.0 --subdir manual --name product --origin-base https://docs.example.com/latest --out ~/code/docs
docs-puller reindex --out ~/code/docs
docs-puller status --out ~/code/docs --check
docs-puller status --out ~/code/docs --check --check-embeddings
docs-puller search "supabase row level security" --out ~/code/docs --compact
docs-puller pins refresh --write --out ~/code/docs
docs-puller search "flatlist performance" --out ~/code/docs --source react-native --version 0.79
docs-puller search "react native debugging" --out ~/code/docs --source react-native --all-versions
```

`--replace-source` treats the discovered URL set as authoritative. It refuses
large deletion plans by default and also refuses filtered or capped replacement
runs. After reviewing the discovery input, pass `--allow-large-prune` to
explicitly acknowledge an intentional large replacement.

Local, GitHub, and generic Git ingestion accept `.md`, `.mdx`, `.mdoc`, and
`.rst`. reStructuredText is normalized into agent-readable Markdown while
preserving retrieval-relevant headings, Sphinx roles, links, figures,
admonitions, and code blocks. Generic Git checkouts are shallow, refreshable,
and cached under `<out>/.cache/<source>-src`; use `--origin-base` when the
published documentation URL differs from the source repository URL.

## Rerank And Embeddings

Embeddings are stored separately from FTS at `<out>/.cache/embeddings.db`; the FTS index remains `<out>/.cache/search.db`. Whole-doc embedding runs also write a flat vector sidecar (`embeddings-<model>.vec`) used by `--rerank-hybrid` before falling back to SQLite.

`status` reports missing or stale embedding sidecars, but `status --check` only fails on core corpus/index health. Add `--check-embeddings` when rerank readiness should be part of the gate.

```sh
docs-puller embed --out ~/code/docs --model text-embedding-3-small
docs-puller embed --out ~/code/docs --model text-embedding-3-small --write-flat-only
docs-puller embed --out ~/code/docs --migrate-legacy
docs-puller search "how do I count tokens with Anthropic" --out ~/code/docs --rerank-llm --rerank-hybrid --rerank-k 10
```

The embedding batcher retries per-input token cap failures and recursively splits batches when the provider rejects total batch tokens.

## Telemetry To Fixture

Query logging is opt-in:

```sh
docs-puller search "deploy Azure Functions from the CLI" --out ~/code/docs --log-query --intent support
DOCS_PULLER_QUERY_LOG=1 docs-puller search "react native list performance" --out ~/code/docs
```

Curate observed queries into a candidate fixture:

```sh
docs-puller telemetry log --limit 20
docs-puller telemetry fixture --intent support --out-file eval/support-candidates.yaml
```

Telemetry-derived fixtures use the observed top hit as `expect` and include a note to verify before promotion.

## Versioned Pins

Latest docs stay canonical at `<out>/<source>/`. Versioned docs are bounded overlays generated from lockfiles, then searched through the same FTS5 index. Sources without source-specific crawl pages seed one entrypoint; high-breakage sources can define a small `versioned_pages` set in `version_policy.yaml`.

```sh
docs-puller pins refresh --out ~/code/docs --json
docs-puller pins refresh --out ~/code/docs --write
docs-puller pins sync --out ~/code/docs --write
docs-puller pull-pins --out ~/code/docs --source react-native --write
docs-puller pins gc --out ~/code/docs --grace-days 14 --write
```

`pull-pins --write` stages a complete pinned source directory before replacing the live overlay, then refreshes only those source IDs in FTS5. Latest docs remain untouched and keep ranking first for migration/latest-intent queries.

Generated pins live at `<out>/_DOCS_PINS.json`. Source families keep their stable names (`react-native`), while pinned source IDs use `<family>__v<version>` (`react-native__v0.79`). Search defaults prefer the current workspace pin, then other workspace pins, latest docs, tools pins, and finally other pins. Use `--all-versions` when mirror hits should remain separate, `--version latest` for upgrade work, or `--version <tag>` for an exact lane.

## Local Web UI And HTTP API (`serve`)

`docs-puller serve` runs a local search server with an embedded web UI — no build
step, no extra dependencies:

```sh
docs-puller serve --out ~/code/docs
# open http://127.0.0.1:7799
```

The UI supports live search with source filtering and doc preview over the JSON API:

- `GET /api/search?q=<query>&source=<id>&limit=<n>`
- `GET /api/sources`
- `GET /api/status`
- `GET /api/doc?path=<rel>`

Security defaults: binds `127.0.0.1` and refuses a non-loopback `--addr` unless a
bearer token is set (`--auth-token`, `--auth-token-file`, or `$DOCS_SERVE_TOKEN`).
The server picks up out-of-process `pull`/`reindex` runs automatically — no
restart needed.

`vscode-extension/` ships a VS Code client for the same endpoint ("Docs Puller:
Search").

## Operator config (optional)

**You do not need config** for pull, search, reindex, eval, or the smoke test above.
Config is for power users who want cwd-based profile selection, monorepo pin
scanning, and custom source keyword boosts.

### Quick start

```sh
docs-puller config init
# edits ~/.docs-puller/config.yaml paths + ~/.docs-puller/profiles/my-stack.yaml sources
docs-puller profile list
docs-puller search "your query" --profile my-stack --out ~/code/docs
```

`config init` writes:

- `~/.docs-puller/config.yaml` (from `config.example.yaml`)
- `~/.docs-puller/profiles/<profile>.yaml` (from `profiles/example.yaml`)

Use `--profile NAME` to pick a different profile name. Pass `--force` to overwrite
existing files.

Check where config resolves:

```sh
docs-puller config path
```

Override location with `DOCS_PULLER_CONFIG=/path/to/config.yaml`.

### Manual setup

```sh
mkdir -p ~/.docs-puller/profiles
cp config.example.yaml ~/.docs-puller/config.yaml
cp profiles/example.yaml ~/.docs-puller/profiles/my-stack.yaml
# edit paths + profile sources, then verify:
docs-puller profile list
```

Profile lookup order: `<corpus>/profiles/` → `~/.docs-puller/profiles/` → profiles
beside your config file → embedded `profiles/example.yaml`.

See `config.example.yaml` for the schema (`cwd_profiles`, `pin_scan_roots`,
`tools_pin_scopes`, `source_keywords`).

## State Paths

- Default corpus: `~/code/docs` (override with `DOCS_PULLER_OUT=<dir>`)
- Isolated corpus: pass `--out <dir>` on pull, reindex, status, search, eval, and pins commands
- Index: `<out>/.cache/search.db`
- Embeddings: `<out>/.cache/embeddings.db` plus optional flat vector sidecars
- Query log: opt-in, controlled by `--log-query` or `DOCS_PULLER_QUERY_LOG=1`
- Ranking-hygiene policy: `DOCS_PULLER_HYGIENE_POLICY=/path/to/policy.json` appends your own downranked path patterns (same JSON shape as `internal/sourcehygiene/policy.json`) to the built-in set — useful for keeping generated notes or scratch exports out of results
- Legacy shared-state paths: set `DOCS_PULLER_LEGACY_NDEV_PATHS=1` only when intentionally sharing corpus state with a private wrapper install (operator builds only)

## Quality Gates

Run these before publishing a public change:

```sh
go build -tags sqlite_fts5 ./...
go vet -tags sqlite_fts5 ./...
go test -tags sqlite_fts5 ./...
docs-puller eval --check-fixture
docs-puller eval --answer-context --record-run
docs-puller eval-suite --overview-md retrieval-metrics.md --overview-html retrieval-metrics.html
docs-puller eval-leaderboard --format json
docs-puller curation lint
```

`eval-suite --overview-md/--overview-html` writes per-library and per-query-type retrieval metrics, including Hit@K, MRR, latency, returned-token estimates, and full answer-context token counts. Overview generation enables answer-context counting automatically so the token columns reflect the returned Markdown docs rather than only snippet metadata.

## Measured Retrieval Eval

The eval harness ships **vendor-style YAML fixtures** (`eval/*.yaml`) you can run against your own corpus:

```sh
docs-puller eval --check-fixture
docs-puller eval-suite --json
```

A **fully reproducible baseline** ships in [`eval/sample-corpus/`](eval/sample-corpus/):
24 pinned public doc pages (SQLite, Go, PostgreSQL) + 24 queries + a frozen
BM25-only baseline (**Hit@1 95.8% / Hit@5 100% / MRR 0.979**) that anyone can
replay with no API key:

```sh
corpus="$(mktemp -d)"
docs-puller pull --from eval/sample-corpus/sources.md --out "$corpus"
docs-puller reindex --out "$corpus"
docs-puller eval --fixture eval/sample-corpus/fixture.yaml --out "$corpus"
docs-puller eval-leaderboard --fixtures eval/sample-corpus --out "$corpus" --format json
```

The main `eval/*.yaml` fixture numbers are measured on the maintainer's larger
multi-vendor corpus mirror — treat those as operator-measured until you rebuild
an equivalent corpus.
