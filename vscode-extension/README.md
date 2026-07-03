# Docs Puller Search — VS Code extension

Thin VS Code frontend over `docs-puller serve`. Adds two commands:

- **Docs Puller: Search** — opens a QuickPick that live-searches the local docs mirror as you type. Click a result to open the Markdown file in the editor and jump to the first matching line. (Bind to a key in your `keybindings.json` if you want a shortcut — left unbound to avoid conflicting with VS Code's defaults.)
- **Docs Puller: Search in Source...** — pick a source (supabase, clickhouse, vscode, etc.) first, then search scoped to it.

Each result has two side-buttons: **open origin URL in browser** and **copy markdown link** (`[Title](URL)`).

Backed by `docs-puller serve` on `http://127.0.0.1:7799` by default. Configurable via `docsPuller.serverUrl` and `docsPuller.resultLimit`.

## Build

```sh
cd vscode-extension
npm install
npm run compile
```

## Develop

Open this folder in VS Code, press **F5**. A new "Extension Development Host" window launches with the extension loaded. Run **Docs Puller: Search** from the Command Palette.

## Install permanently

```sh
npm install
npm run compile
# Then symlink into ~/.vscode/extensions/ (fastest):
ln -s "$PWD" ~/.vscode/extensions/nstranquist.docs-puller-search-0.2.0
# Or package + install:
npx @vscode/vsce package
code --install-extension docs-puller-search-0.2.0.vsix
```

Restart VS Code. The command appears in the palette.

## Architecture

```
QuickPick (debounced 120ms)
    ↓ HTTP GET
docs-puller serve  (localhost:7799)
    ↓ FTS5 query (BM25)
sqlite at ~/code/docs/.cache/search.db
    ↓ ranked paths + URLs + snippets
QuickPick items → click opens vscode.Uri.file(<path>)
```

If the server is down, the extension shows `Run \`docs-puller serve\` to start it` with a "Copy command" button.

## Why a separate process

Keeps the extension dependency-free (just stdlib `http`) and decouples from the docs-puller Go binary version. Update either side independently.
