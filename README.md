# docs-puller

[![ci](https://github.com/nstranquist/docs-puller/actions/workflows/ci.yml/badge.svg)](https://github.com/nstranquist/docs-puller/actions/workflows/ci.yml)

`docs-puller` copies vendor, reference, and local project docs into Markdown, builds a local SQLite FTS5 index, and searches that index on your machine. Retrieval quality is measured with checked-in evaluations you can rerun.

## Live Demo

**[Try the public docs-puller demo](https://docs-puller-demo.darthbitcoin.workers.dev)**

The demo needs no account and no API key. It searches a reviewed snapshot of
24 public SQLite, Go, and PostgreSQL pages with the same Go and SQLite FTS5
engine as the CLI. It does not use AI. A rate-limited Cloudflare boundary
protects the bearer-authenticated origin and limits public document previews.
Read the [method and claim boundaries](https://docs-puller-demo.darthbitcoin.workers.dev/method).
Deployment health and synthetic checks do not prove external adoption.

## Quick Start

Install the CLI, then run the built-in proof. The demo uses an isolated,
three-document corpus. It does not change your normal corpus and does not need
an API key.

```sh
go install github.com/nstranquist/docs-puller@v0.7.4
docs-puller demo
```

The top result must be `docs-puller-demo/sqlite-fts5.md`. Use `--json` for a
stable result that an agent or CI job can check.

To search one real public document:

```sh
corpus="$(mktemp -d)"
docs-puller pull-url https://www.sqlite.org/fts5.html --out "$corpus"
docs-puller status --out "$corpus" --check
docs-puller search "external content table" --out "$corpus" --source sqlite
```

Read the [install guide](docs/user/install.md), the
[first-hour guide](docs/user/first-hour.md), or the complete
[user-guide index](docs/user/README.md).

## Showcase

<img src="https://raw.githubusercontent.com/nstranquist/docs-puller/main/assets/brand/docs-puller.png" width="96" height="96" alt="docs-puller application icon">

![docs-puller web search showing ranked SQLite documentation results](https://raw.githubusercontent.com/nstranquist/docs-puller/main/portfolio/assets/search-results.png)

The screenshot is a real local FTS5 search for `sqlite` on this machine
(47,924 documents in the index). It is the reviewed evidence declared in
`portfolio/manifest.yaml`. A full-page capture is in
[screenshots/](screenshots/). Read the [engineering case study](CASE_STUDY.md)
for the problem, architecture, measured results, and claim boundaries.

## Measured Retrieval

Retrieval quality is measured and checked in. Each result includes its query
count, mode, and measurement date. The public sample workflow can be replayed
without an API key. Its source pages are live, so upstream edits can change the
score. The larger results need the maintainer's local corpus mirror.

| Benchmark | Queries | Mode | Hit@1 | Hit@5 | MRR | Measured | Replay boundary |
| --- | ---: | --- | ---: | ---: | ---: | --- | --- |
| Full fixture suite | 459 | BM25 / FTS5 only | 71.5% | 93.5% | 0.810 | 2026-08-18 | Public fixture; local corpus |
| Final frozen holdout | 35 | BM25 / FTS5 only | 45.7% | 94.3% | 0.674 | 2026-08-18 | Public fixture; local corpus |
| Sample corpus (no API key) | 24 | BM25 / FTS5 only | 95.8% | 100% | 0.979 | 2026-08-18 | Live public pages |

The right-hand column is the claim boundary. Treat results on the local mirror
as maintainer measurements until you rebuild an equivalent corpus. The final
holdout was frozen before its first scored run. Earlier holdouts were promoted
to tuning data and are not presented as independent results.

The sample corpus is the honest floor: a fixed list of 24 public page URLs
(SQLite, Go, and PostgreSQL) that anyone can replay end-to-end in a few minutes
with no API key and no account. It demonstrates the pipeline, not the ceiling.
The dated baseline makes upstream content drift visible.

```sh
corpus="$(mktemp -d)"
docs-puller pull --from eval/sample-corpus/sources.md --out "$corpus"
docs-puller reindex --out "$corpus"
docs-puller eval --fixture eval/sample-corpus/fixture.yaml --out "$corpus" \
  --diff eval/sample-corpus/baseline-2026-07-03.json
```

The full suite contains 459 queries across identifier lookups, natural-language
questions, cross-source retrieval, and reviewed local telemetry. It retains
known difficult cases. The final holdout adds 35 source-scoped questions that
were not used to tune the ranker before their first run.

```sh
docs-puller eval-suite --json
docs-puller eval-leaderboard --fixtures eval/sample-corpus --out "$corpus" --format json
```

**What this does and does not claim.** These results measure docs-puller on
documentation-retrieval fixtures. They do not establish superiority over a
hosted product or trained reranker. They are not a claim to novel retrieval
research, and they are not a general benchmark — a domain corpus is not MTEB.
Scoring discipline, including the multi-fixture rule that exists because a
silent regression once cost 12 points of Hit@1, is documented in
[eval/CONTRIBUTING.md](eval/CONTRIBUTING.md).

This is the **open-core local CLI**. The hosted Team tier (multi-tenant control plane, billing, managed corpora) is proprietary. See [OPEN-CORE.md](OPEN-CORE.md) for the commercial boundary.

This repository is the canonical source for the CLI and its public Go packages.
Downstream tools should consume the executable contract instead of copying this
source tree. `docs-puller version --json` reports the build identity, supported
commands, and stable capabilities for adapters such as `ndev docs`; release
automation can fail closed with `docs-puller version --expect v0.7.4`.

## Install

Requirements: Go 1.26.6 or later.

```sh
go install github.com/nstranquist/docs-puller@latest
```

Or from a checkout:

```sh
git clone https://github.com/nstranquist/docs-puller.git
cd docs-puller
go install .
```

Release archives, checksum verification, Windows steps, and upgrades are in the
[install guide](docs/user/install.md).

## Five-Minute Smoke

This creates a tiny isolated corpus, indexes it, and searches it. It removes the
temporary corpus after the check.

```sh
docs-puller demo --json
```

The result must report `"ok": true`, `"mode": "fts5"`, and three documents.
Pass `--keep` to inspect the generated corpus, or `--out DIR` to choose its
location.

## Core Commands

```sh
docs-puller pull --from urls.md --out ~/code/docs
docs-puller pull --llms-txt https://docs.x.ai/llms.txt --replace-source --out ~/code/docs
docs-puller pull-url https://example.com/docs/page --out ~/code/docs
docs-puller pull-url https://www.firecrawl.dev/ --out ~/code/docs
docs-puller pdf-doctor --write-pin ~/.docs-puller/pdf-inspector.json \
  --detect-pdf /path/to/detect-pdf --pdf2md /path/to/pdf2md \
  --source-revision REVISION --source-version VERSION --json
docs-puller pdf-doctor --provider-pin ~/.docs-puller/pdf-inspector.json
docs-puller pull-pdf ./manual.pdf --name manual \
  --provider-pin ~/.docs-puller/pdf-inspector.json --out ~/code/docs
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

Create and verify a provider pin before you run `pull-pdf`. The pin records the
pdf-inspector source repository, source revision, package version, and SHA-256
hash for each executable. `pull-pdf` rejects a missing or changed hash before it
reads or writes a PDF document. Set
`DOCS_PULLER_PDF_PROVIDER_PIN` to use the same pin without repeating the flag.

`pull-pdf` is an optional local sidecar path for text-based PDFs. It runs
`detect-pdf` before `pdf2md`, then writes Markdown into `pdf-docs/` by default.
Scanned, image-based, mixed, encrypted, oversized, and malformed PDFs fail
closed. Use a separate approved OCR or password workflow for those inputs.

The pin verifies the selected executable bytes. It does not prove a release
signature or a reproducible build. Record the source revision and verify the
provider build before you create a pin.

Firecrawl pages use the first-class `firecrawl` source. The local
`pdf-inspector` sidecar does not call the hosted Firecrawl API and does not
provide OCR.

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

The embedding batcher retries per-input token cap failures and recursively
splits batches when the provider rejects total batch tokens. A successful
whole-document run also removes vectors for deleted or renamed documents before
rewriting the flat sidecar. Source-scoped hybrid search filters the flat and
SQLite vector paths before top-K selection, preserving the source isolation of
the BM25 query.

## Telemetry To Fixture

Query logging is on by default for normal `search` calls and can be disabled
per call with `--log-query=false` or globally with
`DOCS_PULLER_QUERY_LOG=0`. Integrations should identify themselves:

```sh
docs-puller search "deploy Azure Functions from the CLI" --out ~/code/docs \
  --intent support --client terminal --run-context operator
DOCS_PULLER_QUERY_CLIENT=my-agent DOCS_PULLER_RUN_CONTEXT=agent \
  docs-puller search "react native list performance" --out ~/code/docs
```

Contexts `operator`, `agent`, `mcp`, and `production` count as real dogfood;
`eval`, `test`, `benchmark`, and `batch` are synthetic; older or unlabeled
rows stay `unknown`.

Curate observed queries into a candidate fixture:

```sh
docs-puller telemetry log --limit 20
docs-puller telemetry summary --json
docs-puller telemetry fixture --intent support --out-file eval/support-candidates.yaml
```

Telemetry-derived fixtures use the observed top hit as `expect` and include a
note to verify before promotion. Fixture export defaults to real traffic so
repeated eval queries cannot silently become production fixtures; pass
`--traffic-class all` only for an explicit legacy audit.

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
- `GET /api/doc?source=<id>&path=<rel>[&max_bytes=<n>&line=<n>]`

`max_bytes` returns a bounded plain-text Markdown window. Add the one-based
`line` value to keep a search hit inside that window. `line` requires
`max_bytes`. If you omit both fields, the local API returns the full document.

Security defaults: binds `127.0.0.1` and refuses a non-loopback `--addr` unless a
bearer token is set (`--auth-token`, `--auth-token-file`, or `$DOCS_SERVE_TOKEN`).
The server picks up out-of-process `pull`/`reindex` runs automatically — no
restart needed.

`vscode-extension/` ships a VS Code client for the same endpoint. The v0.7.4
GitHub release includes a checksummed `docs-puller-search-0.3.0.vsix`. The
extension is not published in the VS Code Marketplace. It supports bearer
tokens through VS Code SecretStorage and confines returned paths to the local
corpus root. See [`vscode-extension/README.md`](vscode-extension/README.md).

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

## Troubleshooting

### `status` reports a missing or stale FTS5 index

Verify the corpus, rebuild the index, and verify it again:

```sh
docs-puller status --out ~/code/docs
docs-puller reindex --out ~/code/docs
docs-puller status --out ~/code/docs --check
```

Use the same `--out` path for pull, reindex, status, search, and serve.

### A pull records `low-content`

Open the recorded URL and the stored Markdown file. Verify that the source
returns the document body without client-side JavaScript.

If the web page returns only an application shell, use an `llms.txt`, local,
GitHub, or Git source. Pull the source again, then run `reindex`.

### Search returns no useful result

Run `status --check` first. Then repeat the search without `--source`,
`--profile`, or `--version` filters.

If the broad search works, add one filter at a time. If it fails, run
`reindex` against the same corpus path.

### `serve` refuses a non-loopback address

For a non-loopback `--addr`, the server requires a bearer token.
Create a private token file before you expose the server to a trusted network:

```sh
token_file="$HOME/.docs-puller/serve-token"
umask 077
openssl rand -hex 32 > "$token_file"
docs-puller serve --addr 0.0.0.0 --out ~/code/docs \
  --auth-token-file "$token_file"
```

If remote access is not required, keep the default `127.0.0.1` address. The
HTTP server does not provide TLS.

### Rerank reports a missing API key or embedding index

Remove the rerank flags to use local BM25 search without an API key. To use
reranking, configure the selected provider key and build the embedding index.

```sh
docs-puller embed --out ~/code/docs --model text-embedding-3-small
docs-puller status --out ~/code/docs --check-embeddings
```

### `pull-pdf` rejects the provider pin

Do not bypass a missing or changed executable hash. Verify the provider build,
then create a new pin with `pdf-doctor` as shown in Core Commands.

## State Paths

- Default corpus: `~/code/docs` (override with `DOCS_PULLER_OUT=<dir>`)
- Isolated corpus: pass `--out <dir>` on pull, reindex, status, search, eval, and pins commands
- Index: `<out>/.cache/search.db`
- Embeddings: `<out>/.cache/embeddings.db` plus optional flat vector sidecars
- Query log: on by default at `<out>/.cache/query-log.jsonl`; disable per call
  with `--log-query=false` or globally with `DOCS_PULLER_QUERY_LOG=0`
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

A **replayable live-page baseline** ships in
[`eval/sample-corpus/`](eval/sample-corpus/): a fixed list of 24 public doc URLs
(SQLite, Go, and PostgreSQL), 24 queries, and a dated BM25-only baseline
(**Hit@1 95.8% / Hit@5 100% / MRR 0.979**). Anyone can run the workflow with no
API key. Upstream page edits can change a later score.

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

## Hosted Team Design Partner

The local CLI stays free and local-first. I am recruiting one founding design
partner for the proprietary hosted Team service: a 30-day, single-tenant pilot
for an engineering team that wants private docs and repositories continuously
synchronized and searchable by people and coding agents.

The pilot is **$1,500 fixed**, has no automatic renewal, and includes a measured
retrieval baseline plus a full corpus export at exit. The provisional post-pilot
price is $199/month for up to 10 users; it will be validated with the first
partner before becoming a public plan. Read the complete scope and exclusions in
[DESIGN_PARTNERS.md](DESIGN_PARTNERS.md), then use the
[design-partner intake issue](https://github.com/nstranquist/docs-puller/issues/new?template=design-partner.yml).

## License

Apache License 2.0 — see [LICENSE](LICENSE) and [NOTICE](NOTICE).

Copyright 2026 Nico Stranquist.

The hosted Team tier is a separate proprietary work and is not covered by this
license; see [OPEN-CORE.md](OPEN-CORE.md) for the commercial boundary.
