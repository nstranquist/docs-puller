import * as assert from "node:assert/strict";
import {
  mkdtemp,
  mkdir,
  realpath,
  rm,
  symlink,
  writeFile,
} from "node:fs/promises";
import * as http from "node:http";
import * as os from "node:os";
import * as path from "node:path";
import { AddressInfo } from "node:net";
import { after, before, test } from "node:test";
import {
  apiURL,
  fetchJSON,
  markdownLink,
  normalizeResultLimit,
  normalizeServerURL,
  parseSearchResponse,
  parseSourcesResponse,
  resolveExistingDocumentPath,
  resolveDocumentPath,
  safeOriginURL,
  searchURL,
} from "../src/client";

let server: http.Server;
let serverURL: string;

before(async () => {
  server = http.createServer((request, response) => {
    switch (request.url) {
      case "/ok":
        response.setHeader("content-type", "application/json");
        response.end('{"ok":true}');
        return;
      case "/large":
        response.end(JSON.stringify({ body: "x".repeat(128) }));
        return;
      case "/slow":
        setTimeout(() => response.end('{"ok":true}'), 100);
        return;
      case "/bad-status":
        response.statusCode = 503;
        response.end("unavailable");
        return;
      case "/bad-json":
        response.end("not json");
        return;
      case "/auth":
        if (request.headers.authorization !== "Bearer test-secret") {
          response.statusCode = 401;
          response.end();
          return;
        }
        response.end('{"ok":true}');
        return;
      default:
        response.statusCode = 404;
        response.end();
    }
  });
  await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
  const address = server.address() as AddressInfo;
  serverURL = `http://127.0.0.1:${address.port}`;
});

after(async () => {
  await new Promise<void>((resolve, reject) =>
    server.close((error) => (error ? reject(error) : resolve())),
  );
});

test("normalizes only credential-free HTTP server URLs", () => {
  assert.equal(
    normalizeServerURL(" http://127.0.0.1:7799/// "),
    "http://127.0.0.1:7799",
  );
  assert.equal(
    normalizeServerURL("https://example.com/docs/"),
    "https://example.com/docs",
  );
  for (const value of [
    "file:///tmp/docs",
    "ftp://example.com",
    "http://user:pass@example.com",
    "http://example.com?q=1",
    "http://example.com/#x",
  ]) {
    assert.throws(() => normalizeServerURL(value));
  }
});

test("builds encoded API URLs under an optional base path", () => {
  assert.equal(
    apiURL("https://example.com/base", "/api/sources"),
    "https://example.com/base/api/sources",
  );
  assert.equal(
    searchURL("http://127.0.0.1:7799", "row level/security", 20, "post gres"),
    "http://127.0.0.1:7799/api/search?q=row+level%2Fsecurity&limit=20&source=post+gres",
  );
});

test("keeps result paths inside the docs corpus", () => {
  const root = path.resolve("docs-root");
  assert.equal(
    resolveDocumentPath(root, "go/ref/spec.md"),
    path.join(root, "go", "ref", "spec.md"),
  );
  for (const value of [
    "",
    ".",
    "../secret",
    "go/../../secret",
    "/etc/passwd",
    "..\\secret",
    "go\0secret",
  ]) {
    assert.throws(() => resolveDocumentPath(root, value));
  }
});

test("rejects result paths that escape through a symlink", async () => {
  const testRoot = await mkdtemp(path.join(os.tmpdir(), "docs-puller-client-"));
  const docsRoot = path.join(testRoot, "docs");
  const outsideRoot = path.join(testRoot, "outside");
  await mkdir(docsRoot);
  await mkdir(outsideRoot);
  await writeFile(path.join(docsRoot, "safe.md"), "safe");
  await writeFile(path.join(outsideRoot, "secret.md"), "secret");
  await symlink(
    path.join(outsideRoot, "secret.md"),
    path.join(docsRoot, "link.md"),
  );
  try {
    assert.equal(
      await resolveExistingDocumentPath(docsRoot, "safe.md"),
      await realpath(path.join(docsRoot, "safe.md")),
    );
    await assert.rejects(
      resolveExistingDocumentPath(docsRoot, "link.md"),
      /escapes the docs corpus through a link/,
    );
  } finally {
    await rm(testRoot, { recursive: true, force: true });
  }
});

test("validates result limits and escapes copied Markdown links", () => {
  assert.equal(normalizeResultLimit(20), 20);
  for (const value of [0, 101, 1.5, "20", Number.NaN]) {
    assert.throws(() => normalizeResultLimit(value));
  }
  assert.equal(
    markdownLink("A [safe] \\ title", "https://example.com/a_(b)"),
    "[A \\[safe\\] \\\\ title](<https://example.com/a_(b)>)",
  );
});

test("allows only credential-free web origin URLs", () => {
  assert.equal(
    safeOriginURL("https://example.com/a?q=1").toString(),
    "https://example.com/a?q=1",
  );
  for (const value of [
    "file:///tmp/a",
    "javascript:alert(1)",
    "https://user:pass@example.com",
  ]) {
    assert.throws(() => safeOriginURL(value));
  }
});

test("validates source and search response contracts", () => {
  assert.deepEqual(
    parseSourcesResponse({
      root: "/docs",
      sources: [{ name: "go", docs: 10 }],
    }),
    {
      root: "/docs",
      sources: [{ name: "go", docs: 10 }],
    },
  );
  assert.equal(
    parseSearchResponse({
      query: "context",
      mode: "fts5",
      scanned: 10,
      elapsed_ms: 1.5,
      results: [
        {
          path: "go/context.md",
          source: "go",
          score: 2,
          snippets: [{ line: 1, text: "Context" }],
        },
      ],
    }).results[0].path,
    "go/context.md",
  );
  assert.throws(() =>
    parseSourcesResponse({
      root: "/docs",
      sources: [{ name: "go", docs: -1 }],
    }),
  );
  assert.throws(() =>
    parseSearchResponse({
      query: "x",
      mode: "other",
      scanned: 0,
      elapsed_ms: 1,
      results: [],
    }),
  );
  assert.throws(() =>
    parseSearchResponse({
      query: "x",
      mode: "fts5",
      scanned: 0,
      elapsed_ms: 1,
      results: [
        {
          path: "x",
          source: "x",
          score: 1,
          snippets: [{ line: 0, text: "x" }],
        },
      ],
    }),
  );
});

test("fetches bounded JSON and rejects bad responses", async () => {
  assert.deepEqual(await fetchJSON(`${serverURL}/ok`), { ok: true });
  await assert.rejects(
    fetchJSON(`${serverURL}/large`, 5000, 16),
    /exceeds 16 bytes/,
  );
  await assert.rejects(fetchJSON(`${serverURL}/slow`, 10), /request timeout/);
  await assert.rejects(fetchJSON(`${serverURL}/bad-status`), /HTTP 503/);
  await assert.rejects(fetchJSON(`${serverURL}/bad-json`));
  await assert.rejects(fetchJSON("file:///tmp/docs"), /http or https/);
  assert.deepEqual(
    await fetchJSON(`${serverURL}/auth`, 5000, 1024, "test-secret"),
    { ok: true },
  );
  await assert.rejects(
    fetchJSON(`${serverURL}/auth`, 5000, 1024, "bad\r\ntoken"),
    /token is invalid/,
  );
  await assert.rejects(
    fetchJSON("http://example.com/api/search", 5000, 1024, "test-secret"),
    /non-loopback HTTP/,
  );
});
