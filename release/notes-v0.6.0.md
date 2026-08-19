# docs-puller v0.6.0

v0.6.0 turns docs-puller into a complete, measurable adopter experience. It
adds an isolated first-run demo, stronger natural-language retrieval, full user
guides, reproducible packages, a secure VS Code client, and a public engineering
case study.

## Install

With Go 1.26 or newer:

```sh
go install github.com/nstranquist/docs-puller@v0.6.0
docs-puller version --expect v0.6.0 --json
docs-puller demo --json
```

You can also download a macOS, Linux, or Windows archive from this release.
Verify the archive with `checksums.txt` before you run it. See the
[install guide](https://github.com/nstranquist/docs-puller/blob/v0.6.0/docs/user/install.md)
for archive and Windows instructions.

## Highlights

- Search now combines strict FTS5 matching with source-scoped natural-language
  recall and reciprocal-rank fusion. The deterministic path needs no API key.
- `docs-puller demo` proves pull, index, and search on an isolated local corpus.
- Native Markdown mirroring supports Unity documentation.
- The local search page and VS Code client expose the same authenticated HTTP
  contract.
- New install, first-hour, search, architecture, security, troubleshooting, and
  uninstall guides cover the full local CLI journey.
- The [case study](https://github.com/nstranquist/docs-puller/blob/v0.6.0/CASE_STUDY.md)
  explains the architecture, evaluation discipline, and evidence boundaries.

## Measured retrieval

These measurements use deterministic BM25 and FTS5. They made zero AI calls
and used zero model tokens.

| Evaluation | Queries | Hit@1 | Hit@5 | MRR |
| --- | ---: | ---: | ---: | ---: |
| Public pinned sample | 24 | 95.8% | 100% | 0.979 |
| Full maintainer fixture suite | 459 | 71.5% | 93.5% | 0.810 |
| Final frozen holdout | 35 | 45.7% | 94.3% | 0.674 |
| Tuned main regression set | 151 | 84.1% | 100% | 0.900 |

The public sample is reproducible from the release checkout. The larger results
use the maintainer's local corpus mirror and are operator measurements. The
final holdout was frozen before its first scored run. See
[Measured Retrieval](https://github.com/nstranquist/docs-puller/blob/v0.6.0/README.md#measured-retrieval)
for the replay commands and complete claim boundary.

## Security and supply chain

- Wildcard and unspecified HTTP binds require authentication.
- The VS Code client validates the runtime, confines paths, and preserves the
  server authentication boundary.
- The release contains six cross-platform CLI archives, SHA-256 checksums, a
  CycloneDX SBOM, provenance, a release manifest, and a checksummed VSIX.
- The release workflow builds the distribution twice, verifies every checksum,
  scans for secrets, creates GitHub artifact attestations, verifies those
  attestations, and then creates the release.

## Compatibility

This release keeps the v1 executable contract. Consumers can inspect it with
`docs-puller version --json`. The local CLI remains Apache-2.0. The proprietary
hosted Team design remains outside this repository.

## Evidence boundaries

A public release is not proof of external adoption. Local dogfood, synthetic
queries, and maintainer measurements are not counted as external users. Any
adoption claim will require a direct, consented receipt.

For the complete change list, see the
[changelog](https://github.com/nstranquist/docs-puller/blob/v0.6.0/CHANGELOG.md).
