# docs-puller v0.7.1

v0.7.1 is the security and portability rebuild for v0.7.0. It makes the public
live demo an explicit adopter entry point, repairs the Windows runtime path
boundary, and pins release builds to Go 1.26.6. This is a compatible patch
release after v0.7.0.

## Try it

Open the [public live demo](https://docs-puller-demo.darthbitcoin.workers.dev).
It needs no account and no API key. The demo searches a reviewed 24-page public
sample with the same Go and SQLite FTS5 engine as the CLI. It does not use AI.

## Install

With Go 1.26.6 or later:

```sh
go install github.com/nstranquist/docs-puller@v0.7.1
docs-puller version --expect v0.7.1 --json
docs-puller demo --json
```

You can also download a macOS, Linux, or Windows archive from this release.
Verify it with `checksums.txt` before you run it. See the
[install guide](https://github.com/nstranquist/docs-puller/blob/v0.7.1/docs/user/install.md)
for archive and Windows instructions.

## Fixed

- Manifests, APIs, source indexes, scan results, and FTS5 rows use stable `/`
  separators on every operating system.
- Windows read-only SQLite connections use a rooted local file URI.
- Explicit `HOME` environments work for config, profiles, and state paths on
  Windows.
- Windows uses native executable-mode and directory-sync behavior. Only tests
  whose fixtures require POSIX shell semantics are skipped on Windows.

## Security and release engineering

- `go.mod` and the release manifest pin Go 1.26.6.
- An exact patch in the release manifest now requires the exact Go toolchain.
- The release workflow builds six deterministic CLI archives, verifies
  checksums, produces a CycloneDX SBOM and provenance, and attests artifacts
  before publication.
- The public demo workflow builds deterministic Linux binaries and container
  images, runs browser journeys, scans HIGH and CRITICAL vulnerabilities, and
  records an SBOM.

## Evidence boundary

The live URL, deployment readback, and synthetic smoke prove availability and
the tested request path. They do not prove external adoption. See the
[changelog](https://github.com/nstranquist/docs-puller/blob/v0.7.1/CHANGELOG.md)
for the complete change list.
