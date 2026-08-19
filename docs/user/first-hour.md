---
title: First hour
---

# First hour

Start with the isolated proof. No API key is required.

```sh
docs-puller version --json
docs-puller demo --json
```

The demo result must report `ok: true`, `mode: fts5`, and three indexed
documents. It removes its temporary corpus after the check.

## Build a small public corpus

Choose a corpus directory and use the same path for every command:

```sh
corpus="$HOME/code/docs-puller-first-hour"
docs-puller pull-url https://www.sqlite.org/fts5.html --out "$corpus"
docs-puller pull-url https://go.dev/ref/spec --out "$corpus"
docs-puller status --out "$corpus" --check
docs-puller search "external content tables" --out "$corpus" --source sqlite
docs-puller search "method sets pointer receiver" --out "$corpus" --source go --json
```

Each pull updates the FTS5 index. Run `reindex` after you edit or remove corpus
files outside docs-puller.

## Replay the public benchmark

The repository contains a fixed list of 24 live public-page URLs, 24 queries,
and a dated baseline. Use a v0.7.4 checkout so the fixture and binary agree:

```sh
git clone --depth 1 --branch v0.7.4 \
  https://github.com/nstranquist/docs-puller.git
cd docs-puller
corpus="$(mktemp -d)"
go run -tags sqlite_fts5 . pull --from eval/sample-corpus/sources.md --out "$corpus"
go run -tags sqlite_fts5 . eval --fixture eval/sample-corpus/fixture.yaml \
  --out "$corpus" --diff eval/sample-corpus/baseline-2026-07-03.json
```

The baseline is Hit@1 95.8%, Hit@5 100%, and MRR 0.979. Upstream page changes
can change latency or ranking. Treat a reported diff as evidence to inspect.

## Continue

- Read [Search](search.md) for filters, JSON, and the local server.
- Read [Security and privacy](security.md) before you log private queries, use
  reranking, or bind the server to a network interface.
- Read [Troubleshoot](troubleshooting.md) when corpus health is not green.
