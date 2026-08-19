import { describe, expect, it, vi } from "vitest"

import {
  createDemoClient,
  DemoAPIError,
  normalizeQuery,
} from "@/lib/api/client"
import {
  documentFixture,
  metaFixture,
  searchFixture,
  sourcesFixture,
} from "./fixtures"

function json(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  })
}

describe("demo API client", () => {
  it("normalizes Unicode whitespace without changing useful text", () => {
    expect(normalizeQuery("  ＦＴＳ5\n external   tables  ")).toBe(
      "FTS5 external tables"
    )
  })

  it("uses the generated contract for all public endpoints", async () => {
    const requests: Request[] = []
    const fetchImplementation = vi.fn(async (request: Request) => {
      requests.push(request)
      const url = new URL(request.url)
      if (url.pathname.endsWith("/meta")) return json(metaFixture)
      if (url.pathname.endsWith("/sources")) return json(sourcesFixture)
      if (url.pathname.endsWith("/search")) return json(searchFixture)
      return json(documentFixture)
    })
    const client = createDemoClient({
      baseUrl: "https://demo.example",
      fetch: fetchImplementation,
    })

    await expect(client.meta()).resolves.toEqual(metaFixture)
    await expect(client.sources()).resolves.toEqual(sourcesFixture)
    await expect(
      client.search({
        q: "  fts5   external content tables ",
        source: "sqlite",
        limit: 6,
      })
    ).resolves.toEqual(searchFixture)
    await expect(
      client.document({ source: "sqlite", path: "fts5.md" })
    ).resolves.toEqual(documentFixture)

    const searchURL = new URL(requests[2]?.url ?? "")
    expect(searchURL.searchParams.get("q")).toBe("fts5 external content tables")
    expect(searchURL.searchParams.get("source")).toBe("sqlite")
    expect(searchURL.searchParams.get("limit")).toBe("6")
    expect(searchURL.searchParams.get("mode")).toBe("fts5")
    expect(
      requests.every(
        (request) => request.headers.get("Accept") === "application/json"
      )
    ).toBe(true)
  })

  it("omits optional search fields when the caller does not set them", async () => {
    let requestURL = ""
    const client = createDemoClient({
      baseUrl: "https://demo.example",
      fetch: async (request) => {
        requestURL = request.url
        return json(searchFixture)
      },
    })
    await client.search({ q: "sqlite fts5" })
    const url = new URL(requestURL)
    expect(url.searchParams.has("source")).toBe(false)
    expect(url.searchParams.has("limit")).toBe(false)
  })

  it("returns a typed public error with retry information", async () => {
    const client = createDemoClient({
      baseUrl: "https://demo.example",
      fetch: async () =>
        json(
          {
            ok: false,
            error: {
              code: "rate_limited",
              message: "The public request budget is full.",
              request_id: "request-1",
              retry_after_seconds: 60,
            },
          },
          429
        ),
    })

    const error = await client
      .search({ q: "sqlite fts5" })
      .catch((value: unknown) => value)
    expect(error).toBeInstanceOf(DemoAPIError)
    expect(error).toMatchObject({
      status: 429,
      code: "rate_limited",
      requestID: "request-1",
      retryAfterSeconds: 60,
    })
  })

  it("fails safely when an error body is not in the public schema", async () => {
    const client = createDemoClient({
      baseUrl: "https://demo.example",
      fetch: async () =>
        json({ secret: "must not become an error message" }, 500),
    })
    await expect(client.meta()).rejects.toMatchObject({
      status: 500,
      code: "internal_error",
      message: "The live demo did not return a valid response.",
    })
  })
})
