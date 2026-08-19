# Docs Puller Search for VS Code

This extension searches a local docs-puller corpus from VS Code. It uses the
HTTP API from `docs-puller serve` and adds three commands:

- **Docs Puller: Search** searches all sources.
- **Docs Puller: Search in Source...** selects one source before the search.
- **Docs Puller: Set Authentication Token...** stores or removes a server token.

A result can open the local Markdown file, open its public origin URL, or copy a
Markdown link. When line data is available, the editor moves to the first
matching snippet.

## Install the release VSIX

The extension is distributed as an asset with a SHA-256 checksum on the
docs-puller GitHub release. It is not published in the VS Code Marketplace.

Download `docs-puller-search-0.3.0.vsix` and `vsix-checksums.txt` from the
v0.7.5 release. Check the checksum before installation:

```sh
shasum -a 256 -c vsix-checksums.txt
code --install-extension docs-puller-search-0.3.0.vsix
```

On Linux, you can use `sha256sum --check vsix-checksums.txt`.

Start the local server:

```sh
docs-puller serve
```

The default endpoint is `http://127.0.0.1:7799`. If your server uses another
address, change `docsPuller.serverUrl`.

## Use an authenticated server

Run **Docs Puller: Set Authentication Token...** from the Command Palette. The
extension stores the token in VS Code SecretStorage. It does not put the token
in settings or URLs.

The client sends a bearer token to HTTPS endpoints and loopback HTTP endpoints.
It refuses to send a token over non-loopback HTTP.

## Build and test from source

Node.js 22 and Go 1.26.6 are required.

```sh
npm ci
npm run check
npm test
npm run package
```

The package command creates `docs-puller-search-0.3.0.vsix`. A Go normalizer
removes variable ZIP metadata, verifies the package identity and file policy,
and refuses to overwrite different output. Two builds from the same commit
produce the same VSIX bytes.

For extension development, open this directory in VS Code and press **F5**.
This opens an Extension Development Host window.

## Security boundaries

- The client accepts only HTTP and HTTPS server URLs.
- Server responses are limited to 4 MiB.
- A returned document path must stay inside the corpus root.
- An origin link must use HTTP or HTTPS and must not contain credentials.
- The packaged VSIX excludes source files, dependencies, source maps, and local
  settings.

## Architecture

```text
QuickPick
    -> bounded HTTP request
docs-puller serve
    -> SQLite FTS5 search
local docs corpus
    -> ranked paths, origin URLs, and snippets
QuickPick result
```

The extension has no runtime package dependency. Search and indexing remain in
the docs-puller process, so the extension stays small.
