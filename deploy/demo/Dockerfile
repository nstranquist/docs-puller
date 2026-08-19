# Pinned linux/amd64 manifest for distroless static-debian12:nonroot.
FROM gcr.io/distroless/static-debian12:nonroot@sha256:b7ebc675e6df3e26840de28a9d119969806b5542902111bfd111964d0930c08a

LABEL org.opencontainers.image.title="docs-puller public demo origin" \
      org.opencontainers.image.description="Read-only docs-puller Go and SQLite FTS5 public sample origin" \
      org.opencontainers.image.source="https://github.com/nstranquist/docs-puller" \
      org.opencontainers.image.licenses="Apache-2.0"

ADD rootfs.tar /

USER 65532:65532
WORKDIR /app
ENV HOME=/tmp \
    DOCS_PULLER_QUERY_LOG=0 \
    DOCS_PULLER_RUN_CONTEXT=production \
    DOCS_PULLER_QUERY_CLIENT=public-demo
EXPOSE 8080

ENTRYPOINT ["/usr/local/bin/docs-puller", "serve", "--addr", "0.0.0.0", "--port", "8080", "--out", "/app/docs"]
