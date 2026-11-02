# docs-puller v0.8.2

v0.8.2 is a patch release. It ships the binaries that v0.8.1 could not
publish, moves the build to a patched Go toolchain, and fixes pull, search,
and status bugs found after v0.8.1. It does not change the reviewed public
demo corpus or the HTTP API.

## Try it

Open the [public live demo](https://docs-puller-demo.nstranquist.workers.dev).
It needs no account and no API key. It searches 24 reviewed public documents
with SQLite FTS5 and makes zero AI calls.

## Install

With Go 1.26.10 or later:

```sh
go install github.com/nstranquist/docs-puller@v0.8.2
docs-puller version --expect v0.8.2 --json
docs-puller demo --json
```

You can also download a macOS, Linux, or Windows archive from this release.
Verify the selected archive with `checksums.txt` before you run it. See the
[install guide](https://github.com/nstranquist/docs-puller/blob/v0.8.2/docs/user/install.md).

## Security

- The build uses Go 1.26.10 (was 1.26.6) and `golang.org/x/net` v0.62.0 (was
  v0.58.0). `govulncheck` found 13 reachable advisories in the Go 1.26.6
  standard library (`net/http`, `crypto/tls`, `crypto/x509`,
  `net/textproto`) and in `x/net`. It now reports none.
- The public demo site moves to Astro 7.4.4, shadcn 4.23.0, and Wrangler
  4.156.0. The high-severity dependency audit had failed the v0.8.1 release
  build, so the v0.8.1 release page has no archives. Use v0.8.2.
- The VS Code extension lockfile resolves `js-yaml`, `markdown-it`, `qs`, and
  `undici` past their high-severity advisories.

## Added

- `status` reports orphan files: `.md` files under a source that no manifest
  entry covers, for example pages left by an interrupted pull.
  `reindex --prune-orphans` deletes them before the rebuild. Sources without
  `manifest.json` are never touched, and files newer than `--orphan-min-age`
  (default 1h) are spared so a running pull is not broken.

## Fixed

- `search` and `search-batch` no longer hang while a pull holds the corpus
  write lock. Telemetry uses its own lock, and `search` prints results first.
- Pulls that add many new pages no longer hold the write lock for minutes in
  the FTS update. Unmapped paths are cleared in one scan per upsert batch.
- Sitemap, llms.txt, page-data, and `--from` pulls drop translated copies
  when the untranslated twin is in the same set.
- DocC crawls no longer deadlock on large archives. v0.8.1 `refresh` is
  affected because it drops `--max`.
- `status` partial-mirror warnings count only replayable pulls that own the
  source, and deleted sources no longer report as partial mirrors.
- `refresh` output and `--source` help now describe the actual behavior.
- `redux.js.org` routes to the `redux` source, because Redux Toolkit moved
  under `redux.js.org/toolkit`.

## Evidence boundary

Passing local and hosted gates proves the tested build and release path. The
public URL and synthetic checks prove availability. They do not prove external
adoption. See the
[changelog](https://github.com/nstranquist/docs-puller/blob/v0.8.2/CHANGELOG.md)
for the complete change list.
