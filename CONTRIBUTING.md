# Contributing to docs-puller

Thanks for your interest. Issues and PRs are welcome.

**How this repo is maintained:** docs-puller is open-core; this repository is
the canonical public home of the local CLI. The maintainer batches changes, so
history moves in release-sized steps. For larger changes, open an issue first.

## Dev setup

Go 1.26+. Plain `go build ./...` works (the pure-Go SQLite driver ships FTS5
built in); CI passes `-tags sqlite_fts5`, so the canonical commands are:

```sh
go build -tags sqlite_fts5 ./...
go vet  -tags sqlite_fts5 ./...
go test -tags sqlite_fts5 ./...
```

Quick end-to-end sanity check (no network needed):

```sh
go run -tags sqlite_fts5 . pull --local ./somedocs --name demo --out /tmp/corpus
go run -tags sqlite_fts5 . reindex --out /tmp/corpus
go run -tags sqlite_fts5 . search "your query" --out /tmp/corpus --json
```

See `AGENTS.md` for the code layout and `README.md` for the full command
surface.

## Quality bar for PRs

- `gofmt` clean, `go vet` clean, full test suite green (CI enforces all three).
- New behavior comes with tests. Bug fixes come with a regression test.
- **Search/ranking/eval changes** must cite measured metric deltas
  (Hit@1 / Hit@5 / MRR) across the checked-in fixtures — the discipline,
  commands, and regression thresholds are in `eval/CONTRIBUTING.md`.
- Keep dependencies minimal. Pure-Go SQLite (`modernc.org/sqlite`, no cgo) is a
  deliberate choice; PRs that add cgo will not be accepted.

## Scope

This repository is the local-first CLI: pull/crawl, index, search, rerank,
eval, single-tenant `serve` (with the embedded web UI), and the VS Code
extension. Hosted multi-tenant features are out of scope here — see
`OPEN-CORE.md` for the boundary.
