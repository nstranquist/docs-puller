-- Static embedding-store queries. Dynamic vector candidate queries that need
-- runtime IN-list expansion stay in embed.go.

-- name: ListAllEmbeddings :many
SELECT path, mtime_ns, model, dim, vec
FROM embeddings;

-- name: UpsertEmbedding :exec
INSERT INTO embeddings (path, mtime_ns, model, dim, vec)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT(path, model) DO UPDATE SET
    mtime_ns = excluded.mtime_ns,
    dim      = excluded.dim,
    vec      = excluded.vec;

-- name: ListAllEmbeddingChunks :many
SELECT path, chunk_idx, chunk_size, mtime_ns, model, dim, vec
FROM embedding_chunks;

-- name: UpsertEmbeddingChunk :exec
INSERT INTO embedding_chunks (path, chunk_idx, chunk_size, mtime_ns, model, dim, vec)
VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(path, chunk_idx, model, chunk_size) DO UPDATE SET
    mtime_ns = excluded.mtime_ns,
    dim      = excluded.dim,
    vec      = excluded.vec;

-- name: ListEmbeddingModels :many
SELECT model,
       CAST(COUNT(*) AS INTEGER) AS docs,
       CAST(MAX(dim) AS INTEGER) AS dim
FROM embeddings
GROUP BY model
ORDER BY model;

-- name: InspectEmbeddingCacheByModel :one
SELECT CAST(COUNT(*) AS INTEGER) AS count,
       CAST(COALESCE(MAX(dim), 0) AS INTEGER) AS dim
FROM embeddings
WHERE model = ?;

-- name: ListEmbeddingFlatRowsByModel :many
SELECT path, dim, vec
FROM embeddings
WHERE model = ?
ORDER BY path;

-- name: ListEmbeddingMtimesByModel :many
SELECT path, mtime_ns
FROM embeddings
WHERE model = ?;

-- name: ListEmbeddingVectorsByModel :many
SELECT path, vec
FROM embeddings
WHERE model = ?;

-- name: ListEmbeddingChunkMtimesByModelAndChunkSize :many
SELECT path, chunk_idx, mtime_ns
FROM embedding_chunks
WHERE model = ? AND chunk_size = ?;
