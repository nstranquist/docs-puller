# Sample corpus — replayable live-page retrieval eval

Anyone can replay this workflow with no API key. The corpus is built from a
fixed list of 24 public documentation URLs (`sources.md` — SQLite, Go, and
PostgreSQL). `fixture.yaml` scores 24 mixed identifier-style and
natural-language queries against it with BM25 only. The pages are live, so a
later replay can differ from the dated baseline.

## Reproduce

```sh
corpus="$(mktemp -d)"
docs-puller pull --from eval/sample-corpus/sources.md --out "$corpus"
docs-puller reindex --out "$corpus"
docs-puller eval --fixture eval/sample-corpus/fixture.yaml --out "$corpus"
```

Render the leaderboard from it:

```sh
docs-puller eval-leaderboard --fixtures eval/sample-corpus --out "$corpus" --format json
docs-puller eval-leaderboard --fixtures eval/sample-corpus --out "$corpus" --leaderboard-out leaderboard.html
```

## Frozen dated baseline

`baseline-2026-07-03.json` (BM25-only, no rerank):

| Metric | Value |
| --- | --- |
| Hit@1 | 95.8% |
| Hit@3 / Hit@5 | 100% |
| MRR | 0.979 |
| Queries | 24 |
| p50 latency | <1 ms |

Diff a fresh run against it:

```sh
docs-puller eval --fixture eval/sample-corpus/fixture.yaml --out "$corpus" \
  --diff eval/sample-corpus/baseline-2026-07-03.json
```

Content drift on the upstream pages can move rankings slightly over time; the
fixed URL set keeps paths stable, and the baseline is dated so drift is
measurable rather than mysterious.

## Notes

- The corpus is small on purpose: it demonstrates the *pipeline* end-to-end
  (pull → index → search → eval) and gives an honest, replayable floor. The
  main `eval/*.yaml` fixtures measure a much larger multi-vendor corpus.
- A few queries are deliberately cross-source ambiguous (WAL exists in both
  SQLite and PostgreSQL) — that competition is the retrieval problem being
  measured.
