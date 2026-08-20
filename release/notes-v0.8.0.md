# docs-puller v0.8.0

v0.8.0 adds first-party Jupiter documentation routing and makes the public
demo build independent of live vendor pages. It also fixes the public hostname,
release launch proof, AI-facing project facts, and browser-budget reliability.

## Try it

Open the [public live demo](https://docs-puller-demo.nstranquist.workers.dev).
It needs no account and no API key. It searches a reviewed snapshot of 24
public SQLite, Go, and PostgreSQL pages with the same Go and SQLite FTS5 engine
as the CLI. It makes zero AI calls.

## Install

With Go 1.26.6 or later:

```sh
go install github.com/nstranquist/docs-puller@v0.8.0
docs-puller version --expect v0.8.0 --json
docs-puller demo --json
```

You can also download a macOS, Linux, or Windows archive from this release.
Verify it with `checksums.txt` before you run it. See the
[install guide](https://github.com/nstranquist/docs-puller/blob/v0.8.0/docs/user/install.md)
for archive and Windows instructions.

## Added

- Mintlify pages under `developers.jup.ag/docs` now route to the `jupiter`
  source. Native Markdown pages and OpenAPI YAML mirrors use the same source.
- `ai-info.md` and `llms.txt` publish bounded project facts, install guidance,
  claim limits, and links for AI assistants and search systems.

## Public demo reliability

- Production builds from tracked, reviewed document bytes. Normal CI no
  longer depends on the current response from 24 vendor URLs.
- CI builds two new SQLite indexes from separate snapshot copies and requires
  byte-identical results before it stages the container.
- The weekly live pull is now an upstream-drift detector. It opens one issue
  when content or retrieval quality changes and never updates production.
- The reviewed sample now includes the Go 1.27 language specification. The
  corpus still contains 24 documents from three public sources.
- Browser tests keep the page-level performance limits and have enough total
  time to retain traces and video on a busy host.
- Release synchronization treats source-repository launch proof as
  version-independent. Versioned install contracts still require exact tags.

## Measured evidence

The reviewed 24-query sample reports 95.83% Hit@1, 100% Hit@5, and 0.979 MRR.
The reviewed corpus digest is
`sha256:54d3a0da70352d5be0e998714f1722eefa683189afa96fc37dd8373f7a45ec7b`.
The build makes zero model calls and uses no AI tokens.

## Supply-chain evidence

- GitHub locks the release tag and assets after publication and creates its
  release attestation.
- The release workflow attests six deterministic CLI archives, the CycloneDX
  SBOM, and the deterministic VS Code package.
- The public-demo workflow proves the corpus index and container twice. It also
  runs browser journeys, performance limits, vulnerability scans, and an image
  SBOM.

## Evidence boundary

The public URL, provider readback, and synthetic checks prove availability and
the tested request path. They do not prove external adoption. See the
[changelog](https://github.com/nstranquist/docs-puller/blob/v0.8.0/CHANGELOG.md)
for the complete change list.
