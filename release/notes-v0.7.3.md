# docs-puller v0.7.3

v0.7.3 is the public-demo discovery and release-toolchain patch for the v0.7
line. It keeps the v0.7.2 portability fixes and adds stricter, reviewable
release evidence.

## Try it

Open the [public live demo](https://docs-puller-demo.darthbitcoin.workers.dev).
It needs no account and no API key. It searches a reviewed snapshot of 24
public SQLite, Go, and PostgreSQL pages with the same Go and SQLite FTS5 engine
as the CLI. It makes zero AI calls.

## Install

With Go 1.26.6 or later:

```sh
go install github.com/nstranquist/docs-puller@v0.7.3
docs-puller version --expect v0.7.3 --json
docs-puller demo --json
```

You can also download a macOS, Linux, or Windows archive from this release.
Verify it with `checksums.txt` before you run it. See the
[install guide](https://github.com/nstranquist/docs-puller/blob/v0.7.3/docs/user/install.md)
for archive and Windows instructions.

## Fixed

- The release manifest can declare an exact Go patch version. Distribution
  builds then reject every different toolchain patch.
- The README and typed Product Passport expose the live demo as the primary
  no-install product proof.

## Supply-chain evidence

- CI and release assets use Go 1.26.6.
- The release workflow builds six deterministic CLI archives, verifies
  checksums, produces a CycloneDX SBOM and provenance, and attests artifacts
  before publication.
- The public-demo workflow builds a deterministic Linux binary and container,
  runs browser journeys, scans HIGH and CRITICAL vulnerabilities, and records
  an SBOM.

## Evidence boundary

The live URL, provider readback, and synthetic checks prove availability and
the tested request path. They do not prove external adoption. See the
[changelog](https://github.com/nstranquist/docs-puller/blob/v0.7.3/CHANGELOG.md)
for the complete change list.
