---
title: 'search-batch treats inline --queries JSON as a file path'
severity: 'minor'
---

## What happened?

`search-batch` treated the documented inline JSON value for `--queries` as a file path.

## Steps to reproduce

1. Run `docs-puller search-batch --queries ["one query"]`.
2. Observe the `file name too long` error.

## Expected behavior

The command parses the inline JSON array and runs one query.

## docs-puller version

`v0.8.1-0.20260821055349-4fabb0dcbde7+dirty`

## Operating system

macOS

## Safe diagnostic output

```shell
search-batch: open ["one query"]: file name too long
```

## Submission checks

- [x] I removed tokens, private URLs, and non-public document text.
- [x] I agree to follow the docs-puller Code of Conduct.
