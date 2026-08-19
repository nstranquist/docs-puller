# docs-puller v0.7.6

v0.7.6 is the immutable public-demo release for the v0.7 line. It includes the
v0.7.5 Safari keyboard-scroll accessibility fix and the v0.7.4 clean-build
reproducibility fix.

## Try it

Open the [public live demo](https://docs-puller-demo.darthbitcoin.workers.dev).
It needs no account and no API key. It searches a reviewed snapshot of 24
public SQLite, Go, and PostgreSQL pages with the same Go and SQLite FTS5 engine
as the CLI. It makes zero AI calls.

## Install

With Go 1.26.6 or later:

```sh
go install github.com/nstranquist/docs-puller@v0.7.6
docs-puller version --expect v0.7.6 --json
docs-puller demo --json
```

You can also download a macOS, Linux, or Windows archive from this release.
Verify it with `checksums.txt` before you run it. See the
[install guide](https://github.com/nstranquist/docs-puller/blob/v0.7.6/docs/user/install.md)
for archive and Windows instructions.

## Fixed

- GitHub releases are assembled as verified drafts, then published only after
  every asset is attached. The workflow fails unless GitHub confirms that the
  published release is immutable.
- The overflowing document preview is a named, keyboard-focusable scroll
  region. Browser tests exercise real overflow, keyboard scrolling, and axe on
  desktop and mobile viewports.
- The immutable demo image generates source manifests only from the reviewed
  corpus lock. Pull-time timestamps cannot change the root filesystem archive
  when the binary, documents, and SQLite index are unchanged.

## Supply-chain evidence

- GitHub locks the release tag and assets after publication and creates its
  release attestation.
- The release workflow also attests six deterministic CLI archives, the
  CycloneDX SBOM, and the deterministic VS Code package.
- The public-demo workflow rebuilds the reviewed corpus, proves the container
  twice, runs browser journeys and performance budgets, scans HIGH and CRITICAL
  vulnerabilities, and records an image SBOM.

## Evidence boundary

The live URL, provider readback, and synthetic checks prove availability and
the tested request path. They do not prove external adoption. See the
[changelog](https://github.com/nstranquist/docs-puller/blob/v0.7.6/CHANGELOG.md)
for the complete change list.
