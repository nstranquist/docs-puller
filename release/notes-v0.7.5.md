# docs-puller v0.7.5

v0.7.5 makes the live document preview accessible to keyboard users in Safari.
The preview is now a named, focusable scroll region. The browser test uses a
long document, moves focus into the region, scrolls it with the keyboard, and
runs axe against the open dialog.

The defect was found by the final live-browser audit. The original test
document was too short to overflow, so the earlier gate could not detect it.
This release also includes the v0.7.4 deterministic demo-manifest fix and the
v0.7.3 release-toolchain, discovery, and browser performance-budget work.

## Try it

Open the [public live demo](https://docs-puller-demo.darthbitcoin.workers.dev).
It needs no account and no API key. It searches a reviewed snapshot of 24
public SQLite, Go, and PostgreSQL pages with the same Go and SQLite FTS5 engine
as the CLI. It makes zero AI calls.

## Install

With Go 1.26.6 or later:

```sh
go install github.com/nstranquist/docs-puller@v0.7.5
docs-puller version --expect v0.7.5 --json
docs-puller demo --json
```

You can also download a macOS, Linux, or Windows archive from this release.
Verify it with `checksums.txt` before you run it. See the
[install guide](https://github.com/nstranquist/docs-puller/blob/v0.7.5/docs/user/install.md)
for archive and Windows instructions.

## Fixed

- Give the overflowing document preview a keyboard-focusable region.
- Give the region an accessible name for assistive technology.
- Test keyboard focus and scrolling with a document that exceeds the preview
  height.
- Run serious and critical axe checks while the overflowing preview is open on
  desktop and mobile viewports.

## Supply-chain evidence

- CI and release assets use Go 1.26.6.
- The release workflow builds six deterministic CLI archives, verifies
  checksums, produces a CycloneDX SBOM and provenance, and attests artifacts
  before publication.
- The public-demo workflow rebuilds the reviewed corpus, proves the container
  twice, runs browser journeys and performance budgets, scans HIGH and CRITICAL
  vulnerabilities, and records an SBOM.

## Evidence boundary

The live URL, provider readback, and synthetic checks prove availability and
the tested request path. They do not prove external adoption. See the
[changelog](https://github.com/nstranquist/docs-puller/blob/v0.7.5/CHANGELOG.md)
for the complete change list.
