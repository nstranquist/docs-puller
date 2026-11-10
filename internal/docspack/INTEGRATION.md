# INTEGRATION — docspack in docs-puller (REAL source map, 2026-11-09)

This replaces the earlier stage-mapping draft, which was written without
reading docs-puller's source. Everything below names actual files and
functions in this repo at main `dd623ac`, and describes what the
`docspack-transplant` branch actually does.

## The real pipeline (verified in source)

| Stage | Real location | Notes |
|---|---|---|
| URL discovery | `main.go` pull flow (~L440–510) → `loadSitemap` (`sitemap.go:27`, recursive over sitemap indexes) → `filterURLsCounted` (`sitemap.go:149`, prefix + cap, plus locale/version twin dropping L190–330); llms.txt path at `main.go:455` | Discovery is already mature — sitemap + twin-dedupe. docspack's `DiscoverURLs`/`DiscoverSitemapURLs` (in this package, `sitemap.go`) are NOT wired in and should not replace this. |
| Fetch | `httpGet`/`httpDo` (`main.go`, 3 attempts, 429/5xx retry, Retry-After) | Unchanged. |
| Convert (the seam) | `fetchAndConvert(rawURL)` (`main.go:1170`) — called from `main.go:1075` (pull), `article.go:216` (`fetchArticle` in `processArticle`), `version_policy.go:648` | Body: `extractHTMLTitle` (`extract.go:155`) → `extractMain(host, body)` (`extract.go:186`, goquery, per-host `hostSelectors` + `genericSelectors`, strips `stripWithin` chrome) → `htmltomd.ConvertString` (JohannesKaufmann/html-to-markdown v2) → `cleanMarkdown` (`main.go`, anchor/edit-link fixups) → title H1 prepend via `bodyHasMatchingH1`. |
| Chunking | `searchruntime.SplitEmbeddingTextChunks` (`searchruntime/searchruntime.go:595`) — char-window, paragraph-preferring, no overlap; embedding path in `embed.go` (`chunkBatches`, `runEmbedChunked`). FTS indexes whole documents (`index.go: regenerateIndex`). | There is no heading-aware chunker in the baseline; docspack's chunker is a candidate for the embedding path only, a separate change. |
| Pack assembly | No equivalent exists (no TF-IDF/dedupe/budget pack step anywhere in the repo). | docspack `BuildPack`/`SelectPack` remain available in-package for a future multi-page context-pack feature; deliberately unwired. |

## What this branch wires

- `internal/docspack/` — the docspack package as-is (stdlib only):
  `CleanHTML`/`CleanToText`/`ExtractLandmarkHTML` (clean.go),
  `ChunkSegments`/`ChunkHTML` (chunk.go), `TFIDFScores`/`Jaccard`/
  `SelectPack`/`BuildPack` (pack.go), sitemap helpers (sitemap.go),
  hand tokenizer (tokenizer.go), with its tests + fixtures.
- `docspack_wire.go` (package main) — `docspackEnabled()` +
  `convertWithDocspack([]byte) string`: renders docspack segments as
  Markdown (heading levels preserved, `<pre>` → fenced blocks, `<li>` →
  bullets). Covered by `docspack_wire_test.go`.
- `fetchAndConvert` (`main.go`) — when `DOCS_PULLER_DOCSPACK=1`, the
  conversion step uses `convertWithDocspack` instead of
  `extractMain` + html-to-markdown. Title extraction, `cleanMarkdown`
  fixups, and the H1-prepend behaviour are shared with the default path.
  **Default is OFF** (only the exact value `1` enables it).

## What is deliberately NOT wired, and why

- **Not a replacement for `extractMain` by default.** The baseline
  extractor is host-tuned (40+ per-host selector sets, framework chrome
  rules) and html-to-markdown preserves inline links/images; docspack
  cleaning emits text segments and loses those. The harvest benchmark
  (−25–33% tokens vs *naive* whole-page extraction) does not establish
  superiority over this repo's tuned extractor — that comparison needs a
  corpus diff first (below).
- **Discovery untouched** — baseline `loadSitemap` + filters are
  strictly more capable than docspack's discovery helpers.
- **Chunker/pack unwired** — no baseline consumer exists for cited
  heading-aware chunks or budget packs today; wiring them into
  `SplitEmbeddingTextChunks` would change retrieval behaviour and, per
  AGENTS.md, ranking changes require measured Hit@1/Hit@5/MRR deltas
  from the eval harness, which this branch does not run.

## Exact next steps to land

1. When the commit-delivery queue is healthy, submit THIS branch's diff
   through the lane onto main (branch was pushed for review only; do not
   merge out-of-band).
2. Corpus diff before any default flip: pull 10 representative sources
   twice (default vs `DOCS_PULLER_DOCSPACK=1`) into separate `--out`
   corpora; compare per-page token counts, boilerplate spot-checks,
   code-block survival, and link loss.
3. Run the retrieval eval (`eval/` harness) on both corpora; only flip
   the default if Hit@1/Hit@5/MRR do not regress.
4. Separately: prototype heading-aware chunking behind the embedding
   path (`SplitEmbeddingTextChunks` call sites in `embed.go`) with the
   same eval gate.
