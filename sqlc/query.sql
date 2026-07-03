-- Static FTS cache queries for docs-puller. Dynamic ranking SELECTs stay in Go
-- because they compose tier-specific projections and optional source filters.

-- name: ProbeDocsIsHub :many
SELECT is_hub FROM docs LIMIT 0;

-- name: DropDocs :exec
DROP TABLE IF EXISTS docs;

-- name: DropDocsPath :exec
DROP TABLE IF EXISTS docs_path;

-- name: CountDocs :one
SELECT CAST(COUNT(*) AS INTEGER) FROM docs;

-- name: LookupDocRowIDByPath :one
SELECT CAST("rowid" AS INTEGER) FROM docs_path WHERE path = ?;

-- name: DeleteDocByRowID :exec
DELETE FROM docs WHERE rowid = ?;

-- name: DeletePathByPath :exec
DELETE FROM docs_path WHERE path = ?;

-- name: DeletePathsBySource :exec
DELETE FROM docs_path
WHERE "rowid" IN (SELECT "rowid" FROM docs WHERE source = ?);

-- name: DeleteDocsBySource :exec
DELETE FROM docs WHERE source = ?;

-- name: DeleteAllDocs :exec
DELETE FROM docs;

-- name: DeleteAllDocsPath :exec
DELETE FROM docs_path;

-- name: InsertDoc :execresult
INSERT INTO docs(path, source, title, path_tokens, body, url, is_hub)
VALUES(?, ?, ?, ?, ?, ?, ?);

-- name: InsertDocWithRowID :exec
INSERT INTO docs("rowid", path, source, title, path_tokens, body, url, is_hub)
VALUES(?, ?, ?, ?, ?, ?, ?, ?);

-- name: InsertPath :exec
INSERT INTO docs_path(path, "rowid") VALUES(?, ?);

-- name: RebuildPaths :exec
INSERT INTO docs_path(path, "rowid") SELECT path, "rowid" FROM docs;
