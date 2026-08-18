# Troubleshoot docs-puller

## The shell cannot find the command

Run `go env GOBIN` and `go env GOPATH`. If `GOBIN` is empty, Go installs the
binary in `$(go env GOPATH)/bin`. Add the applicable directory to `PATH`. Then
run `docs-puller version --json`.

## Status reports a missing or stale index

Use one corpus path for pull, reindex, status, search, and serve:

```sh
docs-puller status --out "$corpus"
docs-puller reindex --out "$corpus"
docs-puller status --out "$corpus" --check
```

## A pull reports low-content

Open the recorded URL and the stored Markdown file. Verify that the page sends
the article body without client-side JavaScript.

If the page sends only an application shell, look for a native `.md` page or
`llms.txt`. You can also use a source repository or local documentation
directory. Pull from that source, then reindex.

## Search returns no useful result

Run `status --check`. Then repeat the search without `--source`, `--profile`,
or `--version`. After the broad search works, add one filter at a time.

Use `--scan` for punctuation-heavy identifiers that FTS5 tokenization changes.
If the words must be adjacent and in the same order, use `--exact`.

## Serve refuses the address

A non-loopback `--addr` requires a bearer token. This includes an empty address,
`0.0.0.0`, and `::`. See [Security and privacy](security.md).

## Reranking cannot find a key or embedding index

Remove rerank flags to use local BM25 search without an API key. To use an
embedding provider, configure its key and build the selected embedding index:

```sh
docs-puller embed --out "$corpus" --model text-embedding-3-small
docs-puller status --out "$corpus" --check-embeddings
```

## Pull-pdf rejects the provider pin

Do not bypass a missing or changed executable hash. Run `pdf-doctor` to inspect
the provider, then create and review a new pin.

## Collect a useful report

Include these non-secret outputs in an issue:

```sh
docs-puller version --json
docs-puller status --out "$corpus"
docs-puller demo --json
```

Before you publish the report, remove private corpus paths, document text, query
text, tokens, and environment values.
