import type {
  DemoClient,
  DocumentResponse,
  MetaResponse,
  SearchResponse,
  SourcesResponse,
} from "@/lib/api/client"

export const corpus = {
  id: "public-sample-v1",
  digest: `sha256:${"1".repeat(64)}`,
  index_digest: `sha256:${"2".repeat(64)}`,
  document_count: 24,
  source_count: 3,
  retrieved_at: "2026-08-18T12:00:00.000Z",
} as const

export const metaFixture: MetaResponse = {
  ok: true,
  schema_version: 1,
  service: "docs-puller-demo",
  engine: { name: "docs-puller", version: "v0.6.0", mode: "fts5" },
  build_id: "test-build",
  commit: "abcdef0123456789",
  deployed_at: "2026-08-18T13:00:00.000Z",
  corpus,
  limits: {
    query_characters: 160,
    results: 10,
    timeout_ms: 4000,
    response_bytes: 65536,
  },
}

export const sourcesFixture: SourcesResponse = {
  ok: true,
  corpus,
  sources: [
    {
      id: "sqlite",
      label: "SQLite",
      document_count: 8,
      homepage: "https://sqlite.org/docs.html",
      license: "Public domain documentation",
    },
    {
      id: "go",
      label: "Go",
      document_count: 8,
      homepage: "https://go.dev/doc/",
      license: "BSD-style documentation license",
    },
    {
      id: "postgresql",
      label: "PostgreSQL",
      document_count: 8,
      homepage: "https://www.postgresql.org/docs/",
      license: "PostgreSQL documentation license",
    },
  ],
}

export const searchFixture: SearchResponse = {
  ok: true,
  query: "fts5 external content tables",
  engine: "docs-puller",
  mode: "fts5",
  elapsed_ms: 2,
  result_count: 1,
  corpus,
  results: [
    {
      title: "SQLite FTS5 Extension",
      source: "sqlite",
      path: "fts5.md",
      url: "https://sqlite.org/fts5.html",
      score: 42,
      snippets: [
        {
          line: 911,
          text: "An FTS5 table may be created as an external content table.",
        },
      ],
    },
  ],
}

export const documentFixture: DocumentResponse = {
  ok: true,
  source: "sqlite",
  path: "fts5.md",
  title: "SQLite FTS5 Extension",
  url: "https://sqlite.org/fts5.html",
  content_type: "text/markdown",
  content:
    "# SQLite FTS5 Extension\n\nExternal content tables use another table for content.",
  bytes: 78,
  corpus,
}

export function createFixtureClient(
  overrides: Partial<DemoClient> = {}
): DemoClient {
  return {
    meta: async () => metaFixture,
    sources: async () => sourcesFixture,
    search: async () => searchFixture,
    document: async () => documentFixture,
    ...overrides,
  }
}
