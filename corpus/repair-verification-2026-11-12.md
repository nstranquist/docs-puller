# Read-only repair preview verification

Status: 2026-11-12 source landed and normal installed CLI verified.
Owner: Codex session 01a1c82a-4cc8-7950-ad12-380eb2d1ecce; WORK-5.

## Decision 1

Add `reconcile verify --plan FILE --out DIR`. Decode exactly one JSON object,
reject unknown fields, validate schema/read-only/root and the saved plan digest,
then rebuild the current preview and compare both digests. This is an observation,
not a reservation or permission to mutate. A future apply command must recheck
while it owns corpus mutation. Existing plan format remains unchanged.

## Verified evidence

Runner 3 job 146b3458-beac-4845-923e-08c9773cd8ed passed focused repair tests,
vet and Darwin arm64 compilation. Job e011642c-025c-4cf0-8c9c-dcdfe76f662a passed
the full suite (`go test -tags sqlite_fts5 -count=1 ./...`). No local heavy build.

The compiled Mac CLI passed seven checks: fresh preview, same-size file drift,
manifest byte drift, trailing JSON, unknown field, tampered digest and restored
preview. Fixture files were restored and no archive was created. Exact command
outcomes and binary SHA-256 are in [the proof](research/repair-verification-2026-11-12.json).
The earlier harness used lowercase `new` against `New`, producing no mutation;
the corrected probe changed one byte and retained file length. That correction
is preserved in the proof.

Telemetry usage for `docs-puller reconcile` returned no rows on 2026-11-12.
This does not establish zero usage or failure rate. CLI outcomes above are
explicit acceptance evidence, not adoption or long-term reliability evidence.

## Open items

- [x] Verify normal installed entrypoint after guarded source publication.
- [ ] Implement reviewed archive/apply and restore with ownership-bound rechecks.
- [ ] Prove generation crash recovery and immutable reader behavior.
- [ ] Complete retrieval/evaluation/client parity acceptance in WORK-5.

## Session journal

2026-11-12: implemented verification without granting deletion authority. Kept
WORK-5 open because this is one modernization capability, not the complete task.

2026-11-12 installed closeout: source commit `31f11385d98e4b7ef33615ac21f41bba1f88d404`
was rebuilt on runner 3 job f5607a40-381d-4cde-9932-bea861562245. The normal
entrypoint reports that exact commit and `corpus.repair-verify.v1`, accepts the
unchanged preview and rejects same-size content drift. Previous installed bytes
were retained before atomic replacement. [Installed identity and outcomes](research/repair-verification-installed-2026-11-12.json).
