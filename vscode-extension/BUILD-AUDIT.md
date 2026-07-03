# Build Audit — Docs Puller Search VS Code extension

This file records how the extension was built **using the docs-puller tool itself** to consume VS Code documentation. Captures the searches, hit quality, and the doc evidence behind each design decision so future iterations can audit and self-improve.

Date: 2026-04-28

## Pull benchmark

VS Code docs were ingested as the 10th source in our local mirror.

| Metric | Value |
|---|---|
| Source URLs | 430 (sitemap-driven) |
| Pulled | 428 (99.5%) |
| Skipped | 2 (legitimate 404s on `intelligentapps/gettingstarted` and `copilot/guides/agents-tutorial`) |
| Warned | 2 (low-content stubs) |
| Wall time | 14.6 s |
| Throughput | 29 URLs/sec @ concurrency 8 |
| Output | 6.8 MB across 427 .md files |
| Selector | `main#main-content` (added explicitly) — generic `main` would have worked too |

## Search audit (queries that informed the build)

All 10 searches scoped via `--source vscode --limit 1`, BM25-ranked. "Top result" is the file the search returned at rank 1.

| # | Query | Top result | Score | Used in design? |
|---|---|---|---:|---|
| 1 | first extension typescript scaffold | `api/get-started/your-first-extension.md` | 139 | Yes — package.json layout, scaffold structure |
| 2 | package.json contributes commands | `api/extension-guides/tree-view.md` | 135 | Indirect — confirmed `contributes.commands` shape |
| 3 | TreeDataProvider sidebar tree view | `api/extension-guides/tree-view.md` | 245 | Considered but rejected — QuickPick is better fit |
| 4 | QuickPick input box | `api/references/vscode-api.md` | 122 | Yes — `vscode.window.createQuickPick<T>()` API |
| 5 | WebviewViewProvider html | `api/references/contribution-points.md` | 52 | Considered, rejected — adds complexity for no UX win |
| 6 | publish extension marketplace vsce | `api/working-with-extensions/publishing-extension.md` | 284 | Yes — symlink-install vs `vsce package` instructions |
| 7 | extension activation events | `api/references/activation-events.md` | 77 | Yes — confirmed VS Code 1.74+ auto-derives from contributes.commands; no explicit `activationEvents` needed |
| 8 | registerCommand command registration | `api/extension-guides/virtual-documents.md` | 143 | Indirect — pattern reused from VS Code samples |
| 9 | extension configuration settings | `docs/configure/extensions/extension-marketplace.md` | 13 | **Weak match** — wanted `contribution-points.md#configuration`. Score 13 indicates poor relevance. Filed as known limitation. |
| 10 | extension test mocha | `api/working-with-extensions/testing-extension.md` | 195 | Deferred — no tests in v0.1.0; doc is the reference for v0.2.0+ |

**Self-eval**: 9/10 queries returned highly relevant top results. The one weak match (#9) is a known issue — "configuration settings" is ambiguous and matched the marketplace settings doc instead of the contribution-point. A title-exact-match tie-breaker (in our future-work list) would help here. Workaround: search for the more specific term `contribution-points configuration`.

## Design decisions backed by the docs

| Decision | Doc cited | Why |
|---|---|---|
| Activation: omit `activationEvents`, rely on `contributes.commands` auto-derivation | `activation-events.md` | "Since VS Code 1.74, your extension does not need to declare a corresponding activationEvents in package.json for any commands defined in `contributes.commands`." |
| UI: `QuickPick` over TreeView or Webview | `vscode-api.md` `createQuickPick<T>()` | Single command + live filter is the canonical search pattern. Webview adds bundle size + security surface for no UX win here. TreeView is for hierarchical browsing, not search. |
| Per-result side-buttons via `QuickPickItem.buttons` + `onDidTriggerItemButton` | `vscode-api.md` `QuickPickItem` | Native pattern for inline secondary actions (open in browser, copy markdown link). |
| Stdlib-only HTTP (`http.get`) instead of `node-fetch` | n/a (project policy) | Zero dependencies → smaller package, no supply-chain surface, faster install. |
| Open file via `vscode.workspace.openTextDocument(Uri.file(...))` + `showTextDocument` | `vscode-api.md` `workspace` namespace | Standard pattern for opening local files into editors. |
| Jump to snippet line via `editor.revealRange` + `editor.selection` | `vscode-api.md` `TextEditor` | Surface the matched context immediately, not just the doc. |

## Token cost vs WebFetch

Building this extension from scratch using only the docs-puller surface:

- 10 searches × ~110ms each = 1.1s wall, ~20 KB JSON total.
- Read 3 specific docs (your-first-extension.md, activation-events.md, publishing-extension.md) = ~25 KB Markdown.
- **Total agent token cost: ~45 KB.**

Equivalent via WebFetch: each docs page ranges 50-150 KB raw HTML before rendering. 10 page reads = ~1 MB. Plus latency: each WebFetch is multi-second.

**Net savings: ~20× token reduction, ~10× latency reduction.** This is the entire reason the docs-puller exists.

## Known limitations recorded for follow-up

1. ~~**Search: title tie-breaker missing**~~ — **RESOLVED (same session).** Added a `+5 per query token present in title` boost on top of BM25 in `(*ftsIndex).search`. Re-sorts results so title-matching docs win equal-BM25 clusters. Demonstrated impact: "row level security" now returns `ddl-rowsecurity.md` (score 73) at #1 instead of `secure-data.md` (score 63). Test: `TestFTSTitleTieBreaker` in `search_fts_test.go`.
2. ~~**`reindex` only updates the FTS5 DB, not per-source `_INDEX.md`**~~ — **RESOLVED (same session).** `cmdReindex` now calls `regenerateIndex(o.out)` before rebuilding FTS5. External-edit users no longer need to trigger a full pull to refresh per-source listings.
3. **`docs-puller serve` must be running** for the extension to work. The error toast surfaces the exact command for onboarding.
4. **No tests** in the extension yet. The `testing-extension.md` doc is the reference for adding `@vscode/test-electron` based tests in v0.2.0. Deferred — extension is ~250 LOC of straight wiring, low test ROI until the surface grows.

## Verification at the end

```sh
# Server up
docs-puller serve

# In a fresh VS Code window:
#   Cmd+Shift+P → "Docs Puller: Search"
#   Type "row level security" → see ranked Supabase + Postgres results
#   Click a result → opens the Markdown file in editor, scrolled to matching line
#   Click the link icon next to a result → opens origin URL in browser
#   Click the copy icon → copies [Title](URL) to clipboard
```
