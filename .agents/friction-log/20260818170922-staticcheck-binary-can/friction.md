---
title: 'Staticcheck binary can be too old for the module Go version'
severity: 'minor'
---

## What happened?

The PATH-resolved Staticcheck 2025.1.1 binary was built with Go 1.24.1 and could not analyze this Go 1.26 module.

## Steps to reproduce

1. Install Staticcheck with Go 1.24.1.
2. Run `staticcheck -tags sqlite_fts5 ./...` in the Go 1.26 docs-puller module.
3. Observe analyzer compile errors before source analysis.

## Expected behavior

The repository gate selects a pinned analyzer that supports the module Go version and fails fast when analysis does not run.

## docs-puller version

devel at commit 3ab65693ab0575b53bb63ff72e0d353b8cc5c4e5

## Operating system

macOS

## Safe diagnostic output

```shell
module requires at least go1.26, but Staticcheck was built with go1.24.1
```

## Submission checks

- [x] I removed tokens, private URLs, and non-public document text.
- [x] I agree to follow the docs-puller Code of Conduct.
