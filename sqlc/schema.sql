-- Typed mirror of cmd/docs-puller/search_fts.go runtime FTS5 schema. Runtime
-- uses CREATE VIRTUAL TABLE docs USING fts5(...); this regular table mirror
-- gives sqlc enough column and rowid shape to type-check static cache queries.

CREATE TABLE IF NOT EXISTS docs (
    "rowid" INTEGER PRIMARY KEY,
    path TEXT NOT NULL,
    source TEXT NOT NULL,
    title TEXT,
    path_tokens TEXT NOT NULL,
    body TEXT NOT NULL,
    url TEXT,
    is_hub INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS docs_path (
    path TEXT PRIMARY KEY,
    "rowid" INTEGER NOT NULL
);
