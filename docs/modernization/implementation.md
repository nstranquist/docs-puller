# docs-puller modernization

The active source checkout is `~/tools/docs-puller`. NDev owns advanced research
and subscription routing; docs-puller owns corpus collection and retrieval.
Advanced research will use the single NDev engine, not a second orchestrator.
Paid providers remain explicit opt-in. Search stays usable without NDev.

## Implemented interfaces

`docs-puller context <query> --source SOURCE --max-bytes 16000` searches locally
and emits a schema-versioned evidence pack. `context --path SOURCE/DOC.md
--line 20 --lines 60` reads a specific window. Flags may precede or follow a
query. The byte budget bounds passage text, excluding JSON metadata; it is
not an exact model token budget. No model or API call is made.

Each passage contains a document ID, content hash, passage ID, relative path,
source, optional URL, heading context, and inclusive line range. IDs are SHA-256:
document identity is the corpus-relative path; passage identity also includes
the full document hash and line range. The corpus root scopes document IDs.
Fenced blocks are expanded rather than cut; passages exceeding the remaining
budget are skipped with a reason. Reads are confined with Go's `os.Root`,
including symlink traversal, and capped at 1 MiB per document. Repeated identical
passages are collapsed. Hashes identify the bytes read; concurrent refreshes
must still be detected by consumers before treating later reads as identical.

`docs-puller reconcile --json` compares individual manifest, disk and FTS paths.
It reports orphan files, missing index entries, excluded indexed files,
unavailable indexed files, missing manifest files, and invalid manifest paths.
This is a read-only diagnostic, not repair or transactional refresh. It does
not migrate legacy manifests. It reads one FTS query snapshot; the filesystem
may change concurrently, so repeat diagnostics after active pulls finish.

## Remaining required implementation

- Freeze fixture/corpus fingerprints; separate coverage failures from ranking;
  create independent private/public holdouts. Correct BM25 positional weights
  with retuning and all-fixture non-regression evidence.
- Recoverable generation journals, conditional HTTP, refresh dependencies,
  explicit repair previews, extraction provenance and quality checks.
- Structured passage index, hash/model/chunker-aware embedding invalidation,
  evaluated hybrid retrieval, local reranking and query reformulation.
- Typed inference tasks and budgets through NDev; managed local runtime with
  pressure admission, memory safeguards, on-demand loading and verified shutdown.
- Upgrade NDev research with semantic support statuses, contradiction handling,
  guided comparisons, private resumable sessions and explicit KB promotion.
- Add CLI research bridge, MCP, evidence APIs and localhost workbench. Register
  correlated telemetry and distinguish real, synthetic and evaluation traffic.
- Validate installed paths, migrations, cancellation, process recovery, resource
  pressure, privacy, exports, promotion, and shutdown. Do not claim epic complete
  until those checks and the independent evaluation gates pass.

Research defaults: 5 minutes, 3 rounds, 24 fetched pages, 60,000 observable model
tokens. Modes: local, suggest sources, discover and fetch. Private session data
stays private until explicit promotion; optional auto-file starts disabled.

The existing authenticated HTTP server now exposes `GET /api/evidence` with
`path`, `line`, `lines`, and `max_bytes` query parameters. It uses the same
reader and hashes as the CLI, with at most 65,536 content bytes. The server has
explicit header, read, write, and idle timeouts.

Eval-suite JSON adds `coverage` per fixture: expected-path count, missing-path
count, and queries with no available expected alternative. Historical ranking
metrics are retained without silently dropping unsupported queries.
