# SQLite FTS5 quick start

Create a full-text search table with SQLite FTS5:

```sql
CREATE VIRTUAL TABLE docs USING fts5(title, body);
```

Insert documents, then search them with a `MATCH` expression. FTS5 ranks the
matching rows with BM25 and supports tokenizers, prefix indexes, and snippets.
