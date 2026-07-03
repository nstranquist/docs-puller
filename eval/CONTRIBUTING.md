# docs-puller eval — discipline guide

Two rules, learned the hard way on 2026-05-04 when a concurrent agent's
`applySearchVersionPolicy` double-call silently undid rerank work and crashed
identifier Hit@1 from 79% → 67% (and NL Hit@1 from 17% → 13%) for hours
before being caught.

## Rule 1 — Multi-fixture scoring is the discipline

Every change to the search pipeline (`dispatchSearch`, rerank, hybrid, version
policy, source hygiene) must report metrics on all checked-in public fixtures:

- `eval/fixture.yaml` — 145 identifier-style queries (the historical baseline).
- `eval/nl-questions.yaml` — 30 natural-language queries (curated 2026-05-04, vocabulary-mismatch focus).
- `eval/natural-language.yaml` — 33 natural-language queries (curated earlier, broader NL-style).
- `eval/production-telemetry.yaml` — 27 hand-reviewed queries promoted from real query logs.
- `eval/refs-dissections.yaml` — 7 reference-repo adoption queries.

Canonical public commands:

```sh
docs-puller eval-suite --rerank-llm
# Before editing:
docs-puller eval-suite --rerank-llm --json > /tmp/pre-change-suite.json
# After editing:
docs-puller eval-suite --rerank-llm --diff /tmp/pre-change-suite.json
```

`eval-suite --rerank-llm` runs HyDE + hybrid + LLM rerank with gate=0.10 over
all `*.yaml` fixtures in this directory that have a `queries:` key. If you need
a historical comparison against only a subset, pin the fixture set explicitly:

```sh
docs-puller eval-suite --rerank-llm \
  --include-fixture fixture.yaml,natural-language.yaml,nl-questions.yaml \
  --diff /tmp/pre-change-suite.json
```

`eval-suite --include-fixture` and `--exclude-fixture` accept comma-separated
fixture basenames or paths. Use them for historical or focused comparisons;
omit them for the canonical current suite. When using `--diff`, compare against
a baseline with the same fixture set; otherwise the diff will correctly report
the intentionally excluded rows as missing current or missing baseline fixtures.

For variant testing (without the full pipeline), call docs-puller eval-suite
directly:

```sh
docs-puller eval-suite --rerank-llm --rerank-hybrid=false   # latency-sensitive
docs-puller eval-suite --rerank-llm --rerank-hyde=false     # without HyDE
docs-puller eval-suite                                       # BM25 only
```

Runs all public fixtures with the production rerank pipeline (HyDE + hybrid +
LLM rerank). Defaults match the production search defaults exactly, so a
config that scores well here is also what users will see.

A change that helps one fixture and regresses another is a **routing problem**,
not a default-flip. Document the trade-off and ship as opt-in until query-shape
routing exists.

## Rule 2 — Architectural changes need fresh eval baselines

Any change to:

- `search.go::dispatchSearch`
- `embed.go::applyHybridRetrieval`
- `rerank_*.go` (any file)
- `version_policy.go`
- `internal/sourcehygiene/`

…must be paired with a fresh eval baseline AND a `--diff` against the prior
baseline. Two-line check:

```sh
# Capture pre-change baseline (run BEFORE editing)
docs-puller eval --rerank-llm --rerank-gate 0.10 \
  --write-baseline /tmp/pre-change.json --json > /dev/null

# After editing + building, diff:
docs-puller eval --rerank-llm --rerank-gate 0.10 \
  --diff /tmp/pre-change.json
```

A drop of >5pp Hit@1 OR >5pp Hit@5 OR per-source regressions on >2 sources is
the smoking gun. Investigate before merging.

For larger arch changes (e.g. swapping an embedding model, changing the
candidate-pool sizing logic, refactoring the search-hits flow), run the full
eval-suite as the gate:

```sh
docs-puller eval-suite --rerank-llm --json > /tmp/pre-change-suite.json
# edit, rebuild, then:
docs-puller eval-suite --rerank-llm --diff /tmp/pre-change-suite.json
docs-puller eval-suite --rerank-llm --json | tee /tmp/post-change-suite.json
```

## Hybrid retrieval and flat sidecars

`applyHybridRetrieval` (`embed.go`) prefers the per-model flat
sidecar (`embeddings-<model>.vec` + `<model>.json` metadata under `.cache/`)
over SQLite. SQLite is the cold path / diagnostic store. **The 2026-05-04
schema bug — `embed --model X` overwriting other models' SQLite rows via
`ON CONFLICT(path)` — went latent for hours because the flat sidecars
(per-model, kept independently) masked the SQLite drift in production
retrieval.**

Detect drift via:

```sh
docs-puller status --check
```

A "stale flat embedding index for model X" warning means the sidecar count
no longer matches SQLite for model X. Run `docs-puller embed --model X` to
reconcile (auto-migration in the embed pipeline now handles the schema fix
on first open; the composite `(path, model)` PK keeps multiple models'
rows independent going forward).

## Baselines (good for diffs)

Public contributors should create a pre-change baseline from their own corpus:

```sh
docs-puller eval-suite --rerank-llm --json > /tmp/pre-change-suite.json
```

Maintainer-only frozen baselines live in the private monorepo because they
encode a private mirror. They are used for release review, but they are not part
of this public repository.

## Standard rerank flags (pin these to match production)

When generating a baseline that matches the production default, use:

```
--rerank-llm                # opt into LLM rerank
--rerank-gate 0.10          # skip rerank on confident BM25 winners
--rerank-k 10               # candidate pool size (defaultRerankK)
# (omit --rerank-hybrid; defaults to true)
# (omit --rerank-hyde;    defaults to true)
# (omit --rerank-llm-model; defaults to gpt-4.1-mini)
# (omit --rerank-llm-provider; defaults to openai)
```

Variants for testing alternative configs:

| variant | extra flags |
|---|---|
| `default` (production) | `--rerank-llm --rerank-gate 0.10` |
| `latency-sensitive` (no HyDE) | `--rerank-llm --rerank-gate 0.10 --rerank-hyde=false` |
| `latency-min` (no hybrid either) | `--rerank-llm --rerank-gate 0.10 --rerank-hyde=false --rerank-hybrid=false --rerank-k 5` |
| `nl-optimized` | `--rerank-llm --rerank-gate 0.10 --rerank-model text-embedding-3-large` |
| `cohere` | `--rerank-llm --rerank-llm-provider cohere --rerank-llm-model rerank-v4.0-pro --rerank-gate 0.10` |
| `tei (local)` | `--rerank-llm --rerank-llm-provider tei --rerank-llm-model BAAI/bge-reranker-v2-m3` (TEI Docker required) |

## Adding a new fixture

`eval-suite` auto-discovers any `*.yaml` in `eval/`. To add a fixture:

1. Create `eval/<name>.yaml` with the same shape as `eval/fixture.yaml`.
2. Verify all canonical paths exist: `docs-puller eval --fixture eval/<name>.yaml --check-fixture`.
3. Re-run `docs-puller eval-suite --rerank-llm` — the new fixture will be included.

## Production-telemetry fixture

`--log-query` is default ON since 2026-05-04. The checked-in
`eval/production-telemetry.yaml` fixture is a curated subset of real
query-log entries, with internal KB/local-workflow rows removed before
promotion and obvious top-1 placeholders corrected to canonical docs.

To refresh the candidate pool from current local telemetry:

```sh
docs-puller telemetry fixture \
  --out-file /tmp/docs-puller-production-telemetry.yaml \
  --since 2026-05-04T00:00:00Z \
  --exclude-fixture eval/fixture.yaml,eval/nl-questions.yaml,eval/natural-language.yaml,eval/production-telemetry.yaml \
  --limit 200
```

The `--exclude-fixture` flag (added 2026-05-04) skips queries already in the
known fixtures so the production sample isn't dominated by eval-driven
duplicates.

Before checking in refreshed rows, hand-verify canonical paths. Telemetry uses
observed top-1 hits as placeholders, so promotion must fix narrow
troubleshooting hits when the canonical guide is the better expected answer
and must drop private/internal query text that should not become repo eval
data. `docs-puller eval-suite --rerank-llm` auto-includes the fixture; the
canonical suite baseline includes it.
