---
title: Search
---

# Search

After a pull and reindex, search the local index.

```sh
docs-puller search "how do I create an FTS5 table" --out "$corpus"
docs-puller search "sqlite fts5" --out "$corpus" --json
```

Useful flags:

- `--limit N`: limit the result list.
- `--json`: emit stable rows for agents and scripts.
- `--compact`: reduce the output size for an agent context.
- `--source NAME`: restrict search to one ingested source.
- `--exact`: search the complete query as one phrase.
- `--scan`: if FTS5 tokenization is not suitable, use a literal file scan.
- `--log-query=false`: do not add this query to the local telemetry log.

Run `docs-puller list --json` to discover source names. If the index is missing
or stale, run `status --check`.

## Keep output small

For an agent, start with a small result set and compact JSON:

```sh
docs-puller search "sqlite fts5" --out "$corpus" \
  --source sqlite --limit 5 --json --compact
```

If paths and titles are sufficient, add `--no-snippets`. If a scoped query
returns no useful result, remove the source filter.

## Local HTTP search

`serve` exposes the same index to the embedded browser UI and JSON API:

```sh
docs-puller serve --out "$corpus"
```

Open `http://127.0.0.1:7799`. If you do not need remote access, keep the default
loopback address. Before you use a non-loopback address, read
[Security and privacy](security.md).

Normal searches write bounded, local telemetry by default. The hosted Team
service is separate and does not receive this log from the open-source CLI.
