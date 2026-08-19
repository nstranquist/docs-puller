# docs-puller v0.7.1

v0.7.1 is the security and portability rebuild for v0.7.0. It uses Go
1.26.6 for CI, release assets, and the public demo after the container-image
gate found eight fixed HIGH findings in binaries built with Go 1.26.5. The
v0.7.0 prebuilt release was stopped before publication.

This patch also normalizes manifest and corpus paths across operating systems.
It does not change the public API.

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

- CI reads the exact Go 1.26.6 version from `go.mod`.
- Release and demo binaries use the Go 1.26.6 standard library.
- Logical document paths stay portable across macOS, Linux, and Windows.
- The demo corpus builder avoids directory synchronization calls on Windows,
  where directory handles do not support them.
- The fail-closed image scan remains required before production deployment.
- The repeated release fuzz smoke uses a fixed execution budget. This avoids a
  shutdown deadline race while the daily fuzz jobs keep their two-minute runs.

## Included from v0.7.0

- `GET /api/doc` accepts optional `max_bytes` and `line` values.
- The public demo previews a bounded window around the matched line.
- The version contract reports `serve.document-window.v1`.

## Evidence boundary

Release scans and public synthetic smoke checks prove build and deployment
health. They do not prove external adoption. See the
[changelog](https://github.com/nstranquist/docs-puller/blob/v0.7.1/CHANGELOG.md)
for the complete change list.
