import * as http from "http";
import * as https from "https";
import { realpath } from "fs/promises";
import * as path from "path";
import { URL } from "url";

export const maxResponseBytes = 4 * 1024 * 1024;

export interface SearchResult {
  path: string;
  source: string;
  title?: string;
  url?: string;
  score: number;
  snippets?: { line: number; text: string }[];
}

export interface SearchResponse {
  query: string;
  mode: "fts5" | "scan";
  scanned: number;
  elapsed_ms: number;
  results: SearchResult[];
}

export interface SourcesResponse {
  root: string;
  sources: { name: string; docs: number }[];
}

export function normalizeServerURL(rawURL: string): string {
  const value = rawURL.trim();
  const parsed = new URL(value);
  if (parsed.protocol !== "http:" && parsed.protocol !== "https:") {
    throw new Error("server URL must use http or https");
  }
  if (parsed.username || parsed.password) {
    throw new Error("server URL must not contain credentials");
  }
  if (parsed.search || parsed.hash) {
    throw new Error("server URL must not contain a query or fragment");
  }
  parsed.pathname = parsed.pathname.replace(/\/+$/, "");
  return parsed.toString().replace(/\/$/, "");
}

export function apiURL(
  serverURL: string,
  endpoint: string,
  params?: URLSearchParams,
): string {
  const base = normalizeServerURL(serverURL) + "/";
  const relativeEndpoint = endpoint.replace(/^\/+/, "");
  const target = new URL(relativeEndpoint, base);
  if (params) {
    target.search = params.toString();
  }
  return target.toString();
}

export function searchURL(
  serverURL: string,
  query: string,
  limit: number,
  source?: string,
): string {
  const params = new URLSearchParams({ q: query, limit: String(limit) });
  if (source) {
    params.set("source", source);
  }
  return apiURL(serverURL, "api/search", params);
}

export function normalizeResultLimit(value: unknown): number {
  if (
    typeof value !== "number" ||
    !Number.isInteger(value) ||
    value < 1 ||
    value > 100
  ) {
    throw new Error("result limit must be an integer from 1 through 100");
  }
  return value;
}

export function resolveDocumentPath(root: string, resultPath: string): string {
  if (
    !root.trim() ||
    !path.isAbsolute(root) ||
    !resultPath.trim() ||
    resultPath.includes("\0") ||
    resultPath.includes("\\") ||
    path.posix.isAbsolute(resultPath)
  ) {
    throw new Error("search result path is not a safe relative path");
  }
  const rootPath = path.resolve(root);
  const candidate = path.resolve(rootPath, ...resultPath.split("/"));
  const relative = path.relative(rootPath, candidate);
  if (
    !relative ||
    relative === ".." ||
    relative.startsWith(`..${path.sep}`) ||
    path.isAbsolute(relative)
  ) {
    throw new Error("search result path escapes the docs corpus");
  }
  return candidate;
}

export async function resolveExistingDocumentPath(
  root: string,
  resultPath: string,
): Promise<string> {
  const candidate = resolveDocumentPath(root, resultPath);
  const [realRoot, realCandidate] = await Promise.all([
    realpath(root),
    realpath(candidate),
  ]);
  const relative = path.relative(realRoot, realCandidate);
  if (
    !relative ||
    relative === ".." ||
    relative.startsWith(`..${path.sep}`) ||
    path.isAbsolute(relative)
  ) {
    throw new Error(
      "search result path escapes the docs corpus through a link",
    );
  }
  return realCandidate;
}

export function safeOriginURL(rawURL: string): URL {
  const parsed = new URL(rawURL);
  if (
    (parsed.protocol !== "http:" && parsed.protocol !== "https:") ||
    parsed.username ||
    parsed.password
  ) {
    throw new Error(
      "origin URL must be an http or https URL without credentials",
    );
  }
  return parsed;
}

export function markdownLink(label: string, rawURL: string): string {
  const url = safeOriginURL(rawURL).toString();
  const safeLabel = label
    .replace(/\\/g, "\\\\")
    .replace(/\[/g, "\\[")
    .replace(/\]/g, "\\]");
  return `[${safeLabel}](<${url}>)`;
}

export function fetchJSON(
  rawURL: string,
  timeoutMs = 5000,
  limitBytes = maxResponseBytes,
  bearerToken = "",
): Promise<unknown> {
  return new Promise((resolve, reject) => {
    let parsed: URL;
    try {
      parsed = new URL(rawURL);
      if (parsed.protocol !== "http:" && parsed.protocol !== "https:") {
        throw new Error("request URL must use http or https");
      }
      if (parsed.username || parsed.password) {
        throw new Error("request URL must not contain credentials");
      }
    } catch (error) {
      reject(error);
      return;
    }

    const token = bearerToken.trim();
    if (
      token.length > 8192 ||
      (token.length > 0 && !/^[A-Za-z0-9\-._~+/]+=*$/.test(token))
    ) {
      reject(new Error("bearer token is invalid"));
      return;
    }
    if (
      token &&
      parsed.protocol === "http:" &&
      !isLoopbackHostname(parsed.hostname)
    ) {
      reject(
        new Error(
          "refusing to send a bearer token over non-loopback HTTP; use HTTPS",
        ),
      );
      return;
    }

    const transport = parsed.protocol === "https:" ? https : http;
    const headers: Record<string, string> = { Accept: "application/json" };
    if (token) {
      headers.Authorization = `Bearer ${token}`;
    }
    const request = transport.get(parsed, { headers }, (response) => {
      if (
        !response.statusCode ||
        response.statusCode < 200 ||
        response.statusCode >= 300
      ) {
        response.resume();
        reject(new Error(`HTTP ${response.statusCode ?? "unknown"}`));
        return;
      }
      const chunks: Buffer[] = [];
      let size = 0;
      let responseFailure: Error | undefined;
      response.on("data", (chunk: Buffer) => {
        size += chunk.length;
        if (size > limitBytes) {
          responseFailure = new Error(`response exceeds ${limitBytes} bytes`);
          response.destroy(responseFailure);
          return;
        }
        chunks.push(chunk);
      });
      response.on("error", reject);
      response.on("aborted", () =>
        reject(responseFailure ?? new Error("response aborted")),
      );
      response.on("end", () => {
        try {
          resolve(
            JSON.parse(Buffer.concat(chunks).toString("utf-8")) as unknown,
          );
        } catch (error) {
          reject(error);
        }
      });
    });
    request.on("error", reject);
    request.setTimeout(timeoutMs, () =>
      request.destroy(new Error("request timeout")),
    );
  });
}

export function parseSourcesResponse(value: unknown): SourcesResponse {
  if (
    !isRecord(value) ||
    typeof value.root !== "string" ||
    !value.root.trim() ||
    !Array.isArray(value.sources)
  ) {
    throw new Error("sources response has an invalid shape");
  }
  const sources = value.sources.map((source) => {
    if (
      !isRecord(source) ||
      typeof source.name !== "string" ||
      !source.name.trim() ||
      !isNonNegativeInteger(source.docs)
    ) {
      throw new Error("sources response contains an invalid source");
    }
    return { name: source.name, docs: source.docs };
  });
  return { root: value.root, sources };
}

export function parseSearchResponse(value: unknown): SearchResponse {
  if (
    !isRecord(value) ||
    typeof value.query !== "string" ||
    (value.mode !== "fts5" && value.mode !== "scan") ||
    !isNonNegativeInteger(value.scanned) ||
    !isFiniteNumber(value.elapsed_ms) ||
    value.elapsed_ms < 0 ||
    !Array.isArray(value.results)
  ) {
    throw new Error("search response has an invalid shape");
  }
  const results = value.results.map(parseSearchResult);
  return {
    query: value.query,
    mode: value.mode,
    scanned: value.scanned,
    elapsed_ms: value.elapsed_ms,
    results,
  };
}

function parseSearchResult(value: unknown): SearchResult {
  if (
    !isRecord(value) ||
    typeof value.path !== "string" ||
    typeof value.source !== "string" ||
    !isFiniteNumber(value.score)
  ) {
    throw new Error("search response contains an invalid result");
  }
  if (value.title !== undefined && typeof value.title !== "string") {
    throw new Error("search result title is invalid");
  }
  if (value.url !== undefined && typeof value.url !== "string") {
    throw new Error("search result URL is invalid");
  }
  let snippets: { line: number; text: string }[] | undefined;
  if (value.snippets !== undefined) {
    if (!Array.isArray(value.snippets)) {
      throw new Error("search result snippets are invalid");
    }
    snippets = value.snippets.map((snippet) => {
      if (
        !isRecord(snippet) ||
        !isNonNegativeInteger(snippet.line) ||
        snippet.line < 1 ||
        typeof snippet.text !== "string"
      ) {
        throw new Error("search result contains an invalid snippet");
      }
      return { line: snippet.line, text: snippet.text };
    });
  }
  return {
    path: value.path,
    source: value.source,
    title: value.title as string | undefined,
    url: value.url as string | undefined,
    score: value.score,
    snippets,
  };
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

function isFiniteNumber(value: unknown): value is number {
  return typeof value === "number" && Number.isFinite(value);
}

function isNonNegativeInteger(value: unknown): value is number {
  return isFiniteNumber(value) && Number.isInteger(value) && value >= 0;
}

function isLoopbackHostname(hostname: string): boolean {
  const normalized = hostname.toLowerCase().replace(/^\[|\]$/g, "");
  return (
    normalized === "localhost" ||
    normalized.endsWith(".localhost") ||
    normalized === "::1" ||
    /^127(?:\.[0-9]{1,3}){3}$/.test(normalized)
  );
}
