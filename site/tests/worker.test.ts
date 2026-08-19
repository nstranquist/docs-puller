// @vitest-environment node

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest"

import {
  handleRequest,
  type AnalyticsBinding,
  type CacheLike,
  type Env,
  type ExecutionContextLike,
  type RateLimitBinding,
  type WorkerDependencies,
} from "../worker/index"

const corpusDigest = `sha256:${"1".repeat(64)}`
const indexDigest = `sha256:${"2".repeat(64)}`

class TestContext implements ExecutionContextLike {
  readonly promises: Promise<unknown>[] = []

  waitUntil(promise: Promise<unknown>): void {
    this.promises.push(promise)
  }

  async flush(): Promise<void> {
    await Promise.all(this.promises)
  }
}

class MemoryCache implements CacheLike {
  private readonly responses = new Map<string, Response>()

  async match(request: Request): Promise<Response | undefined> {
    return this.responses.get(request.url)?.clone()
  }

  async put(request: Request, response: Response): Promise<void> {
    this.responses.set(request.url, response.clone())
  }
}

interface Harness {
  env: Env
  context: TestContext
  dependencies: WorkerDependencies
  originFetch: ReturnType<typeof vi.fn<(request: Request) => Promise<Response>>>
  searchLimiter: ReturnType<typeof vi.fn<RateLimitBinding["limit"]>>
  apiLimiter: ReturnType<typeof vi.fn<RateLimitBinding["limit"]>>
  analyticsWrite: ReturnType<typeof vi.fn<AnalyticsBinding["writeDataPoint"]>>
  assetFetch: ReturnType<typeof vi.fn<(request: Request) => Promise<Response>>>
  cache: MemoryCache
}

function createHarness(
  originHandler: (request: Request) => Promise<Response> = defaultOrigin
): Harness {
  const originFetch = vi.fn(originHandler)
  const searchLimiter = vi.fn(async () => ({ success: true }))
  const apiLimiter = vi.fn(async () => ({ success: true }))
  const analyticsWrite = vi.fn()
  const assetFetch = vi.fn(
    async () =>
      new Response("<h1>asset</h1>", {
        status: 200,
        headers: { "Content-Type": "text/html; charset=utf-8" },
      })
  )
  const cache = new MemoryCache()
  const env: Env = {
    ASSETS: { fetch: assetFetch },
    SEARCH_RATE_LIMITER: { limit: searchLimiter },
    API_RATE_LIMITER: { limit: apiLimiter },
    DEMO_ANALYTICS: { writeDataPoint: analyticsWrite },
    SIDECAR_URL: "https://docs-puller-demo-origin.fly.dev",
    SIDECAR_TOKEN: "origin-secret",
    RATE_KEY_SECRET: "rate-key-secret-with-enough-entropy",
    PUBLIC_ORIGIN: "https://docs-puller-demo.nstranquist.workers.dev",
    BUILD_ID: "build-20260818",
    BUILD_COMMIT: "abcdef0123456789",
    DEPLOYED_AT: "2026-08-18T13:00:00.000Z",
    ENGINE_VERSION: "v0.6.0",
    CORPUS_DIGEST: corpusDigest,
    CORPUS_INDEX_DIGEST: indexDigest,
    CORPUS_RETRIEVED_AT: "2026-08-18T12:00:00Z",
  }
  return {
    env,
    context: new TestContext(),
    dependencies: {
      fetch: originFetch,
      cache,
      now: () => new Date("2026-08-18T14:00:00.000Z"),
      randomUUID: () => "00000000-0000-4000-8000-000000000001",
    },
    originFetch,
    searchLimiter,
    apiLimiter,
    analyticsWrite,
    assetFetch,
    cache,
  }
}

function request(path: string, init: RequestInit = {}): Request {
  const headers = new Headers(init.headers)
  if (!headers.has("CF-Connecting-IP"))
    headers.set("CF-Connecting-IP", "203.0.113.42")
  return new Request(
    `https://docs-puller-demo.nstranquist.workers.dev${path}`,
    {
      ...init,
      headers,
    }
  )
}

function json(
  body: unknown,
  status = 200,
  headers: HeadersInit = {}
): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json", ...headers },
  })
}

async function responseJSON<T>(response: Response): Promise<T> {
  return (await response.json()) as T
}

async function defaultOrigin(originRequest: Request): Promise<Response> {
  expect(originRequest.headers.get("Authorization")).toBe(
    "Bearer origin-secret"
  )
  const url = new URL(originRequest.url)
  if (url.pathname === "/api/status") {
    return json({
      ok: true,
      apiVersion: "v1",
      root: "/app/docs",
      totalDocs: 24,
      sources: 3,
    })
  }
  if (url.pathname === "/api/doc") {
    expect(url.searchParams.get("max_bytes")).toBe("32000")
    const focusLine = Number(url.searchParams.get("line") ?? "1")
    return json({
      source: "sqlite",
      path: "fts5.md",
      title: "SQLite FTS5 Extension",
      url: "https://sqlite.org/fts5.html",
      content: "# SQLite FTS5\n\nExternal content tables.",
      bytes: 39,
      total_bytes: 165924,
      truncated: true,
      start_line: Math.max(1, focusLine - 10),
      end_line: focusLine + 10,
      total_lines: 3100,
    })
  }
  return json({
    query: url.searchParams.get("q"),
    mode: "fts5",
    scanned: 24,
    elapsed_ms: 2,
    root: "/app/docs",
    results: [
      {
        path: "sqlite/fts5.md",
        source: "sqlite",
        source_family: "internal-metadata-must-not-leak",
        title: "SQLite FTS5 Extension",
        url: "https://sqlite.org/fts5.html#external_content_tables",
        score: 42,
        snippets: [
          {
            line: 911,
            text: "An FTS5 table may be created as an external content table.",
          },
        ],
      },
    ],
  })
}

async function run(
  harness: Harness,
  path: string,
  init?: RequestInit
): Promise<Response> {
  return handleRequest(
    request(path, init),
    harness.env,
    harness.context,
    harness.dependencies
  )
}

describe("public demo Worker", () => {
  beforeEach(() => vi.useRealTimers())
  afterEach(() => vi.restoreAllMocks())

  it("serves liveness without the origin, rate limiter, or a query leak", async () => {
    const harness = createHarness()
    const response = await run(harness, "/healthz")
    expect(response.status).toBe(200)
    expect(await responseJSON(response)).toEqual({
      ok: true,
      service: "docs-puller-demo",
      build_id: "build-20260818",
    })
    expect(harness.originFetch).not.toHaveBeenCalled()
    expect(harness.searchLimiter).not.toHaveBeenCalled()
    expect(response.headers.get("Content-Security-Policy")).toContain(
      "frame-ancestors 'none'"
    )
    expect(response.headers.get("X-Request-ID")).toBe(
      "00000000-0000-4000-8000-000000000001"
    )
  })

  it("serves static assets through the bound asset handler with security headers", async () => {
    const harness = createHarness()
    const response = await run(harness, "/method/")
    expect(response.status).toBe(200)
    expect(await response.text()).toBe("<h1>asset</h1>")
    expect(harness.assetFetch).toHaveBeenCalledOnce()
    expect(response.headers.get("X-Frame-Options")).toBe("DENY")
    expect(response.headers.get("Cross-Origin-Opener-Policy")).toBe(
      "same-origin"
    )
  })

  it("routes machine-readable product files through ASSETS", async () => {
    const harness = createHarness()
    for (const path of ["/llms.txt", "/ai-info.md"]) {
      harness.assetFetch.mockClear()
      const response = await run(harness, path)
      expect(response.status, path).toBe(200)
      expect(harness.assetFetch, path).toHaveBeenCalledOnce()
      const requested = harness.assetFetch.mock.calls[0]?.[0] as Request
      expect(new URL(requested.url).pathname, path).toBe(path)
    }
  })

  it("allows only the exact public origin for browser API access", async () => {
    const harness = createHarness()
    const denied = await run(harness, "/api/v1/demo/meta", {
      headers: { Origin: "https://attacker.example" },
    })
    expect(denied.status).toBe(403)
    expect(denied.headers.has("Access-Control-Allow-Origin")).toBe(false)

    const allowed = await run(harness, "/api/v1/demo/meta", {
      headers: { Origin: harness.env.PUBLIC_ORIGIN },
    })
    expect(allowed.status).toBe(200)
    expect(allowed.headers.get("Access-Control-Allow-Origin")).toBe(
      harness.env.PUBLIC_ORIGIN
    )

    const preflight = await run(harness, "/api/v1/demo/search", {
      method: "OPTIONS",
      headers: { Origin: harness.env.PUBLIC_ORIGIN },
    })
    expect(preflight.status).toBe(204)
    expect(preflight.headers.get("Access-Control-Allow-Methods")).toBe(
      "GET, OPTIONS"
    )
  })

  it("publishes only validated build and corpus metadata", async () => {
    const harness = createHarness()
    const response = await run(harness, "/api/v1/demo/meta")
    const body = await responseJSON<Record<string, unknown>>(response)
    expect(response.status).toBe(200)
    expect(body).toMatchObject({
      ok: true,
      service: "docs-puller-demo",
      build_id: "build-20260818",
      commit: "abcdef0123456789",
      corpus: {
        document_count: 24,
        source_count: 3,
        retrieved_at: "2026-08-18T12:00:00.000Z",
      },
    })
    expect(JSON.stringify(body)).not.toContain("SIDECAR")
    expect(JSON.stringify(body)).not.toContain("/app/docs")
    expect(harness.originFetch).not.toHaveBeenCalled()
  })

  it("fails closed when deployment identity is missing or malformed", async () => {
    const harness = createHarness()
    harness.env.CORPUS_DIGEST = "not-a-digest"
    const badCorpus = await run(harness, "/api/v1/demo/meta")
    expect(badCorpus.status).toBe(503)
    expect(await responseJSON(badCorpus)).toMatchObject({
      error: { code: "internal_error" },
    })

    harness.env.CORPUS_DIGEST = corpusDigest
    harness.env.BUILD_COMMIT = "not-a-commit"
    const badBuild = await run(harness, "/api/v1/demo/meta")
    expect(badBuild.status).toBe(503)
  })

  it("returns the closed three-source catalog without an origin root", async () => {
    const harness = createHarness()
    const response = await run(harness, "/api/v1/demo/sources")
    const body = await responseJSON<{
      sources: Array<{ id: string; document_count: number }>
    }>(response)
    expect(body.sources).toEqual([
      expect.objectContaining({ id: "sqlite", document_count: 8 }),
      expect.objectContaining({ id: "go", document_count: 8 }),
      expect.objectContaining({ id: "postgresql", document_count: 8 }),
    ])
    expect(JSON.stringify(body)).not.toContain("root")
  })

  it("normalizes a search and returns only the public DTO", async () => {
    const harness = createHarness()
    const response = await run(
      harness,
      "/api/v1/demo/search?q=%EF%BC%A6%EF%BC%B4%EF%BC%B35%20%20external%20content%20tables&source=sqlite&limit=6&mode=fts5"
    )
    const body = await responseJSON<{
      query: string
      mode: string
      result_count: number
      results: Array<Record<string, unknown>>
    }>(response)
    expect(response.status).toBe(200)
    expect(body.query).toBe("FTS5 external content tables")
    expect(body.mode).toBe("fts5")
    expect(body.result_count).toBe(1)
    expect(body.results[0]).toEqual({
      title: "SQLite FTS5 Extension",
      source: "sqlite",
      path: "fts5.md",
      url: "https://sqlite.org/fts5.html#external_content_tables",
      score: 42,
      snippets: [
        {
          line: 911,
          text: "An FTS5 table may be created as an external content table.",
        },
      ],
    })
    expect(JSON.stringify(body)).not.toContain("source_family")
    expect(JSON.stringify(body)).not.toContain("/app/docs")

    const originURL = new URL(harness.originFetch.mock.calls[0]?.[0].url ?? "")
    expect(originURL.searchParams.get("q")).toBe("FTS5 external content tables")
    expect(originURL.searchParams.get("source")).toBe("sqlite")
    expect(originURL.searchParams.get("limit")).toBe("6")
  })

  it.each([
    ["unknown field", "/api/v1/demo/search?q=sqlite&debug=true"],
    ["duplicate field", "/api/v1/demo/search?q=sqlite&q=go"],
    ["short query", "/api/v1/demo/search?q=x"],
    ["bad source", "/api/v1/demo/search?q=sqlite&source=private"],
    ["bad limit", "/api/v1/demo/search?q=sqlite&limit=11"],
    ["bad mode", "/api/v1/demo/search?q=sqlite&mode=scan"],
  ])("rejects %s before the origin", async (_label, path) => {
    const harness = createHarness()
    const response = await run(harness, path)
    expect(response.status).toBe(400)
    expect(harness.originFetch).not.toHaveBeenCalled()
  })

  it("fails closed when the origin returns a private source or a non-FTS mode", async () => {
    const privateHarness = createHarness(async () =>
      json({
        query: "private",
        mode: "fts5",
        elapsed_ms: 1,
        results: [
          {
            path: "private/secret.md",
            source: "private",
            title: "Secret",
            url: "https://example.com/secret",
            score: 1,
            snippets: [],
          },
        ],
      })
    )
    const privateResponse = await run(
      privateHarness,
      "/api/v1/demo/search?q=private"
    )
    expect(privateResponse.status).toBe(502)
    expect(JSON.stringify(await responseJSON(privateResponse))).not.toContain(
      "Secret"
    )

    const scanHarness = createHarness(async () =>
      json({ query: "sqlite", mode: "scan", elapsed_ms: 1, results: [] })
    )
    const scanResponse = await run(scanHarness, "/api/v1/demo/search?q=sqlite")
    expect(scanResponse.status).toBe(502)
  })

  it("rejects oversized origin responses while reading the body", async () => {
    const harness = createHarness(async () =>
      json({
        query: "sqlite",
        mode: "fts5",
        elapsed_ms: 1,
        results: [],
        padding: "x".repeat(70_000),
      })
    )
    const response = await run(harness, "/api/v1/demo/search?q=sqlite")
    expect(response.status).toBe(502)
    expect(await responseJSON(response)).toMatchObject({
      ok: false,
      error: { code: "origin_invalid" },
    })
  })

  it("rejects non-JSON and malformed origin responses", async () => {
    const textHarness = createHarness(
      async () =>
        new Response("not json", {
          headers: { "Content-Type": "text/plain" },
        })
    )
    const textResponse = await run(textHarness, "/api/v1/demo/search?q=sqlite")
    expect(textResponse.status).toBe(502)

    const malformedHarness = createHarness(
      async () =>
        new Response("{", {
          headers: { "Content-Type": "application/json" },
        })
    )
    const malformedResponse = await run(
      malformedHarness,
      "/api/v1/demo/search?q=sqlite"
    )
    expect(malformedResponse.status).toBe(502)
  })

  it("returns a four-second timeout without an unhandled origin error", async () => {
    const harness = createHarness(
      (originRequest) =>
        new Promise((_resolve, reject) => {
          originRequest.signal.addEventListener("abort", () =>
            reject(new DOMException("aborted", "AbortError"))
          )
        })
    )
    harness.dependencies.originTimeoutMS = 5
    const response = await run(harness, "/api/v1/demo/search?q=sqlite")
    expect(response.status).toBe(504)
    expect(await responseJSON(response)).toMatchObject({
      error: { code: "origin_timeout" },
    })
  })

  it("rate-limits with an opaque ephemeral key and Retry-After", async () => {
    const harness = createHarness()
    harness.env.SEARCH_RATE_LIMITER = {
      limit: vi.fn(async () => ({ success: false })),
    }
    const response = await run(harness, "/api/v1/demo/search?q=sqlite")
    expect(response.status).toBe(429)
    expect(response.headers.get("Retry-After")).toBe("60")
    expect(harness.originFetch).not.toHaveBeenCalled()
    const key = (
      harness.env.SEARCH_RATE_LIMITER.limit as ReturnType<typeof vi.fn>
    ).mock.calls[0]?.[0].key as string
    expect(key).not.toContain("203.0.113.42")
    expect(key).toHaveLength(32)
  })

  it("returns document Markdown as inert JSON and rejects traversal", async () => {
    const harness = createHarness()
    const traversal = await run(
      harness,
      "/api/v1/demo/doc?source=sqlite&path=..%2Fsecret.md"
    )
    expect(traversal.status).toBe(400)
    expect(harness.originFetch).not.toHaveBeenCalled()

    const response = await run(
      harness,
      "/api/v1/demo/doc?source=sqlite&path=fts5.md"
    )
    const body = await responseJSON<{
      content_type: string
      content: string
      path: string
    }>(response)
    expect(response.status).toBe(200)
    expect(body.content_type).toBe("text/markdown")
    expect(body.content).toContain("# SQLite FTS5")
    expect(body.path).toBe("fts5.md")
    expect(body).toMatchObject({
      bytes: 39,
      total_bytes: 165924,
      truncated: true,
      start_line: 1,
      end_line: 11,
      total_lines: 3100,
    })
    expect(response.headers.get("Content-Type")).toContain("application/json")

    const focused = await run(
      harness,
      "/api/v1/demo/doc?source=sqlite&path=fts5.md&line=911"
    )
    expect(focused.status).toBe(200)
    expect(await responseJSON(focused)).toMatchObject({
      start_line: 901,
      end_line: 921,
    })
  })

  it("maps a missing reviewed document without returning the origin body", async () => {
    const harness = createHarness(async () =>
      json({ error: "origin detail that must not leak" }, 404)
    )
    const response = await run(
      harness,
      "/api/v1/demo/doc?source=sqlite&path=fts5.md"
    )
    expect(response.status).toBe(404)
    expect(JSON.stringify(await responseJSON(response))).not.toContain(
      "origin detail"
    )
  })

  it("verifies readiness without exposing the origin root", async () => {
    const harness = createHarness()
    const response = await run(harness, "/readyz")
    const body = await responseJSON<Record<string, unknown>>(response)
    expect(response.status).toBe(200)
    expect(body).toMatchObject({
      ok: true,
      origin: "ready",
      checked_at: "2026-08-18T14:00:00.000Z",
    })
    expect(JSON.stringify(body)).not.toContain("/app/docs")

    const badHarness = createHarness(async () =>
      json({
        ok: true,
        apiVersion: "v1",
        root: "/app/docs",
        totalDocs: 25,
        sources: 3,
      })
    )
    const badResponse = await run(badHarness, "/readyz")
    expect(badResponse.status).toBe(503)
  })

  it("caches only the sanitized successful anonymous response", async () => {
    const harness = createHarness()
    const path = "/api/v1/demo/search?q=sqlite&limit=6"
    const first = await run(harness, path)
    await harness.context.flush()
    const second = await run(harness, path)
    expect(first.headers.get("X-Docs-Puller-Cache")).toBe("MISS")
    expect(second.headers.get("X-Docs-Puller-Cache")).toBe("HIT")
    expect(harness.originFetch).toHaveBeenCalledOnce()
    expect(JSON.stringify(await responseJSON(second))).not.toContain(
      "source_family"
    )
  })

  it("records aggregate dimensions without query or visitor identifiers", async () => {
    const harness = createHarness()
    await run(
      harness,
      "/api/v1/demo/search?q=uniquely-sensitive-query&source=sqlite",
      {
        headers: { "X-Docs-Puller-Probe": "synthetic" },
      }
    )
    expect(harness.analyticsWrite).toHaveBeenCalled()
    const event = harness.analyticsWrite.mock.calls.at(-1)?.[0]
    const serialized = JSON.stringify(event)
    expect(serialized).not.toContain("uniquely-sensitive-query")
    expect(serialized).not.toContain("203.0.113.42")
    expect(serialized).toContain("synthetic")
    expect(serialized).toContain("sqlite")
  })

  it("rejects mutating methods and unknown API paths", async () => {
    const harness = createHarness()
    const post = await run(harness, "/api/v1/demo/search?q=sqlite", {
      method: "POST",
    })
    expect(post.status).toBe(405)
    expect(post.headers.get("Allow")).toBe("GET, OPTIONS")
    const missing = await run(harness, "/api/v1/demo/private")
    expect(missing.status).toBe(404)
  })
})
