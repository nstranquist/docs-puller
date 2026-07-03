CREATE VIRTUAL TABLE IF NOT EXISTS docs USING fts5(
    path UNINDEXED,
    source UNINDEXED,
    title,
    path_tokens,
    body,
    url UNINDEXED,
    is_hub UNINDEXED,
    tokenize='porter unicode61'
);

-- Side table for O(log N) path-to-rowid lookups. FTS5's UNINDEXED columns are
-- not backed by a btree, so DELETE WHERE path = ? would otherwise scan the
-- whole index. docs_path stays in sync with docs through the sqlc-backed
-- insert, upsert, and rebuild operations.
CREATE TABLE IF NOT EXISTS docs_path (
    path TEXT PRIMARY KEY,
    rowid INTEGER NOT NULL
);
