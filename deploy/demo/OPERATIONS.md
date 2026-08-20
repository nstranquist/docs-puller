# Public demo operations

## Service identities

- Public Worker: `docs-puller-demo`
- Public URL: `https://docs-puller-demo.nstranquist.workers.dev`
- Origin app: `docs-puller-demo-origin`
- Origin URL: `https://docs-puller-demo-origin.fly.dev`
- Fly primary region: `ord`

The Worker URL is the supported endpoint. Do not publish the Fly URL as an API.

## Required secrets

Provider runtime secrets:

- Fly `DOCS_SERVE_TOKEN`
- Worker `SIDECAR_TOKEN` with the same value
- Worker `RATE_KEY_SECRET` with an independent random value

GitHub Actions deployment secrets:

- `FLY_API_TOKEN`, limited to `docs-puller-demo-origin`
- `CLOUDFLARE_API_TOKEN`, limited to Workers Scripts for the selected account
- `CLOUDFLARE_ACCOUNT_ID`

Repository variables:

- `DOCS_PULLER_DEMO_CD_ENABLED=true` enables deployment after a successful
  `main` CI run.
- `DOCS_PULLER_DEMO_MONITOR_ENABLED=true` enables the five-minute schedule.

Keep both variables absent or false during the first deployment. Enable CD only
after the production environment has its least-privilege provider tokens.
Enable monitoring only after the first public smoke check passes.

Never put these values in a file, command log, issue, artifact, or build
argument.

## Deployment order

The production workflow performs these operations in order:

1. Verify Go modules and the full Go test suite.
2. Install the exact pnpm lock and run all site checks.
3. Copy the reviewed snapshot into two new directories and build both indexes.
4. Require equal index bytes and verify the reviewed content lock.
5. Enforce Hit@1 >= 0.90, Hit@5 = 1.0, MRR >= 0.95, and p99 <= 250 ms.
6. Build the Linux binary twice and require equal SHA-256 digests.
7. Create the deterministic root filesystem and container image.
8. Generate the SBOM and GitHub artifact attestations.
9. Deploy the Fly origin with a commit-based image label and blue-green checks.
10. Prove the old Worker can still reach the new origin.
11. Deploy the Worker with the exact commit, corpus, index, and deployment time.
12. Run public readiness, metadata, source, search, document, header, and page
    smoke checks.

The workflow uses one concurrency group and does not cancel an active
production deployment.

## Manual deployment

First, run the offline snapshot proof and stage the reviewed corpus. Use a new
temporary directory for the staged snapshot:

```sh
make verify-public-snapshot
```

Then use the same verified `deploy/demo/.build` directory as CI:

```sh
flyctl deploy deploy/demo/.build \
  --config "$(pwd)/deploy/demo/fly.toml" \
  --local-only \
  --image-label "$(git rev-parse --short=12 HEAD)" \
  --build-arg "SOURCE_DATE_EPOCH=$(jq -r .source_date_epoch deploy/demo/corpus.lock.json)"
```

Then deploy the Worker from `site/`. Use the account-pinned
`cloudflare.docs-puller.production` profile. Override all dynamic identity
fields:

```sh
pnpm exec wrangler deploy \
  --var "BUILD_ID:manual-$(date -u +%Y%m%dT%H%M%SZ)" \
  --var "BUILD_COMMIT:$(git rev-parse HEAD)" \
  --var "DEPLOYED_AT:$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
  --var "ENGINE_VERSION:$(jq -r .version ../release/manifest.json)"
```

Do not deploy the Worker metadata before the matching origin is healthy.

## Required smoke checks

Verify these public routes:

- `/healthz` returns `200`.
- `/readyz` returns `200` and reports 24 documents and three sources.
- `/api/v1/demo/meta` reports the expected commit and corpus digests.
- `/api/v1/demo/sources` returns only SQLite, Go, and PostgreSQL.
- A fixed FTS5 search returns at least one result and a safe source URL.
- A result document opens through `/api/v1/demo/doc`.
- `/`, `/demo/`, `/method/`, `/robots.txt`, and `/sitemap.xml` return `200`.
- Security headers are present. CORS rejects an unrelated origin.
- The Fly origin returns `401` without a bearer token.

Record the response time, request ID, build ID, commit, corpus digest, and
provider deployment IDs. Do not record the origin token or visitor queries.

## Rollback

### Fly origin

List releases with their image references:

```sh
flyctl releases --app docs-puller-demo-origin --image
```

Redeploy the last verified image without rebuilding it:

```sh
flyctl deploy --app docs-puller-demo-origin \
  --config "$(pwd)/deploy/demo/fly.toml" \
  --image registry.fly.io/docs-puller-demo-origin:VERIFIED_LABEL
```

### Cloudflare Worker

List versions and deployments, then roll back to the last verified version:

```sh
pnpm exec wrangler versions list
pnpm exec wrangler deployments list
pnpm exec wrangler rollback VERIFIED_VERSION_ID
```

For local production work, run Wrangler through the account-pinned
`cloudflare.docs-puller.production` profile. The profile must own only the
`docs-puller-demo` Worker. Do not use an account-wide default profile.

Run every required smoke check after a rollback. Provider success is not proof
that search works.

## Monitoring and objectives

The external GitHub monitor runs every five minutes with one fixed synthetic
query. It checks readiness, metadata, page delivery, search, headers, and
latency. It opens an issue only after two consecutive failed runs.

Operational targets, not a contractual SLA:

- Monthly availability >= 99.5%.
- Public readiness p95 < 500 ms.
- Warm search p95 < 800 ms.
- Public 5xx rate < 1% over 15 minutes.
- No raw query or document text in application telemetry.

Investigate one failed run. Treat two consecutive failures as an incident.
Disable the public Worker or roll back if requests leak protected fields,
authentication fails open, the corpus identity changes without review, or the
5xx rate stays above the target.

## Secret rotation

1. Announce a short maintenance window.
2. Generate a new high-entropy origin token without printing it.
3. Update `DOCS_SERVE_TOKEN` on Fly.
4. Update `SIDECAR_TOKEN` on the Worker immediately.
5. Run the full smoke set.
6. Rotate `RATE_KEY_SECRET` separately when needed.

The provider stores secret values. The repository stores only secret names and
procedures.
