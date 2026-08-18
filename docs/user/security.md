# Security and privacy

docs-puller is local-first. The open-source CLI writes documents, indexes, and
query telemetry to the selected corpus directory. It does not upload that
corpus to the proprietary hosted Team service.

## Local data

If a corpus contains private repositories, internal documentation, or private
queries, treat it as sensitive. Protect these paths with operating-system
permissions and backups:

- `<out>/<source>/`: copied Markdown documents and manifests.
- `<out>/.cache/search.db`: the FTS5 index and document text.
- `<out>/.cache/query-log.jsonl`: bounded local query telemetry.
- `<out>/.cache/embeddings.db`: local embedding metadata and vectors.

Disable logging for one query with `--log-query=false`. Set
`DOCS_PULLER_QUERY_LOG=0` to disable query logging for a process or integration.
Do not publish the query log without review and consent.

## Network pulls

A pull downloads untrusted vendor or repository content. Review the source URL.
If you test a large discovery source, use `--filter` or `--max`. Before you use
`--replace-source` and `--allow-large-prune`, review the authoritative
replacement.

The CLI stores content as data. Do not execute code blocks or commands copied
from a pulled document without review.

## Local server

`serve` binds to `127.0.0.1` by default. If no bearer token is configured, it
refuses every unspecified or non-loopback address.

For a trusted network, create a private token file:

```sh
token_file="$HOME/.docs-puller/serve-token"
umask 077
openssl rand -hex 32 > "$token_file"
docs-puller serve --addr 0.0.0.0 --out "$corpus" \
  --auth-token-file "$token_file"
```

The built-in server does not provide TLS. If traffic leaves the local machine,
use a trusted encrypted tunnel or a TLS reverse proxy. Do not put a bearer token
in shell history, source control, a URL, or a public log.

## Reranking and external providers

Local BM25 search does not need an API key. Optional embedding and LLM rerank
modes can send query or document-derived content to the configured provider.
Before you enable them for a private corpus, review the provider, data policy,
selected model, and command flags.

## PDF sidecars

`pull-pdf` runs external provider binaries. Use `pdf-doctor` and a reviewed
provider pin. The pin must match the source revision, version, and SHA-256 for
each executable.

## Report a vulnerability

Follow [SECURITY.md](../../SECURITY.md). Do not open a public issue that contains
an exploit, token, private document, private query, or private path.
