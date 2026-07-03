# docs-puller — agent guide

`docs-puller` is a local-first documentation retrieval CLI (pure Go,
`modernc.org/sqlite`, zero cgo). It mirrors any docs corpus to Markdown, builds a
SQLite FTS5 index, and exposes agent-friendly search plus a reproducible retrieval
eval (Hit@1 / Hit@5 / MRR).

## Build, test, run

Plain `go build ./...` works — the pure-Go SQLite driver ships FTS5 built in.
CI passes `-tags sqlite_fts5` (the tag is accepted and harmless), and the
commands below match CI exactly.

```sh
go build -tags sqlite_fts5 ./...
go vet  -tags sqlite_fts5 ./...
go test -tags sqlite_fts5 ./...
```

Quick end-to-end:

```sh
go run -tags sqlite_fts5 . pull --local ./somedocs --name demo --out /tmp/corpus
go run -tags sqlite_fts5 . reindex --out /tmp/corpus
go run -tags sqlite_fts5 . search "your query" --out /tmp/corpus --json
```

State: default corpus is `~/code/docs`; pass `--out <dir>` to isolate one. The index
lives at `<out>/.cache/search.db`. Optional operator config: `docs-puller config init`
→ `~/.docs-puller/config.yaml`. See `README.md` for the full command surface.

## Layout

- `main.go` — command dispatcher
- `pull*` / `crawl*` / `extract*` — fetch + HTML→Markdown conversion
- `index.go`, `search*.go`, `rerank_*.go` — FTS5 index + search + rerank
- `eval*.go`, `eval/` — retrieval eval harness + frozen fixtures (the published quality number)
- `searchcore/`, `searchengine/`, `searchruntime/` — search dispatch
- `internal/` — `apppaths`, `providers` (AI provider registry), `sourcehygiene`, db layers
- `vscode-extension/` — VS Code client for `docs-puller serve`

## Conventions

- Correct, typed Go. Pure-Go SQLite (no cgo). Keep the `sqlite_fts5` tag on every build/test.
- Eval/ranking changes must cite the measured metric delta (Hit@1 / Hit@5 / MRR) — discipline in `eval/CONTRIBUTING.md`.
- This repository is the **open-core local CLI**. The hosted / multi-tenant tiers are
  proprietary and are not part of this surface — see `OPEN-CORE.md`.
