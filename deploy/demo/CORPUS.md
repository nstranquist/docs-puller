# Public corpus policy

The production demo contains 24 reviewed public documentation pages. It has
eight pages from each source:

- SQLite documentation from `sqlite.org`. SQLite states that its code and
  documentation are in the public domain.
- Go documentation from `go.dev`. Except where noted, the Go website publishes
  its content under CC BY 4.0. Code uses a BSD-style license.
- PostgreSQL documentation from `postgresql.org`. The PostgreSQL License
  permits use and distribution of the software and its documentation.

The authoritative URL list is
[`eval/sample-corpus/sources.md`](../../eval/sample-corpus/sources.md). The
authoritative byte-level review is [`corpus.lock.json`](corpus.lock.json).

License references:

- <https://www.sqlite.org/copyright.html>
- <https://go.dev/copyright>
- <https://www.postgresql.org/about/licence/>

## Inclusion rules

A document can enter this corpus only when all these statements are true:

1. Its URL is in the reviewed source list.
2. It uses HTTPS and an exact allowlisted host.
3. Its manifest source, mode, path, URL, digest, and fetch time are valid.
4. Its content is no larger than 2 MiB.
5. The complete corpus is no larger than 16 MiB.
6. The corpus contains exactly 24 documents from exactly three sources.
7. The local tree contains no symlink, private source, or unreviewed file.
8. The pulled bytes match the reviewed lock.
9. The SQLite index passes `PRAGMA integrity_check` and contains 24 documents.

The container does not receive `_INGEST_LOG.jsonl`, write locks, title caches,
or any file that is not needed at runtime.

## Refresh procedure

The scheduled refresh workflow pulls all 24 URLs and verifies the existing
lock. It never updates production content automatically.

If upstream bytes change:

1. Inspect the changed source pages and their license status.
2. Run the retrieval eval and compare Hit@1, Hit@5, MRR, p50, and p99.
3. Confirm that no private or unrelated content entered the corpus.
4. Run the corpus builder with `--write-lock`.
5. Review the complete lock diff.
6. Run the full demo CI and managed-browser review.
7. Deploy the origin before the Worker metadata.

The lock ignores only the fresh fetch timestamp when it compares a replay.
Every source URL, path, license label, byte count, and content digest must stay
equal.
