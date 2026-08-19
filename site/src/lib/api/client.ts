import createClient from "openapi-fetch"

import type { components, paths } from "@/lib/api/generated"

export type SourceID = components["schemas"]["SourceID"]
export type MetaResponse = components["schemas"]["MetaResponse"]
export type SourcesResponse = components["schemas"]["SourcesResponse"]
export type SearchResponse = components["schemas"]["SearchResponse"]
export type SearchResult = components["schemas"]["SearchResult"]
export type DocumentResponse = components["schemas"]["DocumentResponse"]
export type ErrorResponse = components["schemas"]["ErrorResponse"]

export interface SearchInput {
  q: string
  source?: SourceID
  limit?: number
}

export interface DocumentInput {
  source: SourceID
  path: string
}

export interface DemoClient {
  meta(signal?: AbortSignal): Promise<MetaResponse>
  sources(signal?: AbortSignal): Promise<SourcesResponse>
  search(input: SearchInput, signal?: AbortSignal): Promise<SearchResponse>
  document(
    input: DocumentInput,
    signal?: AbortSignal
  ): Promise<DocumentResponse>
}

export class DemoAPIError extends Error {
  readonly code: ErrorResponse["error"]["code"]
  readonly requestID: string
  readonly retryAfterSeconds: number | undefined
  readonly status: number

  constructor(status: number, payload?: ErrorResponse) {
    const message =
      payload?.error.message ?? "The live demo did not return a valid response."
    super(message)
    this.name = "DemoAPIError"
    this.status = status
    this.code = payload?.error.code ?? "internal_error"
    this.requestID = payload?.error.request_id ?? "unavailable"
    this.retryAfterSeconds = payload?.error.retry_after_seconds
  }
}

export function normalizeQuery(value: string): string {
  return value.normalize("NFKC").trim().replace(/\s+/gu, " ")
}

type FetchImplementation = (request: Request) => Promise<Response>

export function createDemoClient(
  options: {
    baseUrl?: string
    fetch?: FetchImplementation
  } = {}
): DemoClient {
  const config: Parameters<typeof createClient<paths>>[0] = {
    baseUrl: options.baseUrl ?? "",
    headers: { Accept: "application/json" },
  }
  if (options.fetch) config.fetch = options.fetch
  const client = createClient<paths>(config)

  return {
    async meta(signal) {
      const response = await client.GET("/api/v1/demo/meta", {
        signal: signal ?? null,
      })
      return unwrap(response.data, response.error, response.response.status)
    },
    async sources(signal) {
      const response = await client.GET("/api/v1/demo/sources", {
        signal: signal ?? null,
      })
      return unwrap(response.data, response.error, response.response.status)
    },
    async search(input, signal) {
      const q = normalizeQuery(input.q)
      const query: {
        q: string
        source?: SourceID
        limit?: number
        mode: "fts5"
      } = {
        q,
        mode: "fts5",
      }
      if (input.source) query.source = input.source
      if (input.limit !== undefined) query.limit = input.limit
      const response = await client.GET("/api/v1/demo/search", {
        params: { query },
        signal: signal ?? null,
      })
      return unwrap(response.data, response.error, response.response.status)
    },
    async document(input, signal) {
      const response = await client.GET("/api/v1/demo/doc", {
        params: { query: input },
        signal: signal ?? null,
      })
      return unwrap(response.data, response.error, response.response.status)
    },
  }
}

function unwrap<T>(data: T | undefined, error: unknown, status: number): T {
  if (data !== undefined) return data
  throw new DemoAPIError(status, isErrorResponse(error) ? error : undefined)
}

function isErrorResponse(value: unknown): value is ErrorResponse {
  if (
    !value ||
    typeof value !== "object" ||
    !("ok" in value) ||
    value.ok !== false
  )
    return false
  if (!("error" in value) || !value.error || typeof value.error !== "object")
    return false
  return (
    "code" in value.error &&
    typeof value.error.code === "string" &&
    "message" in value.error &&
    typeof value.error.message === "string" &&
    "request_id" in value.error &&
    typeof value.error.request_id === "string"
  )
}
