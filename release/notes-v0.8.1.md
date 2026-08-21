# docs-puller v0.8.1

v0.8.1 is a patch release for retrieval hygiene, authoritative local refreshes,
agent command ergonomics, and reproducible release checks. It does not change
the reviewed public demo corpus or the HTTP API.

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

If you already have a corpus, rebuild its derived FTS5 cache once. v0.8.1 adds
a content fingerprint that supports exact-copy collapse:

```sh
docs-puller reindex --out ~/code/docs
```

You can also download a macOS, Linux, or Windows archive from this release.
Verify the selected archive with `checksums.txt` before you run it. See the
[install guide](https://github.com/nstranquist/docs-puller/blob/v0.8.1/docs/user/install.md).

## Fixed

- Search now collapses exact copies across sources and inside aggregate
  workspace or corpus trees. JSON results report `duplicates_collapsed`.
  Distinct aliases inside one normal vendor source remain visible.
- `pull-local-batch --replace-source` now applies the authoritative replacement
  policy that the flag requests. It prunes removed files and FTS rows, refuses
  unapproved large prune plans, reports prune counts, and handles a source that
  now has zero documents.
- Local batch refreshes reuse unchanged files when a temporary staging root
  changes. They also skip docs-puller's own held-out corpus snapshots to prevent
  recursive ingestion.
- SDK searches now connect `sdk` with `library` paths and prefer a matching
  `libraries/<runtime>` page. macOS configuration-payload searches now prefer
  the Apple device-management source without treating all macOS queries as MDM
  queries.
- A query that explicitly requests an API or CLI now prefers a matching path
  surface. Queries that do not name either interface keep their normal order.
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

On the public 151-query regression fixture against the 181,118-document local
corpus, Hit@1 changed from 84.77% to 86.09%, Hit@3 from 94.04% to 96.69%,
Hit@5 from 96.69% to 100%, Hit@10 from 98.68% to 100%, and MRR from 0.8956 to
0.9160. The evaluation made zero model calls and used zero AI tokens.

A new 30-query fixture was frozen before its first ranked run. None of its
expected documents appears in an earlier checked-in fixture. The first run
reports 60.00% Hit@1, 96.67% Hit@5, 100% Hit@10, 0.7542 MRR, and 593 ms p99 on
a hashed 181,118-document FTS snapshot. After an authoritative refresh to
191,436 indexed documents, the quality metrics are unchanged and p99 is 1.43
seconds. Both runs pass the precommitted gates and use zero model calls and zero
AI tokens. The fixture and run evidence are in
`eval/heldout-v0.8.1-final.yaml` and
`eval/heldout-v0.8.1-first-run.json`.

The unchanged reviewed 24-query sample reports 95.83% Hit@1, 100% Hit@5, and
0.979 MRR. Its corpus digest is
`sha256:54d3a0da70352d5be0e998714f1722eefa683189afa96fc37dd8373f7a45ec7b`.
The sample build and evaluation also make zero model calls and use no AI
tokens.

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
