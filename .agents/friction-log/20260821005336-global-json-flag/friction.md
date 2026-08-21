---
title: 'Global JSON flag failed before docs-puller command'
severity: 'minor'
---

## What happened?

`docs-puller --json status` treated `--json` as a subcommand. The command returned the full help text.

## Steps to reproduce

1. Run `docs-puller --json status`.
2. Observe `unknown subcommand: --json`.
3. Run `docs-puller status --json` and observe valid JSON.

## Expected behavior

The agent-oriented JSON flag works before or after the command, as it does in `ndev`.

## docs-puller version

v0.8.0

## Operating system

macOS

## Safe diagnostic output

```shell
unknown subcommand: --json
```

## Submission checks

- [x] I removed tokens, private URLs, and non-public document text.
- [x] I agree to follow the docs-puller Code of Conduct.

The local fix moves leading `--json` and `--compact` flags to the selected command. A regression test covers both placements.
