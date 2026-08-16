# Security Policy

## Supported Versions

The latest SemVer release receives security fixes. Verify the problem on the
latest release before you report it.

## Report a Vulnerability

Use [GitHub private vulnerability reporting](https://github.com/nstranquist/docs-puller/security/advisories/new).
Do not put vulnerability details in a public issue.

Include this information:

- The affected version and operating system.
- The smallest safe reproduction.
- The expected result and the observed result.
- The security impact.
- Any temporary mitigation that you tested.

Remove private documents, query text, access tokens, and credentials from the
report. If a corpus is necessary, use a small synthetic corpus.

## Security Boundaries

`docs-puller` stores copied documents, indexes, embeddings, and query logs on
the local machine. These files can contain private text.

Treat pulled documents as untrusted input. Verify the original source before
you execute commands or code from a pulled document.

The `serve` command binds to `127.0.0.1` by default. It rejects a non-loopback
address without a bearer token. The built-in server does not provide TLS.

The optional PDF path executes only the binaries recorded in a provider pin.
It rejects a missing or changed SHA-256 hash.

Reports about path traversal, unintended network access, credential exposure,
authorization bypass, or unsafe provider execution are in scope.
