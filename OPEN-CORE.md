# OPEN-CORE boundary — docs-puller

docs-puller is open-core: the local-first CLI in this repository is open
source (Apache-2.0); the hosted multi-tenant tier is proprietary.

**Open (this repository / local CLI):**

- pull, crawl, index, search, rerank, eval, eval-leaderboard
- single-tenant `serve` — bearer-gated HTTP API + embedded local web UI
- operator config (`config init`, profiles, versioned pins)
- VS Code extension (`vscode-extension/`)
- published eval fixtures (`eval/*.yaml`) and leaderboard HTML

**Closed (proprietary — the hosted Team tier):**

- multi-tenant control plane: auth, billing, corpus provisioning
- hosted ingest and search services
- Team-tier operator console (web app)
- deploy/ops validators tied to hosted operations
- metered agent-commerce retrieval settlement

**The line, in one sentence:** anything a single developer runs on their own
machine against their own corpus is open; anything that operates someone
else's corpus as a service is closed.

PRs are welcome — see `CONTRIBUTING.md`.
