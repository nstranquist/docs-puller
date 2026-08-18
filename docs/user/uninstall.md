# Uninstall docs-puller

Uninstall the binary separately from your corpus and configuration. Removing
the binary does not remove data.

## Remove a Go-installed binary

Find the active binary first:

```sh
command -v docs-puller
go env GOBIN
go env GOPATH
```

When `GOBIN` is set, Go installs the binary there. Otherwise, the normal path is
`$(go env GOPATH)/bin/docs-puller`. After you verify the path, remove only the
exact binary that `command -v` reports.

## Remove a release-archive install

Find the active binary with `command -v docs-puller` on macOS or Linux, or
`Get-Command docs-puller` in PowerShell. Remove that exact binary and the archive
you downloaded.

## Review local data

The usual paths are:

- `~/code/docs`: the default corpus.
- `~/.docs-puller`: optional configuration, profiles, and token files.
- A custom directory passed with `--out` or `DOCS_PULLER_OUT`.

Before deletion, inspect and back up each path. A corpus can contain source
documents, query logs, indexes, and embeddings. You cannot recover this data
from the binary. If possible, use your operating system's trash or recycle bin.

## Remove the VS Code extension

If you installed the VSIX, open the Extensions view and uninstall **Docs Puller
Search**. After you verify that a manual symlink points to this repository,
remove it from the VS Code extensions directory.
