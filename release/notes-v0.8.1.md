# docs-puller v0.8.1

v0.8.1 is a patch release for agent command ergonomics and reproducible
release checks. It does not change search ranking, the public corpus, or the
HTTP API.

## Try it

Open the [public live demo](https://docs-puller-demo.nstranquist.workers.dev).
It needs no account and no API key. It searches 24 reviewed public documents
with SQLite FTS5 and makes zero AI calls.

## Install

With Go 1.26.6 or later:

```sh
go install github.com/nstranquist/docs-puller@v0.8.1
docs-puller version --expect v0.8.1 --json
docs-puller demo --json
```

You can also download a macOS, Linux, or Windows archive from this release.
Verify the selected archive with `checksums.txt` before you run it. See the
[install guide](https://github.com/nstranquist/docs-puller/blob/v0.8.1/docs/user/install.md).

## Fixed

- `--json` and `--compact` can now appear before the top-level command. The
  command still owns and validates each flag. A real subprocess test proves
  the agent-facing invocation.
- VS Code packages now use stored ZIP entries. Identical reviewed input files
  produce identical VSIX bytes across supported Go patch releases.
- Local and hosted Staticcheck gates now read one repository version pin. A
  contract test rejects a copied analyzer version.
- Release synchronization now fails when portfolio notes or verification dates
  still describe an older release.

## Measured evidence

The unchanged reviewed 24-query sample reports 95.83% Hit@1, 100% Hit@5, and
0.979 MRR. Its corpus digest is
`sha256:54d3a0da70352d5be0e998714f1722eefa683189afa96fc37dd8373f7a45ec7b`.
The build and evaluation make zero model calls and use no AI tokens.

## Supply-chain evidence

- The release workflow verifies six deterministic CLI archives, checksums, a
  CycloneDX SBOM, provenance, and the deterministic VS Code package.
- Local publication gates run tests, race tests, static analysis, vulnerability
  and secret scans, fuzz smoke tests, browser journeys, retrieval checks, and a
  clean-clone proof.
- The public demo has separate deployment, readiness, origin-authentication,
  and synthetic monitor evidence.

## Evidence boundary

Passing local and hosted gates proves the tested build and release path. The
public URL and synthetic checks prove availability. They do not prove external
adoption. See the
[changelog](https://github.com/nstranquist/docs-puller/blob/v0.8.1/CHANGELOG.md)
for the complete change list.
