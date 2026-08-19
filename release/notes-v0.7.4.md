# docs-puller v0.7.4

v0.7.4 is the verified public-demo release for the v0.7 line. It includes the
v0.7.3 discovery and exact-toolchain work, and it fixes a clean-build
reproducibility defect found during final release review.

## Try it

Open the [public live demo](https://docs-puller-demo.darthbitcoin.workers.dev).
It needs no account and no API key. It searches a reviewed snapshot of 24
public SQLite, Go, and PostgreSQL pages with the same Go and SQLite FTS5 engine
as the CLI. It makes zero AI calls.

## Install

With Go 1.26.6 or later:

```sh
go install github.com/nstranquist/docs-puller@v0.7.4
docs-puller version --expect v0.7.4 --json
docs-puller demo --json
```

You can also download a macOS, Linux, or Windows archive from this release.
Verify it with `checksums.txt` before you run it. See the
[install guide](https://github.com/nstranquist/docs-puller/blob/v0.7.4/docs/user/install.md)
for archive and Windows instructions.

## Fixed

- The immutable demo image now generates source manifests only from the
  reviewed corpus lock. Pull-time timestamps cannot change the root filesystem
  archive when the binary, documents, and SQLite index are unchanged.
- The release manifest can declare an exact Go patch version. Distribution
  builds then reject every different toolchain patch.
- The README and typed Product Passport expose the live demo as the primary
  no-install product proof.

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
[changelog](https://github.com/nstranquist/docs-puller/blob/v0.7.4/CHANGELOG.md)
for the complete change list.
