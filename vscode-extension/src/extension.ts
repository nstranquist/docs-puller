// Docs Puller Search — VS Code extension that wraps `docs-puller serve`.
//
// Activation is command-driven (no activationEvents needed; VS Code 1.74+
// auto-derives them from contributes.commands). The extension shells out
// nothing — it only fetches the local HTTP API on startup and on each
// keystroke. If the server is down we surface a clear error with the exact
// command to start it.

import * as vscode from "vscode";
import * as http from "http";
import * as https from "https";
import { URL } from "url";

interface SearchResult {
  path: string;
  source: string;
  title?: string;
  url?: string;
  score: number;
  snippets?: { line: number; text: string }[];
}

interface SearchResponse {
  query: string;
  mode: "fts5" | "scan";
  scanned: number;
  elapsed_ms: number;
  results: SearchResult[];
}

interface SourcesResponse {
  root: string;
  sources: { name: string; docs: number }[];
}

let docsRoot = ""; // populated lazily on first command invocation

function cfg() {
  const c = vscode.workspace.getConfiguration("docsPuller");
  return {
    serverUrl: (c.get<string>("serverUrl") ?? "http://127.0.0.1:7799").replace(/\/$/, ""),
    limit: c.get<number>("resultLimit") ?? 20,
  };
}

// fetchJSON is a tiny stdlib-only HTTP GET helper. We don't pull in node-fetch
// to keep the extension dependency-free and the package small.
function fetchJSON<T>(rawURL: string, timeoutMs = 5000): Promise<T> {
  return new Promise((resolve, reject) => {
    const u = new URL(rawURL);
    const lib = u.protocol === "https:" ? https : http;
    const req = lib.get(rawURL, (res) => {
      const chunks: Buffer[] = [];
      res.on("data", (c) => chunks.push(c));
      res.on("end", () => {
        if (res.statusCode && res.statusCode >= 400) {
          reject(new Error(`HTTP ${res.statusCode}`));
          return;
        }
        try {
          resolve(JSON.parse(Buffer.concat(chunks).toString("utf-8")) as T);
        } catch (e) {
          reject(e);
        }
      });
    });
    req.on("error", reject);
    req.setTimeout(timeoutMs, () => {
      req.destroy(new Error("request timeout"));
    });
  });
}

async function ensureRoot(): Promise<string> {
  if (docsRoot) return docsRoot;
  const { serverUrl } = cfg();
  const r = await fetchJSON<SourcesResponse>(`${serverUrl}/api/sources`);
  docsRoot = r.root;
  return docsRoot;
}

function showServerDown(err: unknown) {
  const msg = err instanceof Error ? err.message : String(err);
  vscode.window
    .showErrorMessage(
      `Docs Puller: cannot reach docs-puller serve (${msg}). Run \`docs-puller serve\` to start it.`,
      "Copy command"
    )
    .then((choice) => {
      if (choice === "Copy command") {
        vscode.env.clipboard.writeText("docs-puller serve");
      }
    });
}

interface ResultItem extends vscode.QuickPickItem {
  result: SearchResult;
}

function buildItems(resp: SearchResponse): ResultItem[] {
  return resp.results.map((r) => ({
    label: `$(book) ${r.title || "(untitled)"}`,
    description: `[${r.source}] · score ${r.score}`,
    detail: r.snippets && r.snippets.length
      ? `${r.path}  —  ${r.snippets[0].text}`
      : r.path,
    result: r,
    buttons: [
      { iconPath: new vscode.ThemeIcon("link-external"), tooltip: "Open origin URL in browser" },
      { iconPath: new vscode.ThemeIcon("copy"), tooltip: "Copy markdown link" },
    ],
  }));
}

async function openResult(result: SearchResult) {
  try {
    const root = await ensureRoot();
    const fileUri = vscode.Uri.file(`${root}/${result.path}`);
    const doc = await vscode.workspace.openTextDocument(fileUri);
    await vscode.window.showTextDocument(doc);
    // Best-effort: jump to first matching snippet line.
    if (result.snippets && result.snippets.length) {
      const editor = vscode.window.activeTextEditor;
      if (editor) {
        const line = Math.max(0, result.snippets[0].line - 1);
        const range = new vscode.Range(line, 0, line, 0);
        editor.revealRange(range, vscode.TextEditorRevealType.InCenter);
        editor.selection = new vscode.Selection(range.start, range.start);
      }
    }
  } catch (err) {
    vscode.window.showErrorMessage(`Failed to open: ${err}`);
  }
}

async function runSearchUI(scopedSource?: string) {
  const { serverUrl, limit } = cfg();
  // Probe sources up-front so we can surface a clear error before the user
  // wonders why typing returns nothing.
  try {
    await ensureRoot();
  } catch (err) {
    showServerDown(err);
    return;
  }

  const qp = vscode.window.createQuickPick<ResultItem>();
  qp.placeholder = scopedSource
    ? `Search ${scopedSource} docs...`
    : "Search local docs (FTS5)...";
  qp.matchOnDescription = true;
  qp.matchOnDetail = true;

  let seq = 0;
  let debounceTimer: NodeJS.Timeout | undefined;

  const fetchAndSet = async (value: string) => {
    if (!value.trim()) {
      qp.items = [];
      return;
    }
    const mySeq = ++seq;
    qp.busy = true;
    try {
      const params = new URLSearchParams({ q: value, limit: String(limit) });
      if (scopedSource) params.set("source", scopedSource);
      const resp = await fetchJSON<SearchResponse>(`${serverUrl}/api/search?${params.toString()}`);
      if (mySeq !== seq) return; // stale response
      qp.items = buildItems(resp);
      qp.title = `${resp.results.length} results · ${resp.scanned} scanned · ${resp.elapsed_ms}ms · ${resp.mode}`;
    } catch (err) {
      if (mySeq !== seq) return;
      qp.items = [
        {
          label: `$(error) ${err instanceof Error ? err.message : String(err)}`,
          description: "fetch failed",
          detail: "Run `docs-puller serve` to start the search server.",
          result: { path: "", source: "", score: 0 },
        },
      ];
    } finally {
      if (mySeq === seq) qp.busy = false;
    }
  };

  qp.onDidChangeValue((value) => {
    clearTimeout(debounceTimer);
    debounceTimer = setTimeout(() => fetchAndSet(value), 120);
  });

  qp.onDidAccept(async () => {
    const item = qp.activeItems[0];
    if (item && item.result.path) {
      qp.hide();
      await openResult(item.result);
    }
  });

  qp.onDidTriggerItemButton(async (e) => {
    const r = e.item.result;
    if (!r.url) {
      vscode.window.showInformationMessage("This doc has no origin URL recorded.");
      return;
    }
    const tooltip = String(e.button.tooltip ?? "");
    if (tooltip.startsWith("Copy")) {
      await vscode.env.clipboard.writeText(`[${r.title || r.path}](${r.url})`);
      vscode.window.setStatusBarMessage("$(check) Markdown link copied", 1500);
    } else {
      vscode.env.openExternal(vscode.Uri.parse(r.url));
    }
  });

  qp.onDidHide(() => qp.dispose());
  qp.show();
}

async function pickSource(): Promise<string | undefined> {
  const { serverUrl } = cfg();
  let resp: SourcesResponse;
  try {
    resp = await fetchJSON<SourcesResponse>(`${serverUrl}/api/sources`);
  } catch (err) {
    showServerDown(err);
    return undefined;
  }
  const picked = await vscode.window.showQuickPick(
    resp.sources.map((s) => ({
      label: s.name,
      description: `${s.docs} docs`,
    })),
    { placeHolder: "Pick a source to scope your search" }
  );
  return picked?.label;
}

export function activate(context: vscode.ExtensionContext) {
  context.subscriptions.push(
    vscode.commands.registerCommand("docsPuller.search", () => runSearchUI()),
    vscode.commands.registerCommand("docsPuller.searchScoped", async () => {
      const source = await pickSource();
      if (source) await runSearchUI(source);
    })
  );
}

export function deactivate() {}
