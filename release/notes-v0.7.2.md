# docs-puller v0.7.2

v0.7.2 is the publishable security and portability release for the v0.7
line. It uses Go 1.26.6 after the container-image gate found eight fixed HIGH
findings in binaries built with Go 1.26.5. It also normalizes manifest and
corpus paths across operating systems.

The v0.7.0 and v0.7.1 prebuilt releases were stopped before publication when
their fail-closed gates found the toolchain and Windows-test problems. Their
public tags remain immutable history. v0.7.2 contains the corrected source.

## Install

With Go 1.26.6 or later:

```sh
go install github.com/nstranquist/docs-puller@v0.7.2
docs-puller version --expect v0.7.2 --json
docs-puller demo --json
```

You can also download a macOS, Linux, or Windows archive from this release.
Verify it with `checksums.txt` before you run it. See the
[install guide](https://github.com/nstranquist/docs-puller/blob/v0.7.2/docs/user/install.md)
for archive and Windows instructions.

## Fixed

- CI and release assets use the exact Go 1.26.6 version from `go.mod`.
- Logical document paths stay portable across macOS, Linux, and Windows.
- The Windows test asserts the slash-normalized logical-path contract instead
  of the host filesystem separator.
- The demo corpus builder avoids directory synchronization calls on Windows,
  where directory handles do not support them.
- The fail-closed image scan remains required before production deployment.
- Repeated release fuzz smoke uses a fixed execution budget. Daily fuzz jobs
  keep their two-minute runs.

## Included from v0.7.0

- `GET /api/doc` accepts optional `max_bytes` and `line` values.
- The public demo previews a bounded window around the matched line.
- The version contract reports `serve.document-window.v1`.

## Evidence boundary

Release scans and public synthetic smoke checks prove build and deployment
health. They do not prove external adoption. See the
[changelog](https://github.com/nstranquist/docs-puller/blob/v0.7.2/CHANGELOG.md)
for the complete change list.
