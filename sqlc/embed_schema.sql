CREATE TABLE IF NOT EXISTS embeddings (
  path     TEXT NOT NULL,
  mtime_ns INTEGER NOT NULL,
  model    TEXT NOT NULL,
  dim      INTEGER NOT NULL,
  vec      BLOB NOT NULL,
  PRIMARY KEY (path, model)
);

CREATE INDEX IF NOT EXISTS embeddings_model_idx
  ON embeddings(model);

CREATE TABLE IF NOT EXISTS embedding_chunks (
  path       TEXT NOT NULL,
  chunk_idx  INTEGER NOT NULL,
  chunk_size INTEGER NOT NULL,
  mtime_ns   INTEGER NOT NULL,
  model      TEXT NOT NULL,
  dim        INTEGER NOT NULL,
  vec        BLOB NOT NULL,
  PRIMARY KEY (path, chunk_idx, model, chunk_size)
);

CREATE INDEX IF NOT EXISTS embedding_chunks_path_idx
  ON embedding_chunks(path, model, chunk_size);
