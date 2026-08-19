# Public demo deployment

This directory owns the production origin for the public docs-puller demo.
The public site and its Cloudflare Worker are in [`site/`](../../site/).

## Runtime shape

```text
browser
  -> Cloudflare Worker and static assets
       -> typed, rate-limited `/api/v1/demo/*` boundary
            -> bearer-authenticated Fly origin
                 -> unmodified `docs-puller serve`
                      -> read-only SQLite FTS5 sample corpus
```

The Worker is the only public application API. The Fly hostname is reachable
because Cloudflare Workers cannot use Fly private networking, but every origin
route requires a bearer token. The Worker removes origin-only fields before it
returns a response.

This is a single-tenant, read-only sample. It does not accept uploads, custom
URLs, private documents, accounts, cookies, or arbitrary search modes. It does
not weaken the open-core boundary in [`OPEN-CORE.md`](../../OPEN-CORE.md).

## Owned artifacts

- `corpus.lock.json` records every reviewed public URL, source license, byte
  count, and SHA-256 digest.
- `cmd/corpus-builder` verifies the lock, checkpoints SQLite, and creates the
  only allowed container build context.
- `Dockerfile` uses a digest-pinned distroless non-root base image.
- `fly.toml` defines the origin service, health check, limits, and blue-green
  deployment strategy.
- `CORPUS.md`, `SECURITY.md`, and `OPERATIONS.md` define the review and operator
  contracts.

The generated `deploy/demo/.build` directory is ignored. It contains only a
deterministic content-addressed `rootfs-<sha256>.tar`, a Dockerfile that names
that exact archive, and a build manifest. The changing archive name prevents a
stale BuildKit local-source snapshot when a same-size binary changes. The builder
refuses any other output path.

## Local verification

Build the release-identified CLI for the Fly runtime twice. Require equal
bytes before you stage it:

```sh
version="$(jq -r .version release/manifest.json)"
commit="$(git rev-parse HEAD)"
ldflags="-s -w -buildid= -X main.releaseIdentity=docs-puller-release:${version}@${commit}"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
  -mod=readonly -tags sqlite_fts5 -trimpath -buildvcs=false \
  -ldflags "$ldflags" -o /tmp/docs-puller-demo-linux-amd64-a .
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
  -mod=readonly -tags sqlite_fts5 -trimpath -buildvcs=false \
  -ldflags "$ldflags" -o /tmp/docs-puller-demo-linux-amd64-b .
cmp /tmp/docs-puller-demo-linux-amd64-a /tmp/docs-puller-demo-linux-amd64-b
```

Pull the fixed public sample, reindex it, and run the published eval floor:

```sh
go build -mod=readonly -tags sqlite_fts5 -trimpath -buildvcs=false \
  -o /tmp/docs-puller-demo-host .
/tmp/docs-puller-demo-host pull \
  --from eval/sample-corpus/sources.md --out /tmp/docs-puller-public-demo
/tmp/docs-puller-demo-host reindex --out /tmp/docs-puller-public-demo
DOCS_PULLER_QUERY_LOG=0 /tmp/docs-puller-demo-host eval \
  --fixture eval/sample-corpus/fixture.yaml \
  --out /tmp/docs-puller-public-demo \
  --min-hit-at-1 0.90 --min-hit-at-5 1.0 --min-mrr 0.95 --max-p99-ms 250
```

Verify and stage only the reviewed corpus:

```sh
go run -tags sqlite_fts5 ./deploy/demo/cmd/corpus-builder \
  --corpus /tmp/docs-puller-public-demo \
  --binary /tmp/docs-puller-demo-linux-amd64-a \
  --build-context deploy/demo/.build
```

Use `source_date_epoch` from `corpus.lock.json` for a reproducible image:

```sh
docker buildx build --load --platform linux/amd64 \
  --build-arg SOURCE_DATE_EPOCH="$(jq -r .source_date_epoch deploy/demo/corpus.lock.json)" \
  --provenance=false --sbom=false \
  --tag docs-puller-demo-origin:local deploy/demo/.build
```

The CI build also creates provenance and SBOM attestations. The local command
turns those exporters off only because the Docker image store cannot retain
attestations.

See [`OPERATIONS.md`](OPERATIONS.md) for deployment, smoke, rollback, and
incident procedures.
