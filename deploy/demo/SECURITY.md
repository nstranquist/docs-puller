# Public demo security model

## Protected assets

- The origin bearer token.
- The Worker rate-key secret.
- The integrity and fixed scope of the public corpus.
- The availability of the public site and search API.
- The privacy of visitor IP addresses and search text.

This demo does not hold customer documents, user accounts, payment data, or
session data.

## Trust boundaries

The browser is untrusted. The Cloudflare Worker validates every public request.
The Fly origin trusts only a correct bearer token. Pulled documentation is
untrusted input, even when it comes from an allowlisted project.

## Controls

- The Worker accepts only `GET`, `HEAD`, and required `OPTIONS` requests.
- Search supports one fixed FTS5 mode, three source IDs, a bounded query, and a
  limit from 1 through 10.
- Document reads require an allowlisted source and a canonical Markdown path.
  The Worker requests at most 32,000 bytes around the selected search-result
  line. The response states the excerpt and full-document byte and line bounds.
- CORS permits only the exact public origin.
- Search and API rate limits use an ephemeral HMAC of the client address. The
  raw address is not stored.
- Analytics record route class, status, latency bucket, result-count bucket,
  source ID, cache result, and build ID. They never record query text, document
  text, bearer tokens, raw IP addresses, or full URLs.
- The Worker limits origin time to 4 seconds and response size to 64 KiB.
- The origin token is never sent to the browser or included in an error.
- The origin container runs as UID/GID 65532. Its binary, corpus, index, and
  application directories are read-only to that identity. Fly uses an
  ephemeral root filesystem with `persist_rootfs = "never"` and no volume.
  The application does not persist query logs.
- The container contains a fixed, hash-locked corpus and a checkpointed SQLite
  index. It has no writable volume.
- The base image is digest-pinned. Go module versions and site dependencies are
  lockfile-pinned.
- CI scans Go vulnerabilities, audits JavaScript dependencies, scans secrets,
  produces an SBOM, and attests the binary and root filesystem.

## Known residuals

- The bearer-authenticated Fly hostname is internet-routable. It is not a
  supported public API and it has no anonymous route.
- Cloudflare and Fly retain provider access logs under their own service
  policies. Application telemetry still excludes query and document text.
- A one-token origin cannot provide overlap during token rotation. Operators
  must rotate in a short maintenance window and run the smoke checks at once.
- The Fly TCP health check proves that the process accepts connections. The
  deployment workflow and external monitor prove authenticated search through
  the full public path.
- Pulled documentation can contain misleading instructions. The UI renders it
  as text and does not execute scripts or Markdown HTML.

Report a vulnerability through the private process in
[`SECURITY.md`](../../SECURITY.md).
