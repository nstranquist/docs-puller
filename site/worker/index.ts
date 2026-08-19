import type { components } from "../src/lib/api/generated"

type CorpusIdentity = components["schemas"]["CorpusIdentity"]
type DocumentResponse = components["schemas"]["DocumentResponse"]
type ErrorCode = components["schemas"]["ErrorResponse"]["error"]["code"]
type ErrorResponse = components["schemas"]["ErrorResponse"]
type HealthResponse = components["schemas"]["HealthResponse"]
type MetaResponse = components["schemas"]["MetaResponse"]
type ReadinessResponse = components["schemas"]["ReadinessResponse"]
type SearchResponse = components["schemas"]["SearchResponse"]
type SearchResult = components["schemas"]["SearchResult"]
type Source = components["schemas"]["Source"]
type SourceID = components["schemas"]["SourceID"]
type SourcesResponse = components["schemas"]["SourcesResponse"]

const serviceName = "docs-puller-demo" as const
const apiPrefix = "/api/v1/demo/"
const maxURLBytes = 2048
const maxResponseBytes = 65_536
const maxDocumentBytes = 60_000
const originTimeoutMS = 4_000
const searchCacheSeconds = 30
const documentCacheSeconds = 3_600
const allowedSources = new Set<SourceID>(["sqlite", "go", "postgresql"])
const allowedSearchFields = new Set(["q", "source", "limit", "mode"])
const allowedDocumentFields = new Set(["source", "path"])
const digestPattern = /^sha256:[a-f0-9]{64}$/u
const commitPattern = /^[a-f0-9]{7,64}$/u
const pathPattern = /^[A-Za-z0-9._/-]+\.md$/u

const sourceCatalog: readonly Source[] = [
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
    license: "CC BY 4.0 website content, unless noted otherwise",
  },
  {
    id: "postgresql",
    label: "PostgreSQL",
    document_count: 8,
    homepage: "https://www.postgresql.org/docs/",
    license: "PostgreSQL documentation license",
  },
] as const

const sourceHosts: Record<SourceID, ReadonlySet<string>> = {
  sqlite: new Set(["sqlite.org", "www.sqlite.org"]),
  go: new Set(["go.dev"]),
  postgresql: new Set(["postgresql.org", "www.postgresql.org"]),
}

export interface RateLimitBinding {
  limit(options: { key: string }): Promise<{ success: boolean }>
}

export interface AnalyticsBinding {
  writeDataPoint(event: {
    indexes?: string[]
    blobs?: string[]
    doubles?: number[]
  }): void
}

export interface AssetBinding {
  fetch(request: Request): Promise<Response>
}

export interface Env {
  ASSETS: AssetBinding
  SEARCH_RATE_LIMITER: RateLimitBinding
  API_RATE_LIMITER: RateLimitBinding
  DEMO_ANALYTICS?: AnalyticsBinding
  SIDECAR_URL: string
  SIDECAR_TOKEN: string
  RATE_KEY_SECRET: string
  PUBLIC_ORIGIN: string
  BUILD_ID: string
  BUILD_COMMIT: string
  DEPLOYED_AT: string
  ENGINE_VERSION: string
  CORPUS_DIGEST: string
  CORPUS_INDEX_DIGEST: string
  CORPUS_RETRIEVED_AT: string
}

export interface ExecutionContextLike {
  waitUntil(promise: Promise<unknown>): void
}

export interface CacheLike {
  match(request: Request): Promise<Response | undefined>
  put(request: Request, response: Response): Promise<void>
}

export interface WorkerDependencies {
  fetch(request: Request): Promise<Response>
  cache?: CacheLike
  originTimeoutMS?: number
  now(): Date
  randomUUID(): string
}

interface RouteOutcome {
  response: Response
  event: string
  resultCount?: number
  source?: SourceID | "all"
  cache: "hit" | "miss" | "none"
}

interface ParsedSearch {
  query: string
  source: SourceID | ""
  limit: number
}

interface ParsedDocument {
  source: SourceID
  path: string
}

interface OriginSearchResponse {
  query: string
  mode: string
  elapsed_ms: number
  results: OriginSearchHit[]
}

interface OriginSearchHit {
  path: string
  source: string
  title: string
  url: string
  score: number
  snippets: Array<{ line: number; text: string }>
}

interface OriginDocumentResponse {
  source: string
  path: string
  title: string
  url: string
  content: string
  bytes: number
}

interface OriginStatusResponse {
  ok: boolean
  apiVersion: string
  totalDocs: number
  sources: number
}

class PublicError extends Error {
  readonly status: number
  readonly code: ErrorCode
  readonly retryAfterSeconds: number | undefined

  constructor(
    status: number,
    code: ErrorCode,
    message: string,
    retryAfterSeconds?: number
  ) {
    super(message)
    this.name = "PublicError"
    this.status = status
    this.code = code
    this.retryAfterSeconds = retryAfterSeconds
  }
}

const productionDependencies: WorkerDependencies = {
  fetch: (request) => fetch(request),
  now: () => new Date(),
  randomUUID: () => crypto.randomUUID(),
}
const productionCache = defaultCache()
if (productionCache) productionDependencies.cache = productionCache

export default {
  fetch(
    request: Request,
    env: Env,
    context: ExecutionContext
  ): Promise<Response> {
    return handleRequest(request, env, context, productionDependencies)
  },
} satisfies ExportedHandler<Env>

export async function handleRequest(
  request: Request,
  env: Env,
  context: ExecutionContextLike,
  dependencies: WorkerDependencies = productionDependencies
): Promise<Response> {
  const started = performance.now()
  const requestID = safeRequestID(request, dependencies)
  let outcome: RouteOutcome

  try {
    outcome = await routeRequest(request, env, context, dependencies, requestID)
  } catch (error) {
    const publicError = toPublicError(error)
    outcome = {
      response: errorResponse(
        publicError.status,
        publicError.code,
        publicError.message,
        requestID,
        publicError.retryAfterSeconds
      ),
      event: routeEvent(new URL(request.url).pathname),
      cache: "none",
    }
  }

  const response = applyResponseHeaders(
    outcome.response,
    request,
    env,
    requestID,
    outcome.cache
  )
  recordTelemetry(
    env,
    request,
    outcome,
    response.status,
    performance.now() - started
  )
  return response
}

async function routeRequest(
  request: Request,
  env: Env,
  context: ExecutionContextLike,
  dependencies: WorkerDependencies,
  requestID: string
): Promise<RouteOutcome> {
  if (new TextEncoder().encode(request.url).byteLength > maxURLBytes) {
    throw new PublicError(
      400,
      "invalid_request",
      "The request URL is too long."
    )
  }

  const url = new URL(request.url)
  const isPublicAPI =
    url.pathname.startsWith(apiPrefix) || url.pathname === "/readyz"
  if (isPublicAPI) enforceOrigin(request, env)

  if (request.method === "OPTIONS") {
    if (!isPublicAPI)
      return outcome(new Response(null, { status: 404 }), "not_found")
    return outcome(new Response(null, { status: 204 }), "preflight")
  }

  if (request.method !== "GET") {
    const response = errorResponse(
      405,
      "invalid_request",
      "Only GET and OPTIONS are allowed.",
      requestID
    )
    response.headers.set("Allow", "GET, OPTIONS")
    return outcome(response, "method_not_allowed")
  }

  switch (url.pathname) {
    case "/healthz":
      rejectQueryFields(url.searchParams, new Set())
      return outcome(
        jsonResponse<HealthResponse>(200, {
          ok: true,
          service: serviceName,
          build_id: requiredText(env.BUILD_ID, "BUILD_ID", 120),
        }),
        "health"
      )
    case "/readyz":
      rejectQueryFields(url.searchParams, new Set())
      await enforceRateLimit(request, env, "api")
      return readinessOutcome(env, dependencies)
    case "/api/v1/demo/meta":
      rejectQueryFields(url.searchParams, new Set())
      await enforceRateLimit(request, env, "api")
      return outcome(metaResponse(env), "meta")
    case "/api/v1/demo/sources":
      rejectQueryFields(url.searchParams, new Set())
      await enforceRateLimit(request, env, "api")
      return outcome(sourcesResponse(env), "sources")
    case "/api/v1/demo/search":
      await enforceRateLimit(request, env, "search")
      return searchOutcome(url, env, context, dependencies)
    case "/api/v1/demo/doc":
      await enforceRateLimit(request, env, "api")
      return documentOutcome(url, env, context, dependencies)
    default:
      if (
        url.pathname.startsWith("/api/") ||
        url.pathname.startsWith("/readyz")
      ) {
        return outcome(
          errorResponse(
            404,
            "not_found",
            "The public API route does not exist.",
            requestID
          ),
          "not_found"
        )
      }
      return outcome(await env.ASSETS.fetch(request), "asset")
  }
}

async function readinessOutcome(
  env: Env,
  dependencies: WorkerDependencies
): Promise<RouteOutcome> {
  const response = await fetchOrigin("/api/status", env, dependencies)
  if (!response.ok) {
    throw new PublicError(
      503,
      "origin_unavailable",
      "The search origin is not ready."
    )
  }
  const parsed = parseOriginStatus(await readBoundedJSON(response))
  if (
    !parsed.ok ||
    parsed.apiVersion !== "v1" ||
    parsed.totalDocs !== 24 ||
    parsed.sources !== 3
  ) {
    throw new PublicError(
      503,
      "origin_invalid",
      "The search origin does not match the reviewed corpus."
    )
  }
  const body: ReadinessResponse = {
    ok: true,
    service: serviceName,
    origin: "ready",
    corpus: corpusIdentity(env),
    checked_at: dependencies.now().toISOString(),
  }
  return outcome(jsonResponse(200, body, "no-store"), "readiness")
}

function metaResponse(env: Env): Response {
  const deployedAt = requiredDate(env.DEPLOYED_AT, "DEPLOYED_AT")
  if (!commitPattern.test(env.BUILD_COMMIT)) {
    throw new PublicError(
      503,
      "internal_error",
      "The deployment identity is invalid."
    )
  }
  const body: MetaResponse = {
    ok: true,
    schema_version: 1,
    service: serviceName,
    engine: {
      name: "docs-puller",
      version: requiredText(env.ENGINE_VERSION, "ENGINE_VERSION", 64),
      mode: "fts5",
    },
    build_id: requiredText(env.BUILD_ID, "BUILD_ID", 120),
    commit: env.BUILD_COMMIT,
    deployed_at: deployedAt,
    corpus: corpusIdentity(env),
    limits: {
      query_characters: 160,
      results: 10,
      timeout_ms: 4000,
      response_bytes: 65536,
    },
  }
  return jsonResponse(200, body, "public, max-age=60, s-maxage=300")
}

function sourcesResponse(env: Env): Response {
  const body: SourcesResponse = {
    ok: true,
    corpus: corpusIdentity(env),
    sources: [...sourceCatalog],
  }
  return jsonResponse(200, body, "public, max-age=300, s-maxage=3600")
}

async function searchOutcome(
  publicURL: URL,
  env: Env,
  context: ExecutionContextLike,
  dependencies: WorkerDependencies
): Promise<RouteOutcome> {
  const parsed = parseSearch(publicURL.searchParams)
  const cacheRequest = normalizedCacheRequest(publicURL, {
    q: parsed.query,
    ...(parsed.source ? { source: parsed.source } : {}),
    limit: String(parsed.limit),
    mode: "fts5",
  })
  const cached = await dependencies.cache?.match(cacheRequest)
  if (cached?.ok) {
    return outcome(cached, "search", "hit", undefined, parsed.source || "all")
  }

  const originQuery = new URLSearchParams({
    q: parsed.query,
    limit: String(parsed.limit),
    mode: "fts5",
  })
  if (parsed.source) originQuery.set("source", parsed.source)
  const origin = await fetchOrigin(
    `/api/search?${originQuery}`,
    env,
    dependencies
  )
  if (!origin.ok) throw mapOriginFailure(origin.status)
  const parsedOrigin = parseOriginSearch(await readBoundedJSON(origin))
  const body = sanitizeSearch(parsed, parsedOrigin, env)
  const response = jsonResponse<SearchResponse>(
    200,
    body,
    `public, max-age=0, s-maxage=${searchCacheSeconds}, stale-while-revalidate=120`
  )
  if (dependencies.cache) {
    context.waitUntil(dependencies.cache.put(cacheRequest, response.clone()))
  }
  return outcome(
    response,
    "search",
    "miss",
    body.result_count,
    parsed.source || "all"
  )
}

async function documentOutcome(
  publicURL: URL,
  env: Env,
  context: ExecutionContextLike,
  dependencies: WorkerDependencies
): Promise<RouteOutcome> {
  const parsed = parseDocument(publicURL.searchParams)
  const cacheRequest = normalizedCacheRequest(publicURL, {
    source: parsed.source,
    path: parsed.path,
  })
  const cached = await dependencies.cache?.match(cacheRequest)
  if (cached?.ok)
    return outcome(cached, "document", "hit", undefined, parsed.source)

  const originQuery = new URLSearchParams({
    source: parsed.source,
    path: parsed.path,
  })
  const origin = await fetchOrigin(`/api/doc?${originQuery}`, env, dependencies)
  if (origin.status === 404) {
    throw new PublicError(
      404,
      "not_found",
      "The reviewed document was not found."
    )
  }
  if (!origin.ok) throw mapOriginFailure(origin.status)
  const parsedOrigin = parseOriginDocument(await readBoundedJSON(origin))
  const body = sanitizeDocument(parsed, parsedOrigin, env)
  const response = jsonResponse<DocumentResponse>(
    200,
    body,
    `public, max-age=300, s-maxage=${documentCacheSeconds}, immutable`
  )
  if (dependencies.cache) {
    context.waitUntil(dependencies.cache.put(cacheRequest, response.clone()))
  }
  return outcome(response, "document", "miss", undefined, parsed.source)
}

function parseSearch(params: URLSearchParams): ParsedSearch {
  rejectQueryFields(params, allowedSearchFields)
  rejectDuplicateFields(params, allowedSearchFields)

  const query = normalizeQuery(params.get("q") ?? "")
  const queryLength = [...query].length
  if (queryLength < 2 || queryLength > 160 || hasControlCharacters(query)) {
    throw new PublicError(
      400,
      "invalid_request",
      "Use a query with 2 to 160 visible characters."
    )
  }

  const rawSource = params.get("source") ?? ""
  const source = rawSource === "" ? "" : parseSource(rawSource)
  const rawLimit = params.get("limit") ?? "6"
  if (!/^\d{1,2}$/u.test(rawLimit)) {
    throw new PublicError(
      400,
      "invalid_request",
      "Limit must be an integer from 1 to 10."
    )
  }
  const limit = Number(rawLimit)
  if (!Number.isSafeInteger(limit) || limit < 1 || limit > 10) {
    throw new PublicError(
      400,
      "invalid_request",
      "Limit must be an integer from 1 to 10."
    )
  }
  const mode = params.get("mode") ?? "fts5"
  if (mode !== "fts5") {
    throw new PublicError(
      400,
      "invalid_request",
      "The public demo supports FTS5 mode only."
    )
  }
  return { query, source, limit }
}

function parseDocument(params: URLSearchParams): ParsedDocument {
  rejectQueryFields(params, allowedDocumentFields)
  rejectDuplicateFields(params, allowedDocumentFields)
  const source = parseSource(params.get("source") ?? "")
  const path = normalizePublicPath(params.get("path") ?? "", source)
  return { source, path }
}

function sanitizeSearch(
  request: ParsedSearch,
  origin: OriginSearchResponse,
  env: Env
): SearchResponse {
  if (origin.mode !== "fts5") {
    throw new PublicError(
      502,
      "origin_invalid",
      "The origin did not use the required FTS5 engine."
    )
  }
  const results = origin.results
    .slice(0, request.limit)
    .map((hit) => sanitizeHit(hit))
  if (
    request.source &&
    results.some((result) => result.source !== request.source)
  ) {
    throw new PublicError(
      502,
      "origin_invalid",
      "The origin returned a source outside the request."
    )
  }
  return {
    ok: true,
    query: request.query,
    engine: "docs-puller",
    mode: "fts5",
    elapsed_ms: boundedInteger(
      origin.elapsed_ms,
      0,
      60_000,
      "origin elapsed time"
    ),
    result_count: results.length,
    corpus: corpusIdentity(env),
    results,
  }
}

function sanitizeHit(hit: OriginSearchHit): SearchResult {
  const source = parseOriginSource(hit.source)
  const path = normalizeOriginPath(hit.path, source)
  const url = validateCanonicalURL(hit.url, source)
  const snippets = hit.snippets.slice(0, 3).map((snippet) => ({
    line: boundedInteger(snippet.line, 1, 10_000_000, "snippet line"),
    text: boundedVisibleText(snippet.text, 500, "snippet text"),
  }))
  return {
    title: boundedVisibleText(hit.title || path, 240, "result title"),
    source,
    path,
    url,
    score: boundedInteger(
      hit.score,
      -2_147_483_648,
      2_147_483_647,
      "result score"
    ),
    snippets,
  }
}

function sanitizeDocument(
  request: ParsedDocument,
  origin: OriginDocumentResponse,
  env: Env
): DocumentResponse {
  const source = parseOriginSource(origin.source)
  const path = normalizeOriginPath(origin.path, source)
  if (source !== request.source || path !== request.path) {
    throw new PublicError(
      502,
      "origin_invalid",
      "The origin returned a different document."
    )
  }
  const encoded = new TextEncoder().encode(origin.content)
  if (encoded.byteLength > maxDocumentBytes || hasNullByte(origin.content)) {
    throw new PublicError(
      502,
      "origin_invalid",
      "The origin document exceeds the public limit."
    )
  }
  return {
    ok: true,
    source,
    path,
    title: boundedVisibleText(origin.title || path, 240, "document title"),
    url: validateCanonicalURL(origin.url, source),
    content_type: "text/markdown",
    content: origin.content,
    bytes: encoded.byteLength,
    corpus: corpusIdentity(env),
  }
}

async function fetchOrigin(
  pathAndQuery: string,
  env: Env,
  dependencies: WorkerDependencies
): Promise<Response> {
  const sidecarURL = parseSidecarURL(env.SIDECAR_URL)
  const token = requiredText(env.SIDECAR_TOKEN, "SIDECAR_TOKEN", 512)
  const target = new URL(
    pathAndQuery,
    `${sidecarURL.toString().replace(/\/$/u, "")}/`
  )
  if (target.origin !== sidecarURL.origin) {
    throw new PublicError(
      503,
      "internal_error",
      "The origin target is invalid."
    )
  }
  const controller = new AbortController()
  const timeout = setTimeout(
    () => controller.abort(),
    dependencies.originTimeoutMS ?? originTimeoutMS
  )
  try {
    return await dependencies.fetch(
      new Request(target, {
        method: "GET",
        headers: {
          Accept: "application/json",
          Authorization: `Bearer ${token}`,
          "User-Agent": "docs-puller-public-demo/1",
        },
        signal: controller.signal,
      })
    )
  } catch {
    if (controller.signal.aborted) {
      throw new PublicError(
        504,
        "origin_timeout",
        "The search origin exceeded four seconds."
      )
    }
    throw new PublicError(
      503,
      "origin_unavailable",
      "The search origin is unavailable."
    )
  } finally {
    clearTimeout(timeout)
  }
}

async function readBoundedJSON(response: Response): Promise<unknown> {
  const contentType = response.headers.get("Content-Type") ?? ""
  if (!contentType.toLocaleLowerCase().includes("application/json")) {
    throw new PublicError(
      502,
      "origin_invalid",
      "The origin response is not JSON."
    )
  }
  const contentLength = Number(response.headers.get("Content-Length") ?? "0")
  if (Number.isFinite(contentLength) && contentLength > maxResponseBytes) {
    throw new PublicError(
      502,
      "origin_invalid",
      "The origin response exceeds 64 KiB."
    )
  }
  if (!response.body) return null
  const reader = response.body.getReader()
  const chunks: Uint8Array[] = []
  let total = 0
  while (true) {
    const { done, value } = await reader.read()
    if (done) break
    total += value.byteLength
    if (total > maxResponseBytes) {
      await reader.cancel()
      throw new PublicError(
        502,
        "origin_invalid",
        "The origin response exceeds 64 KiB."
      )
    }
    chunks.push(value)
  }
  const merged = new Uint8Array(total)
  let offset = 0
  for (const chunk of chunks) {
    merged.set(chunk, offset)
    offset += chunk.byteLength
  }
  try {
    return JSON.parse(
      new TextDecoder("utf-8", { fatal: true }).decode(merged)
    ) as unknown
  } catch {
    throw new PublicError(
      502,
      "origin_invalid",
      "The origin returned invalid JSON."
    )
  }
}

function parseOriginSearch(value: unknown): OriginSearchResponse {
  const record = asRecord(value, "origin search response")
  const results = asArray(record.results, "origin search results").map(
    (item) => {
      const hit = asRecord(item, "origin search hit")
      const snippets =
        hit.snippets === undefined
          ? []
          : asArray(hit.snippets, "origin snippets")
      return {
        path: asString(hit.path, "origin path"),
        source: asString(hit.source, "origin source"),
        title: optionalString(hit.title, "origin title"),
        url: optionalString(hit.url, "origin URL"),
        score: asNumber(hit.score, "origin score"),
        snippets: snippets.map((item) => {
          const snippet = asRecord(item, "origin snippet")
          return {
            line: asNumber(snippet.line, "origin snippet line"),
            text: asString(snippet.text, "origin snippet text"),
          }
        }),
      }
    }
  )
  return {
    query: asString(record.query, "origin query"),
    mode: asString(record.mode, "origin mode"),
    elapsed_ms: asNumber(record.elapsed_ms, "origin elapsed time"),
    results,
  }
}

function parseOriginDocument(value: unknown): OriginDocumentResponse {
  const record = asRecord(value, "origin document response")
  return {
    source: asString(record.source, "origin document source"),
    path: asString(record.path, "origin document path"),
    title: optionalString(record.title, "origin document title"),
    url: optionalString(record.url, "origin document URL"),
    content: asString(record.content, "origin document content"),
    bytes: asNumber(record.bytes, "origin document bytes"),
  }
}

function parseOriginStatus(value: unknown): OriginStatusResponse {
  const record = asRecord(value, "origin status response")
  return {
    ok: record.ok === true,
    apiVersion: asString(record.apiVersion, "origin API version"),
    totalDocs: asNumber(record.totalDocs, "origin document count"),
    sources: asNumber(record.sources, "origin source count"),
  }
}

function corpusIdentity(env: Env): CorpusIdentity {
  if (
    !digestPattern.test(env.CORPUS_DIGEST) ||
    !digestPattern.test(env.CORPUS_INDEX_DIGEST)
  ) {
    throw new PublicError(
      503,
      "internal_error",
      "The corpus identity is invalid."
    )
  }
  return {
    id: "public-sample-v1",
    digest: env.CORPUS_DIGEST,
    index_digest: env.CORPUS_INDEX_DIGEST,
    document_count: 24,
    source_count: 3,
    retrieved_at: requiredDate(env.CORPUS_RETRIEVED_AT, "CORPUS_RETRIEVED_AT"),
  }
}

async function enforceRateLimit(
  request: Request,
  env: Env,
  className: "search" | "api"
): Promise<void> {
  const secret = requiredText(env.RATE_KEY_SECRET, "RATE_KEY_SECRET", 512)
  const rateKey = await deriveRateKey(request, secret)
  const binding =
    className === "search" ? env.SEARCH_RATE_LIMITER : env.API_RATE_LIMITER
  if (!binding) {
    throw new PublicError(
      503,
      "internal_error",
      "The request limiter is unavailable."
    )
  }
  const decision = await binding.limit({ key: rateKey })
  if (!decision.success) {
    throw new PublicError(
      429,
      "rate_limited",
      "The public request budget is full.",
      60
    )
  }
}

async function deriveRateKey(
  request: Request,
  secret: string
): Promise<string> {
  const address = request.headers.get("CF-Connecting-IP")?.trim() || "unknown"
  const day = new Date().toISOString().slice(0, 10)
  const encoder = new TextEncoder()
  const key = await crypto.subtle.importKey(
    "raw",
    encoder.encode(secret),
    { name: "HMAC", hash: "SHA-256" },
    false,
    ["sign"]
  )
  const signature = await crypto.subtle.sign(
    "HMAC",
    key,
    encoder.encode(`${day}:${address}`)
  )
  return bytesToBase64URL(new Uint8Array(signature)).slice(0, 32)
}

function enforceOrigin(request: Request, env: Env): void {
  const origin = request.headers.get("Origin")
  if (origin && origin !== requiredOrigin(env.PUBLIC_ORIGIN)) {
    throw new PublicError(
      403,
      "invalid_request",
      "Cross-origin access is not allowed."
    )
  }
}

function applyResponseHeaders(
  response: Response,
  request: Request,
  env: Env,
  requestID: string,
  cacheState: RouteOutcome["cache"]
): Response {
  const headers = new Headers(response.headers)
  headers.set("Content-Security-Policy", securityPolicy())
  headers.set("Cross-Origin-Opener-Policy", "same-origin")
  headers.set("Cross-Origin-Resource-Policy", "same-origin")
  headers.set(
    "Permissions-Policy",
    "camera=(), microphone=(), geolocation=(), payment=(), usb=()"
  )
  headers.set("Referrer-Policy", "no-referrer")
  headers.set(
    "Strict-Transport-Security",
    "max-age=63072000; includeSubDomains; preload"
  )
  headers.set("X-Content-Type-Options", "nosniff")
  headers.set("X-Frame-Options", "DENY")
  headers.set("X-Request-ID", requestID)
  headers.set("X-Docs-Puller-Cache", cacheState.toUpperCase())

  const origin = request.headers.get("Origin")
  if (origin && origin === requiredOrigin(env.PUBLIC_ORIGIN)) {
    headers.set("Access-Control-Allow-Origin", origin)
    headers.set("Access-Control-Allow-Methods", "GET, OPTIONS")
    headers.set("Access-Control-Allow-Headers", "Content-Type")
    headers.set("Access-Control-Max-Age", "600")
    appendVary(headers, "Origin")
  } else {
    headers.delete("Access-Control-Allow-Origin")
    appendVary(headers, "Origin")
  }
  return new Response(response.body, {
    status: response.status,
    statusText: response.statusText,
    headers,
  })
}

function recordTelemetry(
  env: Env,
  request: Request,
  route: RouteOutcome,
  status: number,
  elapsedMS: number
): void {
  if (!env.DEMO_ANALYTICS || route.event === "asset") return
  const synthetic =
    request.headers.get("X-Docs-Puller-Probe") === "synthetic"
      ? "synthetic"
      : "unclassified"
  try {
    env.DEMO_ANALYTICS.writeDataPoint({
      indexes: [safeDimension(env.BUILD_ID, 120, "unknown")],
      blobs: [
        safeDimension(route.event, 32, "unknown"),
        statusBucket(status),
        durationBucket(elapsedMS),
        resultBucket(route.resultCount),
        route.source ?? "none",
        synthetic,
        route.cache,
      ],
      doubles: [1],
    })
  } catch {
    // Telemetry must never change the public response.
  }
}

function jsonResponse<T>(
  status: number,
  body: T,
  cacheControl = "no-store"
): Response {
  const encoded = JSON.stringify(body)
  if (new TextEncoder().encode(encoded).byteLength > maxResponseBytes) {
    throw new PublicError(
      502,
      "origin_invalid",
      "The public response exceeds 64 KiB."
    )
  }
  return new Response(encoded, {
    status,
    headers: {
      "Cache-Control": cacheControl,
      "Content-Type": "application/json; charset=utf-8",
    },
  })
}

function errorResponse(
  status: number,
  code: ErrorCode,
  message: string,
  requestID: string,
  retryAfterSeconds?: number
): Response {
  const error: ErrorResponse["error"] = { code, message, request_id: requestID }
  if (retryAfterSeconds !== undefined)
    error.retry_after_seconds = retryAfterSeconds
  const response = jsonResponse<ErrorResponse>(
    status,
    { ok: false, error },
    "no-store"
  )
  if (retryAfterSeconds !== undefined)
    response.headers.set("Retry-After", String(retryAfterSeconds))
  return response
}

function outcome(
  response: Response,
  event: string,
  cache: RouteOutcome["cache"] = "none",
  resultCount?: number,
  source?: SourceID | "all"
): RouteOutcome {
  const value: RouteOutcome = { response, event, cache }
  if (resultCount !== undefined) value.resultCount = resultCount
  if (source !== undefined) value.source = source
  return value
}

function normalizeQuery(value: string): string {
  return value.normalize("NFKC").trim().replace(/\s+/gu, " ")
}

function parseSource(value: string): SourceID {
  if (!allowedSources.has(value as SourceID)) {
    throw new PublicError(
      400,
      "invalid_request",
      "Source must be sqlite, go, or postgresql."
    )
  }
  return value as SourceID
}

function parseOriginSource(value: string): SourceID {
  if (!allowedSources.has(value as SourceID)) {
    throw new PublicError(
      502,
      "origin_invalid",
      "The origin returned a source outside the public allowlist."
    )
  }
  return value as SourceID
}

function normalizePublicPath(value: string, source: SourceID): string {
  let path = value.trim().replaceAll("\\", "/")
  if (path.startsWith(`${source}/`)) path = path.slice(source.length + 1)
  if (
    path.length < 1 ||
    path.length > 240 ||
    !pathPattern.test(path) ||
    path.startsWith("/") ||
    path.includes("//") ||
    path.split("/").some((part) => part === "." || part === "..")
  ) {
    throw new PublicError(
      400,
      "invalid_request",
      "The document path is invalid."
    )
  }
  return path
}

function normalizeOriginPath(value: string, source: SourceID): string {
  try {
    return normalizePublicPath(value, source)
  } catch {
    throw new PublicError(
      502,
      "origin_invalid",
      "The origin returned an invalid public document path."
    )
  }
}

function validateCanonicalURL(value: string, source: SourceID): string {
  let url: URL
  try {
    url = new URL(value)
  } catch {
    throw new PublicError(
      502,
      "origin_invalid",
      "The origin returned an invalid source URL."
    )
  }
  if (
    url.protocol !== "https:" ||
    !sourceHosts[source].has(url.hostname) ||
    url.username ||
    url.password
  ) {
    throw new PublicError(
      502,
      "origin_invalid",
      "The origin returned a source URL outside the allowlist."
    )
  }
  return url.toString()
}

function parseSidecarURL(value: string): URL {
  let url: URL
  try {
    url = new URL(value)
  } catch {
    throw new PublicError(503, "internal_error", "The origin URL is invalid.")
  }
  if (
    url.protocol !== "https:" &&
    url.hostname !== "127.0.0.1" &&
    url.hostname !== "localhost"
  ) {
    throw new PublicError(
      503,
      "internal_error",
      "The origin URL must use HTTPS."
    )
  }
  if (
    url.username ||
    url.password ||
    (url.pathname !== "/" && url.pathname !== "")
  ) {
    throw new PublicError(
      503,
      "internal_error",
      "The origin URL must not contain credentials or a path."
    )
  }
  return url
}

function requiredOrigin(value: string): string {
  let url: URL
  try {
    url = new URL(value)
  } catch {
    throw new PublicError(
      503,
      "internal_error",
      "The public origin is invalid."
    )
  }
  if (
    url.protocol !== "https:" ||
    url.pathname !== "/" ||
    url.search ||
    url.hash ||
    url.username ||
    url.password
  ) {
    throw new PublicError(
      503,
      "internal_error",
      "The public origin is invalid."
    )
  }
  return url.origin
}

function requiredText(value: string, name: string, maxLength: number): string {
  const normalized = value?.trim()
  if (
    !normalized ||
    normalized.length > maxLength ||
    hasControlCharacters(normalized)
  ) {
    throw new PublicError(503, "internal_error", `${name} is not configured.`)
  }
  return normalized
}

function requiredDate(value: string, name: string): string {
  const parsed = new Date(value)
  if (!value || Number.isNaN(parsed.getTime())) {
    throw new PublicError(
      503,
      "internal_error",
      `${name} is not a canonical timestamp.`
    )
  }
  return parsed.toISOString()
}

function rejectQueryFields(
  params: URLSearchParams,
  allowed: ReadonlySet<string>
): void {
  params.forEach((_value, key) => {
    if (!allowed.has(key)) {
      throw new PublicError(
        400,
        "invalid_request",
        `Unknown query field: ${safeDimension(key, 32, "field")}.`
      )
    }
  })
}

function rejectDuplicateFields(
  params: URLSearchParams,
  allowed: ReadonlySet<string>
): void {
  for (const key of allowed) {
    if (params.getAll(key).length > 1) {
      throw new PublicError(
        400,
        "invalid_request",
        `Query field ${key} must appear once.`
      )
    }
  }
}

function normalizedCacheRequest(
  url: URL,
  values: Record<string, string>
): Request {
  const normalized = new URL(url.origin + url.pathname)
  for (const key of Object.keys(values).sort())
    normalized.searchParams.set(key, values[key] ?? "")
  return new Request(normalized, { method: "GET" })
}

function mapOriginFailure(status: number): PublicError {
  if (status === 401 || status === 403 || status >= 500) {
    return new PublicError(
      503,
      "origin_unavailable",
      "The search origin is unavailable."
    )
  }
  return new PublicError(
    502,
    "origin_invalid",
    "The search origin rejected the bounded request."
  )
}

function toPublicError(error: unknown): PublicError {
  if (error instanceof PublicError) return error
  return new PublicError(
    500,
    "internal_error",
    "The public demo could not complete the request."
  )
}

function asRecord(value: unknown, label: string): Record<string, unknown> {
  if (!value || typeof value !== "object" || Array.isArray(value)) {
    throw new PublicError(502, "origin_invalid", `The ${label} is invalid.`)
  }
  return value as Record<string, unknown>
}

function asArray(value: unknown, label: string): unknown[] {
  if (!Array.isArray(value)) {
    throw new PublicError(502, "origin_invalid", `The ${label} is invalid.`)
  }
  return value
}

function asString(value: unknown, label: string): string {
  if (typeof value !== "string") {
    throw new PublicError(502, "origin_invalid", `The ${label} is invalid.`)
  }
  return value
}

function optionalString(value: unknown, label: string): string {
  if (value === undefined || value === null) return ""
  return asString(value, label)
}

function asNumber(value: unknown, label: string): number {
  if (typeof value !== "number" || !Number.isFinite(value)) {
    throw new PublicError(502, "origin_invalid", `The ${label} is invalid.`)
  }
  return value
}

function boundedInteger(
  value: number,
  minimum: number,
  maximum: number,
  label: string
): number {
  if (!Number.isSafeInteger(value) || value < minimum || value > maximum) {
    throw new PublicError(
      502,
      "origin_invalid",
      `The ${label} is outside the public limit.`
    )
  }
  return value
}

function boundedVisibleText(
  value: string,
  maxCharacters: number,
  label: string
): string {
  const normalized = value.normalize("NFKC").trim()
  if (
    !normalized ||
    [...normalized].length > maxCharacters ||
    hasControlCharacters(normalized)
  ) {
    throw new PublicError(502, "origin_invalid", `The ${label} is invalid.`)
  }
  return normalized
}

function hasControlCharacters(value: string): boolean {
  for (const character of value) {
    const codePoint = character.codePointAt(0)
    if (codePoint !== undefined && (codePoint <= 31 || codePoint === 127))
      return true
  }
  return false
}

function hasNullByte(value: string): boolean {
  return value.includes("\u0000")
}

function bytesToBase64URL(bytes: Uint8Array): string {
  let binary = ""
  for (const byte of bytes) binary += String.fromCharCode(byte)
  return btoa(binary)
    .replaceAll("+", "-")
    .replaceAll("/", "_")
    .replace(/=+$/u, "")
}

function safeRequestID(
  request: Request,
  dependencies: WorkerDependencies
): string {
  const ray = request.headers.get("CF-Ray")?.trim()
  if (ray && /^[A-Za-z0-9-]{1,120}$/u.test(ray)) return ray
  return dependencies.randomUUID()
}

function safeDimension(
  value: string,
  maxLength: number,
  fallback: string
): string {
  const normalized = value?.trim().replace(/[^A-Za-z0-9._:/-]/gu, "_")
  return normalized ? normalized.slice(0, maxLength) : fallback
}

function durationBucket(milliseconds: number): string {
  if (milliseconds < 50) return "lt_50ms"
  if (milliseconds < 150) return "lt_150ms"
  if (milliseconds < 500) return "lt_500ms"
  if (milliseconds < 1_500) return "lt_1500ms"
  if (milliseconds < 4_000) return "lt_4000ms"
  return "gte_4000ms"
}

function resultBucket(count: number | undefined): string {
  if (count === undefined) return "unknown"
  if (count === 0) return "zero"
  if (count === 1) return "one"
  if (count <= 3) return "two_to_three"
  if (count <= 6) return "four_to_six"
  return "seven_to_ten"
}

function statusBucket(status: number): string {
  if (status >= 200 && status < 300) return "2xx"
  if (status >= 400 && status < 500) return "4xx"
  if (status >= 500) return "5xx"
  return "other"
}

function routeEvent(path: string): string {
  if (path === "/healthz") return "health"
  if (path === "/readyz") return "readiness"
  if (path === `${apiPrefix}search`) return "search"
  if (path === `${apiPrefix}doc`) return "document"
  if (path === `${apiPrefix}meta`) return "meta"
  if (path === `${apiPrefix}sources`) return "sources"
  return path.startsWith("/api/") ? "not_found" : "asset"
}

function securityPolicy(): string {
  return [
    "default-src 'self'",
    "base-uri 'none'",
    "connect-src 'self'",
    "font-src 'self'",
    "form-action 'self'",
    "frame-ancestors 'none'",
    "img-src 'self' data:",
    "manifest-src 'self'",
    "object-src 'none'",
    "script-src 'self' 'unsafe-inline'",
    "style-src 'self' 'unsafe-inline'",
    "upgrade-insecure-requests",
  ].join("; ")
}

function appendVary(headers: Headers, value: string): void {
  const values = (headers.get("Vary") ?? "")
    .split(",")
    .map((item) => item.trim())
    .filter(Boolean)
  if (!values.includes(value)) values.push(value)
  headers.set("Vary", values.join(", "))
}

function defaultCache(): CacheLike | undefined {
  if (typeof caches === "undefined") return undefined
  const cloudflareCaches = caches as CacheStorage & { default?: Cache }
  return cloudflareCaches.default
}
