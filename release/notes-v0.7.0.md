# docs-puller v0.7.0

v0.7.0 adds bounded document windows to the authenticated local HTTP API and
uses them to make public-demo previews reliable for large source pages. This is
an additive API release after v0.6.0.

## Install

With Go 1.26 or later:

```sh
go install github.com/nstranquist/docs-puller@v0.7.0
docs-puller version --expect v0.7.0 --json
docs-puller demo --json
```

You can also download a macOS, Linux, or Windows archive from this release.
Verify it with `checksums.txt` before you run it. See the
[install guide](https://github.com/nstranquist/docs-puller/blob/v0.7.0/docs/user/install.md)
for archive and Windows instructions.

## Added

- `GET /api/doc` accepts optional `max_bytes` and `line` values.
- A bounded response contains a Markdown window around the requested line.
- The version contract reports `serve.document-window.v1`.

## Fixed

- The public demo requests the window around a matched search snippet. Large
  pages no longer exceed the Worker's response boundary during preview.
- The external smoke test verifies that the returned window contains the
  matched line and stays inside the public size limit.

## Compatibility and security

The existing full-document response remains the default when `max_bytes` and
`line` are absent. `line` requires `max_bytes`. Authentication, path
validation, source allowlists, and the 32 KiB public preview limit remain in
force.

The release contains six cross-platform CLI archives, SHA-256 checksums, a
CycloneDX SBOM, provenance, a release manifest, and a checksummed VSIX. GitHub
Actions verifies and attests these assets before publication.

## Evidence boundary

The public demo and synthetic smoke prove deployment health. They do not prove
external adoption. See the
[changelog](https://github.com/nstranquist/docs-puller/blob/v0.7.0/CHANGELOG.md)
for the complete change list.
