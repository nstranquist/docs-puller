# Source reconciliation and acceptance

Status: 2026-11-10 source reconciliation passed; site gate admission pending.
Owner: Codex, session 01a1c4c8-a8c0-7490-9cb1-f1beb632d1d6; WORK-333.

## Authority and provenance

The canonical Mac checkout is `/Users/nicostran/tools/docs-puller`, origin
`git@github.com:nstranquist/docs-puller.git`. The muse-workspace copy is historical.
Nico prefers shared Mac and VM writers for remaining audit projects, decision
`d-06gshubojlkporm8co`. The existing manifest VM route is retained as historical
routing evidence; the VM working state has not been inspected in this closeout.

Guarded integration `b233e0bfeb05229a81c9d1e38c825fece1b9ce00` has both parents:
Mac `8192648d7cd5a1fa79748e42d3f22eb4ab296dbc` (15 unique commits) and remote
`dd623ac4b0af84761166f138c413648d6fbdd348` (one UI commit). All 13 incoming paths
are byte-identical to that remote commit. Go files, go.mod and go.sum are unchanged
from the Mac parent. There were no pre-existing dirty paths. The guarded receipt
and exact-byte checks are retained privately as `docs-shared-integration.json`
and `docs-post-integration-verification.json` in the session workspace.

## Decisions and alternatives

D1 (2026-11-10): adopt shared human writers and guarded both-parent integration.
Scores out of 5, adoption cutoff 4: reviewed merge 5 (preserves both histories);
ff-only 2 (cannot represent current divergence); overwrite/rebase 1 (unnecessary
history loss). A conflict or a dirty overlapping path would stop promotion.
The legacy V hint remains for installed bridge compatibility; it does not grant
VM-only human authority or implement shared synchronization.

## Acceptance and limits

- [x] Re-pin the canonical source and preserve both parents.
- [x] Verify all 13 incoming paths and unchanged Go source.
- [x] Recheck H08: wildcard CORS remains; non-loopback bind requires a token,
  configured bearer validation is constant-time, anonymous mode is loopback-only
  (`serve.go`, `serve_auth.go`). No listener was found on default port 7799.
- [ ] Run `pnpm check` from `site` when host admission permits it.
- [ ] Obtain current VM working-state proof before claiming VM convergence.
- [x] Central authority `da71314d9d155c77f85deec3104ae7aa22a2583b` and register `5e65f8bf145cdca283338415b2e3fe0a43f8ef59` landed and were verified.

The first frontend check request used an unregistered workload and did not run.
The corrected `candidate-verify-path` request timed out after 60 seconds waiting
for CPU admission (exit 124); no site check passed. Existing site dependencies
were reused without an install. No browser, server, public demo deployment or
`~/code/docs` mutation occurred. Absence on the default port does not prove no
custom-port listener; sensitive browser/live exposure remains unproved.

Journal: the older audit pin `3c1735c1` is historical and superseded for current
source assessment. Divergence alone was not a correctness defect. H02 is resolved
at source-authority level; H08 remains an exposure question, not a confirmed leak.

Dated continuation: WORK-340 owns the remaining frontend acceptance. A fresh
2026-11-11 05:24 UTC host sample was red at 95.8% CPU, so there was no repeat
local admission loop. A read-only probe of active runner 3 found no `node` or
`pnpm` on PATH; no runtime was installed and no remote gate was submitted.
The site check remains unrun, not failed or passed.
