# Case study: measurable local documentation retrieval for coding agents

## Summary

`docs-puller` is a local-first Go CLI for documentation retrieval. It mirrors
vendor and project documentation to Markdown, builds a SQLite FTS5 index, and
returns ranked results to people and coding agents. Optional embedding and LLM
reranking are additive. The default lexical path does not need an API key.

I designed and built the CLI, retrieval pipeline, evaluation system, local web
service, VS Code client, adopter guides, and release contract.

## Problem

Coding agents need current documentation, but the usual choices create three
problems:

- Hosted retrieval can be unsuitable for private or offline corpora.
- Search quality is easy to describe and hard to measure.
- A tool that works only in its author's checkout is not an adoptable product.

The goal was a portable retrieval system with direct quality evidence and a
complete first-hour path.

## Constraints

- Keep document storage and lexical search local by default.
- Use pure Go and SQLite FTS5. Do not require CGO.
- Keep AI providers optional and explicit.
- Preserve source URLs and readable Markdown exports.
- Give agents stable JSON, bounded help, and token-aware results.
- Make release claims reproducible and fail closed on version drift.

## Architecture

```text
documentation sources
        |
        v
pull and extract  --->  Markdown corpus  --->  SQLite FTS5 index
                                                |
                                                v
                                strict lexical retrieval
                                + natural-language recall
                                + optional embed or LLM rerank
                                                |
                       +------------------------+------------------+
                       |                        |                  |
                       v                        v                  v
                      CLI                 local HTTP API       VS Code
                       |
                       v
             reproducible evaluation
             Hit@1, Hit@5, MRR, latency, tokens
```

The executable is the integration contract. `docs-puller version --json`
reports the version, commands, and stable capabilities. Downstream tools use
that contract instead of copying this source tree.

## Important engineering decisions

### Local lexical retrieval is the reliable floor

The default search path uses SQLite FTS5 with title, path, and body evidence.
It combines strict matching with a source-scoped natural-language recall tier.
Reciprocal-rank fusion improves semantic lexical recall without making a model
call. Boilerplate-only variant selectors do not receive semantic body credit.

### Evaluation is part of the product

The repository contains public fixtures, a public sample corpus, regression
gates, and frozen holdouts. A ranking change must be measured across all public
fixtures. Failed early holdouts remain documented as tuning data. The final
holdout was frozen before its first scored run.

### Security changes with the bind address

Loopback serving stays simple. A wildcard or unspecified bind requires
authentication. The VS Code client validates its runtime, confines paths, and
does not silently weaken the server boundary.

### A release is a supply-chain artifact

The v0.7.2 contract builds six macOS, Linux, and Windows archives. It also
builds checksums, a CycloneDX SBOM, provenance, and a deterministic VS Code
package. CI uses pinned action revisions and verifies the generated artifacts
before it creates a GitHub release.

## Measured results

All results below use deterministic BM25 and FTS5. They made zero AI calls and
used zero model tokens.

| Evaluation | Queries | Hit@1 | Hit@5 | MRR | Evidence boundary |
| --- | ---: | ---: | ---: | ---: | --- |
| Public sample | 24 | 95.8% | 100% | 0.979 | Anyone can replay the live public-page list; upstream content can drift. |
| Full fixture suite | 459 | 71.5% | 93.5% | 0.810 | Operator-measured on the maintainer's local mirror. |
| Final frozen holdout | 35 | 45.7% | 94.3% | 0.674 | Fixture is public; exact corpus mirror is not. |
| Tuned main regression set | 151 | 84.1% | 100% | 0.900 | Operator-measured regression gate. |

A refreshed Stripe corpus exposed a real drift regression before release. The
final fix changed the 459-query aggregate from 71.0% to 71.5% Hit@1, from
93.2% to 93.5% Hit@5, and from 0.806 to 0.810 MRR. The same change restored the
final holdout from 42.9% to 45.7% Hit@1 without reducing its 94.3% Hit@5.

The local dogfood corpus audit covered 84 sources and 49,275 documents. The
SQLite index matched all 48,819 indexable documents after refresh. These are
local operational measurements, not external adoption evidence.

## Adopter journey

A new user can:

1. Install one tagged Go module or a checksummed release archive.
2. Run `docs-puller demo --json` without a network call or API key.
3. Follow the first-hour guide to create and search a real corpus.
4. Replay the 24-query public benchmark.
5. Inspect the architecture, security, troubleshooting, and uninstall guides.
6. Report a problem through a structured GitHub issue template.

## What this demonstrates

- Go systems engineering around a pure-Go SQLite index.
- Retrieval-quality work with frozen evidence and explicit regression gates.
- Agent ergonomics through stable JSON, compact help, and bounded context.
- Security design that distinguishes local convenience from remote exposure.
- Release engineering with deterministic packages and supply-chain metadata.
- Product judgment about what public evidence can and cannot prove.

## Claim boundaries

- The public sample workflow is replayable. Its live upstream pages can change.
  The larger local-mirror scores are operator measurements.
- Optional AI reranking needs a separately configured provider.
- A GitHub release and launch post do not prove external adoption.
- Local dogfood telemetry does not count as an external user.
- The hosted Team design is proprietary and is outside this open-core case
  study.

## Replay the public proof

```sh
go install github.com/nstranquist/docs-puller@v0.7.2
docs-puller demo --json

corpus="$(mktemp -d)"
docs-puller pull --from eval/sample-corpus/sources.md --out "$corpus"
docs-puller reindex --out "$corpus"
docs-puller eval --fixture eval/sample-corpus/fixture.yaml --out "$corpus"
```

See the [README](README.md), [first-hour guide](docs/user/first-hour.md), and
[release history](CHANGELOG.md) for the complete public evidence.
