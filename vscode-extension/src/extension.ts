// Docs Puller Search — VS Code extension that wraps `docs-puller serve`.
//
// Activation is command-driven (no activationEvents needed; VS Code 1.74+
// auto-derives them from contributes.commands). The extension shells out
// nothing — it only fetches the local HTTP API on startup and on each
// keystroke. If the server is down we surface a clear error with the exact
// command to start it.

import * as vscode from "vscode";
import {
  SearchResponse,
  SearchResult,
  apiURL,
  fetchJSON,
  markdownLink,
  normalizeResultLimit,
  normalizeServerURL,
  parseSearchResponse,
  parseSourcesResponse,
  resolveExistingDocumentPath,
  safeOriginURL,
  searchURL,
} from "./client";

let docsRoot = ""; // populated lazily on first command invocation
let docsRootServerURL = "";
let secrets: vscode.SecretStorage | undefined;
const authTokenKey = "docsPuller.authToken";

function cfg() {
  const c = vscode.workspace.getConfiguration("docsPuller");
  return {
    serverUrl: normalizeServerURL(
      c.get<string>("serverUrl") ?? "http://127.0.0.1:7799",
    ),
    limit: normalizeResultLimit(c.get<number>("resultLimit") ?? 20),
  };
}

async function authToken(): Promise<string> {
  return (await secrets?.get(authTokenKey)) ?? "";
}

async function ensureRoot(
  serverUrl = cfg().serverUrl,
  bearerToken?: string,
): Promise<string> {
  if (docsRoot && docsRootServerURL === serverUrl) return docsRoot;
  const token = bearerToken ?? (await authToken());
  const r = parseSourcesResponse(
    await fetchJSON(apiURL(serverUrl, "api/sources"), 5000, undefined, token),
  );
  docsRoot = r.root;
  docsRootServerURL = serverUrl;
  return docsRoot;
}

function showServerDown(err: unknown) {
  const msg = err instanceof Error ? err.message : String(err);
  vscode.window
    .showErrorMessage(
      `Docs Puller: cannot reach docs-puller serve (${msg}). Run \`docs-puller serve\` to start it.`,
      "Copy command",
      "Set auth token",
    )
    .then((choice) => {
      if (choice === "Copy command") {
        vscode.env.clipboard.writeText("docs-puller serve");
      } else if (choice === "Set auth token") {
        vscode.commands.executeCommand("docsPuller.setAuthToken");
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
    detail:
      r.snippets && r.snippets.length
        ? `${r.path}  —  ${r.snippets[0].text}`
        : r.path,
    result: r,
    buttons: [
      {
        iconPath: new vscode.ThemeIcon("link-external"),
        tooltip: "Open origin URL in browser",
      },
      { iconPath: new vscode.ThemeIcon("copy"), tooltip: "Copy markdown link" },
    ],
  }));
}

async function openResult(result: SearchResult) {
  try {
    const { serverUrl } = cfg();
    const root = await ensureRoot(serverUrl, await authToken());
    const fileUri = vscode.Uri.file(
      await resolveExistingDocumentPath(root, result.path),
    );
    const doc = await vscode.workspace.openTextDocument(fileUri);
    await vscode.window.showTextDocument(doc);
    // Best-effort: jump to first matching snippet line.
    if (result.snippets && result.snippets.length) {
      const editor = vscode.window.activeTextEditor;
      if (editor) {
        const line = Math.min(
          doc.lineCount - 1,
          Math.max(0, result.snippets[0].line - 1),
        );
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
  let serverUrl: string;
  let limit: number;
  let bearerToken: string;
  try {
    ({ serverUrl, limit } = cfg());
    bearerToken = await authToken();
  } catch (err) {
    showServerDown(err);
    return;
  }
  // Probe sources up-front so we can surface a clear error before the user
  // wonders why typing returns nothing.
  try {
    await ensureRoot(serverUrl, bearerToken);
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
      const resp = parseSearchResponse(
        await fetchJSON(
          searchURL(serverUrl, value, limit, scopedSource),
          5000,
          undefined,
          bearerToken,
        ),
      );
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
      vscode.window.showInformationMessage(
        "This doc has no origin URL recorded.",
      );
      return;
    }
    let originURL: URL;
    try {
      originURL = safeOriginURL(r.url);
    } catch (err) {
      vscode.window.showInformationMessage(
        `Cannot use this origin URL: ${err}`,
      );
      return;
    }
    const tooltip = String(e.button.tooltip ?? "");
    if (tooltip.startsWith("Copy")) {
      await vscode.env.clipboard.writeText(
        markdownLink(r.title || r.path, originURL.toString()),
      );
      vscode.window.setStatusBarMessage("$(check) Markdown link copied", 1500);
    } else {
      await vscode.env.openExternal(vscode.Uri.parse(originURL.toString()));
    }
  });

  qp.onDidHide(() => {
    clearTimeout(debounceTimer);
    seq++;
    qp.dispose();
  });
  qp.show();
}

async function pickSource(): Promise<string | undefined> {
  let resp;
  try {
    const { serverUrl } = cfg();
    resp = parseSourcesResponse(
      await fetchJSON(
        apiURL(serverUrl, "api/sources"),
        5000,
        undefined,
        await authToken(),
      ),
    );
  } catch (err) {
    showServerDown(err);
    return undefined;
  }
  const picked = await vscode.window.showQuickPick(
    resp.sources.map((s) => ({
      label: s.name,
      description: `${s.docs} docs`,
    })),
    { placeHolder: "Pick a source to scope your search" },
  );
  return picked?.label;
}

export function activate(context: vscode.ExtensionContext) {
  secrets = context.secrets;
  context.subscriptions.push(
    vscode.commands.registerCommand("docsPuller.search", () => runSearchUI()),
    vscode.commands.registerCommand("docsPuller.searchScoped", async () => {
      const source = await pickSource();
      if (source) await runSearchUI(source);
    }),
    vscode.commands.registerCommand("docsPuller.setAuthToken", async () => {
      const value = await vscode.window.showInputBox({
        prompt:
          "Set the bearer token for docs-puller serve. Leave it empty to remove the saved token.",
        password: true,
        ignoreFocusOut: true,
      });
      if (value === undefined) return;
      if (value.trim()) {
        await context.secrets.store(authTokenKey, value.trim());
        vscode.window.showInformationMessage(
          "Docs Puller: authentication token saved in VS Code SecretStorage.",
        );
      } else {
        await context.secrets.delete(authTokenKey);
        vscode.window.showInformationMessage(
          "Docs Puller: authentication token removed.",
        );
      }
      docsRoot = "";
      docsRootServerURL = "";
    }),
    vscode.workspace.onDidChangeConfiguration((event) => {
      if (event.affectsConfiguration("docsPuller.serverUrl")) {
        docsRoot = "";
        docsRootServerURL = "";
      }
    }),
  );
}

export function deactivate() {}
