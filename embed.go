package main

import (
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/nstranquist/docs-puller/internal/embeddb"
	"github.com/nstranquist/docs-puller/searchruntime"
)

// === Corpus-agnostic ===
// This file operates on (path, text) -> vector and does not depend on the
// docs-vs-refs distinction. Embedding storage, batching, oversize-fallback,
// cosine, RRF rerank, hybrid retrieval, and chunked max-pool are all generic.
// Candidate for extraction to internal/corpora/embed/ once a second consumer
// (e.g. cmd/refs-puller/) needs the same shape. Until then: copy on Phase 2,
// extract on Phase 3 per the rule-of-three.
// See docs/active/05-02-2244-corpus-core-extraction-strategy.md.

// Embedding reranker prototype. Two surfaces:
//
//	docs-puller embed [--source NAME] [--model M] [--reembed] [--max-cost USD]
//	docs-puller search <q> --rerank [--rerank-k N]
//
// Storage lives in <out>/.cache/embeddings.db, separate from the FTS5 search
// index. One row per doc keyed by path; vector serialized as little-endian
// float32 blob. Search keeps a legacy read fallback for old corpora that still
// have embeddings in <out>/.cache/search.db.
// Incremental — skips docs whose (mtime_ns, model) already matches the cached
// row. `--reembed` forces a full rebuild for the matched scope.
//
// Provider is OpenAI only for this prototype (text-embedding-3-small or -large).
// If quality lift holds we'll port to local embeddings (BGE / nomic on MLX);
// keeping the provider cloud-only here lets us answer the quality question
// without confounding it with model-runtime engineering.

// embedSchema declares the on-disk shape for embeddings.db. The composite
// PRIMARY KEY (path, model) is load-bearing: it lets a single doc carry
// vectors from multiple embedding models simultaneously (small, large,
// gemini-embedding-001, etc.). The 2026-05-04 incident — where embedding
// model X overwrote all rows for model Y — was caused by the previous
// `path TEXT PRIMARY KEY` shape; the upsert ON CONFLICT(path) re-bound the
// model column. Existing DBs auto-migrate via migrateEmbeddingsPKIfNeeded
// on first open.
const embedSchema = `
CREATE TABLE IF NOT EXISTS embeddings (
    path     TEXT NOT NULL,
    mtime_ns INTEGER NOT NULL,
    model    TEXT NOT NULL,
    dim      INTEGER NOT NULL,
    vec      BLOB NOT NULL,
    PRIMARY KEY (path, model)
);
CREATE INDEX IF NOT EXISTS embeddings_model_idx ON embeddings(model);

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
CREATE INDEX IF NOT EXISTS embedding_chunks_path_idx ON embedding_chunks(path, model, chunk_size);`

const flatEmbeddingVersion = 1

type embeddingScoredPath struct {
	path string
	cos  float32
}

func (candidate embeddingScoredPath) HybridPath() string { return candidate.path }

func (candidate embeddingScoredPath) HybridScore() float32 { return candidate.cos }

type flatEmbeddingMeta struct {
	Version     int                  `json:"version"`
	Model       string               `json:"model"`
	Dim         int                  `json:"dim"`
	Count       int                  `json:"count"`
	VectorFile  string               `json:"vector_file"`
	GeneratedAt string               `json:"generated_at"`
	Entries     []flatEmbeddingEntry `json:"entries"`
}

type flatEmbeddingEntry struct {
	Path string `json:"path"`
}

func embeddingsDBPath(out string) string {
	return filepath.Join(out, ".cache", "embeddings.db")
}

func openEmbeddingsDB(out string, readOnly bool) (*sql.DB, error) {
	path := embeddingsDBPath(out)
	if readOnly {
		path = embeddingsReadDBPath(out)
	} else if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}

	dsn := path
	if readOnly {
		dsn += "?mode=ro"
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec("PRAGMA busy_timeout=5000;"); err != nil {
		db.Close()
		return nil, searchruntime.EmbeddingDBBusyTimeoutError(err)
	}
	if !readOnly {
		if _, err := db.Exec("PRAGMA journal_mode=WAL;"); err != nil {
			db.Close()
			return nil, searchruntime.EmbeddingDBJournalModeError(err)
		}
		if _, err := db.Exec(embedSchema); err != nil {
			db.Close()
			return nil, searchruntime.EmbeddingDBSchemaEnsureError(err)
		}
		if err := migrateEmbeddingsPKIfNeeded(db); err != nil {
			db.Close()
			return nil, searchruntime.EmbeddingDBPKMigrationError(err)
		}
	}
	return db, nil
}

// migrateEmbeddingsPKIfNeeded detects an old `path TEXT PRIMARY KEY` schema
// (where embedding model X silently overwrote model Y's rows on upsert) and
// rewrites the embeddings table in-place to use a composite PRIMARY KEY
// (path, model). Idempotent: detection short-circuits on already-migrated
// DBs. Auto-runs from openEmbeddingsDB so no operator action is needed.
//
// Pattern precedent: migrateLegacyEmbeddings (see below) uses the same
// transaction-wrapped table-rewrite pattern.
//
// 2026-05-04 incident: an `embed --model text-embedding-3-large` run
// nuked text-embedding-3-small rows in SQLite (and vice-versa), because
// the upsert ON CONFLICT(path) re-bound the model column. The flat sidecar
// (per-model) survived independently, which masked the SQLite drift in
// production retrieval. Caught by self-audit during post-shipping review.
func migrateEmbeddingsPKIfNeeded(db *sql.DB) error {
	var schemaSQL sql.NullString
	if err := db.QueryRow(
		`SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'embeddings'`,
	).Scan(&schemaSQL); err != nil {
		// Table doesn't exist yet (CREATE IF NOT EXISTS just no-oped on a
		// fresh DB before our exec landed) — nothing to migrate.
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return searchruntime.EmbeddingSchemaReadError(err)
	}
	if !schemaSQL.Valid {
		return nil
	}
	// Already on the composite-PK shape? Nothing to do.
	if strings.Contains(schemaSQL.String, "PRIMARY KEY (path, model)") {
		return nil
	}
	// Old shape detected. Rewrite in a transaction.
	tx, err := db.Begin()
	if err != nil {
		return searchruntime.EmbeddingMigrationBeginError(err)
	}
	defer tx.Rollback()
	migrationSteps := []string{
		`CREATE TABLE embeddings_new (
            path     TEXT NOT NULL,
            mtime_ns INTEGER NOT NULL,
            model    TEXT NOT NULL,
            dim      INTEGER NOT NULL,
            vec      BLOB NOT NULL,
            PRIMARY KEY (path, model)
        )`,
		`INSERT INTO embeddings_new (path, mtime_ns, model, dim, vec)
            SELECT path, mtime_ns, model, dim, vec FROM embeddings`,
		`DROP TABLE embeddings`,
		`ALTER TABLE embeddings_new RENAME TO embeddings`,
		`CREATE INDEX IF NOT EXISTS embeddings_model_idx ON embeddings(model)`,
	}
	for _, step := range migrationSteps {
		if _, err := tx.Exec(step); err != nil {
			return searchruntime.EmbeddingMigrationStepError(step[:40], err)
		}
	}
	return tx.Commit()
}

func embeddingsReadDBPath(out string) string {
	path := embeddingsDBPath(out)
	if _, err := os.Stat(path); err == nil {
		return path
	}
	legacy := ftsDBPath(out)
	if sqliteTableExists(legacy, "embeddings") || sqliteTableExists(legacy, "embedding_chunks") {
		return legacy
	}
	return path
}

func sqliteTableExists(path, table string) bool {
	if _, err := os.Stat(path); err != nil {
		return false
	}
	db, err := sql.Open("sqlite", path+"?mode=ro")
	if err != nil {
		return false
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`,
		table,
	).Scan(&n); err != nil {
		return false
	}
	return n > 0
}

func migrateLegacyEmbeddings(out string) error {
	legacyPath := ftsDBPath(out)
	if !sqliteTableExists(legacyPath, "embeddings") && !sqliteTableExists(legacyPath, "embedding_chunks") {
		return searchruntime.EmbeddingLegacyNotFoundError(legacyPath)
	}
	src, err := sql.Open("sqlite", legacyPath+"?mode=ro")
	if err != nil {
		return searchruntime.EmbeddingLegacyOpenError(err)
	}
	defer src.Close()
	if _, err := src.Exec("PRAGMA busy_timeout=5000;"); err != nil {
		return searchruntime.EmbeddingLegacyBusyTimeoutError(err)
	}
	dst, err := openEmbeddingsDB(out, false)
	if err != nil {
		return searchruntime.EmbeddingIndexOpenError(err)
	}
	defer dst.Close()

	docs, err := copyLegacyWholeEmbeddings(src, dst)
	if err != nil {
		return err
	}
	chunks, err := copyLegacyChunkEmbeddings(src, dst)
	if err != nil {
		return err
	}
	models, err := collectEmbeddingModels(dst)
	if err != nil {
		return err
	}
	for _, model := range models {
		if err := writeFlatEmbeddingIndex(dst, out, model.Model); err != nil {
			return searchruntime.EmbeddingFlatIndexWriteModelError(model.Model, err)
		}
	}
	fmt.Print(searchruntime.EmbeddingLegacyMigrationSummaryMessage(docs, chunks, len(models), embeddingsDBPath(out)))
	return nil
}

func copyLegacyWholeEmbeddings(src, dst *sql.DB) (int, error) {
	if !legacyDBHasTable(src, "embeddings") {
		return 0, nil
	}
	ctx := context.Background()
	rows, err := embeddb.New(src).ListAllEmbeddings(ctx)
	if err != nil {
		return 0, err
	}
	tx, err := dst.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	q := embeddb.New(tx)
	n := 0
	for _, row := range rows {
		if err := q.UpsertEmbedding(ctx, embeddb.UpsertEmbeddingParams(row)); err != nil {
			return 0, err
		}
		n++
	}
	return n, tx.Commit()
}

func copyLegacyChunkEmbeddings(src, dst *sql.DB) (int, error) {
	if !legacyDBHasTable(src, "embedding_chunks") {
		return 0, nil
	}
	ctx := context.Background()
	rows, err := embeddb.New(src).ListAllEmbeddingChunks(ctx)
	if err != nil {
		return 0, err
	}
	tx, err := dst.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	q := embeddb.New(tx)
	n := 0
	for _, row := range rows {
		if err := q.UpsertEmbeddingChunk(ctx, embeddb.UpsertEmbeddingChunkParams(row)); err != nil {
			return 0, err
		}
		n++
	}
	return n, tx.Commit()
}

func legacyDBHasTable(db *sql.DB, table string) bool {
	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`,
		table,
	).Scan(&n); err != nil {
		return false
	}
	return n > 0
}

func flatEmbeddingPaths(out, model string) (metaPath, vecPath string) {
	base := "embeddings-" + safeFileComponent(model)
	cacheDir := filepath.Join(out, ".cache")
	return filepath.Join(cacheDir, base+".json"), filepath.Join(cacheDir, base+".vec")
}

func safeFileComponent(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "default"
	}
	return out
}

func writeFlatEmbeddingIndex(db *sql.DB, out, model string) error {
	metaPath, vecPath := flatEmbeddingPaths(out, model)
	cacheDir := filepath.Dir(metaPath)
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return err
	}

	rows, err := embeddb.New(db).ListEmbeddingFlatRowsByModel(context.Background(), model)
	if err != nil {
		return err
	}

	vecTmp, err := os.CreateTemp(cacheDir, ".embeddings-*.vec")
	if err != nil {
		return err
	}
	vecTmpName := vecTmp.Name()
	cleanupVec := true
	defer func() {
		if cleanupVec {
			_ = os.Remove(vecTmpName)
		}
	}()

	meta := flatEmbeddingMeta{
		Version:     flatEmbeddingVersion,
		Model:       model,
		VectorFile:  filepath.Base(vecPath),
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		Entries:     []flatEmbeddingEntry{},
	}
	for _, row := range rows {
		path := row.Path
		dim := int(row.Dim)
		blob := row.Vec
		if dim <= 0 || len(blob) != dim*4 {
			_ = vecTmp.Close()
			return searchruntime.EmbeddingFlatIndexVectorInvalidError(path, dim, len(blob))
		}
		if meta.Dim == 0 {
			meta.Dim = dim
		} else if meta.Dim != dim {
			_ = vecTmp.Close()
			return searchruntime.EmbeddingFlatIndexMixedDimensionsError(model, meta.Dim, dim)
		}
		if _, err := vecTmp.Write(blob); err != nil {
			_ = vecTmp.Close()
			return err
		}
		meta.Entries = append(meta.Entries, flatEmbeddingEntry{Path: path})
	}
	meta.Count = len(meta.Entries)
	if err := vecTmp.Close(); err != nil {
		return err
	}

	metaTmp, err := os.CreateTemp(cacheDir, ".embeddings-*.json")
	if err != nil {
		return err
	}
	metaTmpName := metaTmp.Name()
	cleanupMeta := true
	defer func() {
		if cleanupMeta {
			_ = os.Remove(metaTmpName)
		}
	}()
	enc := json.NewEncoder(metaTmp)
	enc.SetIndent("", "  ")
	if err := enc.Encode(meta); err != nil {
		_ = metaTmp.Close()
		return err
	}
	if err := metaTmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(vecTmpName, vecPath); err != nil {
		return err
	}
	cleanupVec = false
	if err := os.Rename(metaTmpName, metaPath); err != nil {
		return err
	}
	cleanupMeta = false
	return nil
}

func topFlatEmbeddingPaths(out, model string, queryVec []float32, k int, depthPenalty float32) ([]embeddingScoredPath, bool, error) {
	return topFlatEmbeddingPathsForSource(out, model, queryVec, k, depthPenalty, "")
}

func topFlatEmbeddingPathsForSource(out, model string, queryVec []float32, k int, depthPenalty float32, source string) ([]embeddingScoredPath, bool, error) {
	metaPath, vecPath := flatEmbeddingPaths(out, model)
	data, err := os.ReadFile(metaPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	var meta flatEmbeddingMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		return nil, false, err
	}
	if meta.Version != flatEmbeddingVersion || meta.Model != model || meta.Dim <= 0 || meta.Count != len(meta.Entries) {
		return nil, false, searchruntime.EmbeddingFlatIndexMetadataInvalidError()
	}
	if meta.VectorFile != "" {
		vecPath = filepath.Join(filepath.Dir(metaPath), meta.VectorFile)
	}
	vecBytes, release, err := mmapReadOnly(vecPath)
	if err != nil {
		return nil, false, err
	}
	defer release()

	stride := meta.Dim * 4
	if len(vecBytes) != meta.Count*stride {
		return nil, false, searchruntime.EmbeddingFlatIndexVectorBytesError(len(vecBytes), meta.Count*stride)
	}
	qNorm := vectorNorm(queryVec)
	scored := make([]embeddingScoredPath, 0, meta.Count)
	for i, entry := range meta.Entries {
		if !embeddingPathMatchesSource(entry.Path, source) {
			continue
		}
		start := i * stride
		c := cosineSimilarityBlob(queryVec, qNorm, vecBytes[start:start+stride])
		if depthPenalty != 0 {
			c -= float32(strings.Count(entry.Path, "/")) * depthPenalty
		}
		scored = append(scored, embeddingScoredPath{path: entry.Path, cos: c})
	}
	sort.Slice(scored, func(i, j int) bool { return scored[i].cos > scored[j].cos })
	if k > 0 && len(scored) > k {
		scored = scored[:k]
	}
	return scored, true, nil
}

func embeddingPathMatchesSource(path, source string) bool {
	normalize := func(value string) string {
		return strings.ReplaceAll(filepath.ToSlash(value), "\\", "/")
	}
	return source == "" || strings.HasPrefix(normalize(path), normalize(source)+"/")
}

func filterEmbeddingVectorsBySource(vecs map[string][]float32, source string) map[string][]float32 {
	if source == "" {
		return vecs
	}
	filtered := make(map[string][]float32)
	for path, vec := range vecs {
		if embeddingPathMatchesSource(path, source) {
			filtered[path] = vec
		}
	}
	return filtered
}

func mmapReadOnly(path string) ([]byte, func(), error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, nil, err
	}
	if info.Size() == 0 {
		return nil, nil, searchruntime.EmbeddingFlatIndexVectorFileEmptyError(path)
	}
	data, err := syscall.Mmap(int(f.Fd()), 0, int(info.Size()), syscall.PROT_READ, syscall.MAP_SHARED)
	if err != nil {
		return nil, nil, err
	}
	return data, func() { _ = syscall.Munmap(data) }, nil
}

func vectorNorm(v []float32) float64 {
	var n float64
	for _, f := range v {
		n += float64(f) * float64(f)
	}
	return math.Sqrt(n)
}

func cosineSimilarityBlob(query []float32, queryNorm float64, blob []byte) float32 {
	if len(query) == 0 || len(blob) != len(query)*4 || queryNorm == 0 {
		return 0
	}
	var dot, nb float64
	for i, q := range query {
		f := math.Float32frombits(binary.LittleEndian.Uint32(blob[i*4:]))
		fb := float64(f)
		dot += float64(q) * fb
		nb += fb * fb
	}
	if nb == 0 {
		return 0
	}
	return float32(dot / (queryNorm * math.Sqrt(nb)))
}

type embedOpts struct {
	out       string
	source    string
	model     string
	reembed   bool
	migrate   bool
	flatOnly  bool
	maxCost   float64
	dryRun    bool
	chunkSize int // 0 = whole-doc; > 0 = split each doc into <chunkSize>-char chunks
	apiKey    string
	httpDo    func(req *http.Request) (*http.Response, error)
	// callSite tags the audit-log call_site for optional provider-usage rollup.
	// Empty falls back to searchruntime.DefaultEmbeddingUsageCallSite.
	callSite string
}

func cmdEmbed(args []string) {
	o := embedOpts{
		model:   searchruntime.DefaultEmbeddingModel,
		maxCost: 5.0,
		out:     defaultOpts().out,
	}
	fs := flag.NewFlagSet("embed", flag.ExitOnError)
	fs.StringVar(&o.out, "out", o.out, "output root dir")
	fs.StringVar(&o.source, "source", "", "embed only this source (default: all)")
	fs.StringVar(&o.model, "model", o.model, "OpenAI embedding model")
	fs.BoolVar(&o.reembed, "reembed", false, "ignore cache; re-embed every matched doc")
	fs.BoolVar(&o.migrate, "migrate-legacy", false, "copy legacy embedding tables from search.db into embeddings.db without calling the API")
	fs.BoolVar(&o.flatOnly, "write-flat-only", false, "rewrite the flat vector sidecar from cached embeddings only; no API calls")
	fs.Float64Var(&o.maxCost, "max-cost", o.maxCost, "abort if estimated USD cost exceeds this")
	fs.BoolVar(&o.dryRun, "dry-run", false, "print plan + cost estimate, do not call API")
	fs.IntVar(&o.chunkSize, "chunk-size", 0, "split each doc into chunks of this many chars before embedding (0 = whole-doc, default). Recommended: 1500 (~375 tokens) for retrieval rerank.")
	fs.Parse(args)

	if o.migrate {
		if err := migrateLegacyEmbeddings(o.out); err != nil {
			die(err)
		}
		return
	}
	if o.flatOnly {
		if err := runEmbedWriteFlatOnly(o); err != nil {
			die(err)
		}
		return
	}

	o.apiKey = searchruntime.ResolveEmbeddingAPIKey() // env, then optional secrets CLI
	if o.apiKey == "" && !o.dryRun {
		die(searchruntime.ProviderAPIKeyNotSetError(searchruntime.DefaultEmbeddingAPIKeyEnv))
	}

	if err := runEmbed(o); err != nil {
		die(err)
	}
}

func runEmbedWriteFlatOnly(o embedOpts) error {
	if o.source != "" {
		return searchruntime.EmbeddingWriteFlatOnlySourceError()
	}
	if o.chunkSize > 0 {
		return searchruntime.EmbeddingWriteFlatOnlyChunkSizeError()
	}
	db, err := openEmbeddingsDB(o.out, true)
	if err != nil {
		return searchruntime.EmbeddingIndexOpenError(err)
	}
	defer db.Close()

	inspect, err := embeddb.New(db).InspectEmbeddingCacheByModel(context.Background(), o.model)
	if err != nil {
		return searchruntime.EmbeddingWriteFlatOnlyInspectCacheError(err)
	}
	count, dim := int(inspect.Count), int(inspect.Dim)
	if count == 0 {
		return searchruntime.EmbeddingWriteFlatOnlyMissingCacheError(o.model)
	}
	if err := writeFlatEmbeddingIndex(db, o.out, o.model); err != nil {
		return searchruntime.EmbeddingFlatIndexWriteError(err)
	}
	fmt.Print(searchruntime.EmbeddingFlatIndexWrittenMessage(o.model, count, dim))
	return nil
}

func runEmbed(o embedOpts) error {
	if !searchruntime.IsKnownEmbeddingModel(o.model) {
		return searchruntime.EmbeddingUnknownModelError(o.model)
	}
	if o.chunkSize > 0 {
		return runEmbedChunked(o)
	}

	docs, err := collectDocsForEmbedding(o.out, o.source)
	if err != nil {
		return err
	}
	if len(docs) == 0 {
		fmt.Fprint(os.Stderr, searchruntime.EmbeddingNoDocsFoundMessage())
		return nil
	}

	db, err := openEmbeddingsDB(o.out, false)
	if err != nil {
		return searchruntime.EmbeddingIndexOpenError(err)
	}
	defer db.Close()

	cache, err := loadEmbedCache(db, o.model)
	if err != nil {
		return searchruntime.EmbeddingCacheLoadError(err)
	}

	var pending []docToEmbed
	skipped := 0
	for _, d := range docs {
		if !o.reembed {
			if cached, ok := cache[d.path]; ok && cached == d.mtimeNs {
				skipped++
				continue
			}
		}
		pending = append(pending, d)
	}
	if len(pending) == 0 {
		if !o.dryRun {
			pruned, err := pruneStaleEmbeddings(db, o.model, o.source, docs)
			if err != nil {
				return searchruntime.EmbeddingStoreBatchError(err)
			}
			if pruned > 0 {
				fmt.Fprintf(os.Stderr, "embeddings: pruned %d stale path(s) for model=%s\n", pruned, o.model)
			}
			if err := writeFlatEmbeddingIndex(db, o.out, o.model); err != nil {
				fmt.Fprint(os.Stderr, searchruntime.EmbeddingFlatIndexWriteWarning(err))
			}
		}
		fmt.Print(searchruntime.EmbeddingUpToDateMessage(skipped, o.model))
		return nil
	}

	totalChars := 0
	for _, d := range pending {
		totalChars += searchruntime.ClampEmbeddingInputChars(len(d.embedText()))
	}
	estTokens := searchruntime.EstimateEmbeddingTokensFromChars(totalChars)
	estCost := searchruntime.EstimateEmbeddingUsageUSD(o.model, estTokens)

	fmt.Print(searchruntime.EmbeddingPlanMessage(o.model, len(pending), skipped, estTokens, estCost))
	if estCost > o.maxCost {
		return searchruntime.EmbeddingMaxCostExceededError(estCost, o.maxCost)
	}
	if o.dryRun {
		return nil
	}

	start := time.Now()
	stored, skippedOversize := 0, 0
	for batch := range chunkBatches(pending) {
		kept, vecs, dropped, err := embedBatchWithFallback(o, batch)
		if err != nil {
			return searchruntime.EmbeddingBatchError(err)
		}
		if err := storeEmbeddings(db, kept, vecs, o.model); err != nil {
			return searchruntime.EmbeddingStoreBatchError(err)
		}
		stored += len(kept)
		skippedOversize += dropped
		fmt.Fprint(os.Stderr, searchruntime.EmbeddingProgressMessage(stored, len(pending)))
	}
	if msg := searchruntime.EmbeddingOversizeDocsWarning(skippedOversize); msg != "" {
		fmt.Fprint(os.Stderr, msg)
	}
	pruned, err := pruneStaleEmbeddings(db, o.model, o.source, docs)
	if err != nil {
		return searchruntime.EmbeddingStoreBatchError(err)
	}
	if pruned > 0 {
		fmt.Fprintf(os.Stderr, "embeddings: pruned %d stale path(s) for model=%s\n", pruned, o.model)
	}
	if err := writeFlatEmbeddingIndex(db, o.out, o.model); err != nil {
		fmt.Fprint(os.Stderr, searchruntime.EmbeddingFlatIndexWriteWarning(err))
	}

	fmt.Print(searchruntime.EmbeddingSummaryMessage(stored, time.Since(start).Round(time.Millisecond), o.model))
	return nil
}

type docToEmbed struct {
	path    string // e.g. "supabase/foo/bar.md"
	mtimeNs int64
	title   string // extracted title (or "" if none); kept separate from
	// body so the chunked path can reliably re-prepend it on each chunk.
	body string
}

// embedText returns the input string for the embedding API: title +
// blank line + body when a title is present, otherwise body alone.
// Centralized so the whole-doc and chunked paths agree on the format.
func (d docToEmbed) embedText() string {
	return searchruntime.BuildEmbeddingInput(d.title, d.body)
}

// collectDocsForEmbedding walks <out>/<source>/**/*.md and returns one
// docToEmbed per file: title (extracted via extractTitle) and body kept
// separate so the chunked path can reliably re-prepend title to each
// chunk. Use docToEmbed.embedText() to get the API input string.
// When source is empty, walks every source dir. Order: deterministic.
func collectDocsForEmbedding(out, source string) ([]docToEmbed, error) {
	srcs, err := listSources(out)
	if err != nil {
		return nil, err
	}
	if source != "" {
		found := false
		for _, s := range srcs {
			if s == source {
				found = true
				srcs = []string{source}
				break
			}
		}
		if !found {
			return nil, searchruntime.EmbeddingSourceNotFoundError(source, out)
		}
	}

	var docs []docToEmbed
	for _, src := range srcs {
		srcDir := filepath.Join(out, src)
		err := filepath.WalkDir(srcDir, func(p string, d fs.DirEntry, werr error) error {
			if werr != nil {
				return werr
			}
			if d.IsDir() {
				return nil
			}
			name := d.Name()
			if !strings.HasSuffix(name, ".md") || name == "_INDEX.md" {
				return nil
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			data, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(out, p)
			if err != nil {
				return err
			}
			docs = append(docs, docToEmbed{
				path:    filepath.ToSlash(rel),
				mtimeNs: info.ModTime().UnixNano(),
				title:   extractTitle(p),
				body:    string(data),
			})
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	sort.Slice(docs, func(i, j int) bool { return docs[i].path < docs[j].path })
	return docs, nil
}

// pruneStaleEmbeddings removes whole-document vectors whose paths no longer
// exist in the live corpus. A source-scoped embed only prunes that exact source;
// an all-source embed reconciles the complete selected model. Other models are
// never touched. This keeps the model-wide flat sidecar from carrying deleted
// or renamed documents indefinitely.
func pruneStaleEmbeddings(db *sql.DB, model, source string, docs []docToEmbed) (int, error) {
	live := make(map[string]struct{}, len(docs))
	for _, doc := range docs {
		live[doc.path] = struct{}{}
	}

	rows, err := db.Query(`SELECT path FROM embeddings WHERE model = ?`, model)
	if err != nil {
		return 0, fmt.Errorf("list cached embeddings for stale-path pruning: %w", err)
	}
	var stale []string
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan cached embedding path: %w", err)
		}
		if !embeddingPathMatchesSource(path, source) {
			continue
		}
		if _, ok := live[path]; !ok {
			stale = append(stale, path)
		}
	}
	if err := rows.Close(); err != nil {
		return 0, fmt.Errorf("close cached embedding path rows: %w", err)
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("iterate cached embedding paths: %w", err)
	}
	if len(stale) == 0 {
		return 0, nil
	}

	tx, err := db.Begin()
	if err != nil {
		return 0, fmt.Errorf("begin stale embedding prune: %w", err)
	}
	defer tx.Rollback()
	stmt, err := tx.Prepare(`DELETE FROM embeddings WHERE path = ? AND model = ?`)
	if err != nil {
		return 0, fmt.Errorf("prepare stale embedding prune: %w", err)
	}
	defer stmt.Close()
	for _, path := range stale {
		if _, err := stmt.Exec(path, model); err != nil {
			return 0, fmt.Errorf("prune stale embedding %q: %w", path, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit stale embedding prune: %w", err)
	}
	return len(stale), nil
}

func loadEmbedCache(db *sql.DB, model string) (map[string]int64, error) {
	rows, err := embeddb.New(db).ListEmbeddingMtimesByModel(context.Background(), model)
	if err != nil {
		return nil, err
	}
	out := map[string]int64{}
	for _, row := range rows {
		out[row.Path] = row.MtimeNs
	}
	return out, nil
}

func storeEmbeddings(db *sql.DB, docs []docToEmbed, vecs [][]float32, model string) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	q := embeddb.New(tx)
	ctx := context.Background()
	for i, d := range docs {
		blob := serializeVec(vecs[i])
		if err := q.UpsertEmbedding(ctx, embeddb.UpsertEmbeddingParams{
			Path:    d.path,
			MtimeNs: d.mtimeNs,
			Model:   model,
			Dim:     int64(len(vecs[i])),
			Vec:     blob,
		}); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// chunkBatches packs docs into batches that fit OpenAI's per-request limits:
// at most the runtime input-count cap AND runtime batch-token cap
// estimated tokens. Token estimate uses the runtime batch-packing heuristic,
// which is intentionally more conservative than dry-run cost preflight.
//
// Greedy first-fit. Doesn't try to balance batch sizes — packs each batch
// until the next doc would push it over a limit, then starts fresh.
func chunkBatches(docs []docToEmbed) <-chan []docToEmbed {
	return searchruntime.ChunkEmbeddingBatchItems(docs, func(d docToEmbed) int {
		return len(d.embedText())
	})
}

// embedBatchWithFallback embeds a batch; if a single doc trips the per-input
// cap, it's dropped and the remainder retried. Returns the docs that were
// embedded, the resulting vectors, and how many were skipped as oversize.
// Other errors propagate.
func embedBatchWithFallback(o embedOpts, batch []docToEmbed) ([]docToEmbed, [][]float32, int, error) {
	return searchruntime.RunOpenAIEmbeddingBatchWithFallback(searchruntime.OpenAIEmbeddingBatchWithFallbackInput[docToEmbed]{
		OpenAI: searchruntime.OpenAIEmbeddingBatchCallInput{
			Model:     o.model,
			APIKey:    o.apiKey,
			UserAgent: userAgent,
			Do:        o.httpDo,
			CallSite:  o.callSite,
			Record:    recordAIUsage,
		},
		Batch: batch,
		Text: func(d docToEmbed) string {
			return d.embedText()
		},
		OnDrop: searchruntime.NewEmbeddingOversizeInputDropCall(os.Stderr, func(d docToEmbed) string {
			return d.path
		}, func(d docToEmbed) string {
			return d.embedText()
		}),
	})
}

// serializeVec packs a float32 slice into a little-endian byte blob.
func serializeVec(v []float32) []byte {
	buf := make([]byte, 4*len(v))
	for i, f := range v {
		binary.LittleEndian.PutUint32(buf[i*4:], math.Float32bits(f))
	}
	return buf
}

// deserializeVec unpacks a byte blob produced by serializeVec.
func deserializeVec(b []byte) []float32 {
	if len(b)%4 != 0 {
		return nil
	}
	out := make([]float32, len(b)/4)
	for i := range out {
		bits := binary.LittleEndian.Uint32(b[i*4:])
		out[i] = math.Float32frombits(bits)
	}
	return out
}

// rerankCandidates re-orders hits by Reciprocal Rank Fusion of BM25 rank
// (already encoded as input order) and cosine similarity rank. Final score
// uses runtime-owned weighted RRF policy. Ties broken by BM25 rank since
// that's the more identifier-aware signal in our fixture.
//
// Pure cosine rerank was tried first and regressed Hit@1 by 31pp on the
// 137-query fixture: identifier-style queries ("react useState",
// "stripe connect") are perfectly served by BM25's title/path/source
// boosts and pure cosine demotes the canonical doc in favor of
// thematically-similar pages. RRF fuses both signals so the cosine signal
// can lift semantically-relevant docs without crushing BM25's identifier
// wins.
//
// Hits without stored vectors keep BM25 rank only (cosine_rank = +∞,
// 1/(k+∞) = 0). They naturally sort behind embedded peers but ahead of
// any embedded peer with a much worse BM25 rank — same graceful-degrade
// shape as before.
func rerankCandidates(db *sql.DB, hits []searchHit, queryVec []float32) ([]searchHit, error) {
	if len(hits) == 0 || len(queryVec) == 0 {
		return hits, nil
	}
	placeholders := make([]string, len(hits))
	args := make([]interface{}, len(hits))
	for i, h := range hits {
		placeholders[i] = "?"
		args[i] = h.Path
	}
	q := "SELECT path, vec FROM embeddings WHERE path IN (" + strings.Join(placeholders, ",") + ")"
	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	vecs := map[string][]float32{}
	for rows.Next() {
		var p string
		var blob []byte
		if err := rows.Scan(&p, &blob); err != nil {
			return nil, err
		}
		vecs[p] = deserializeVec(blob)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	type scored struct {
		hit       searchHit
		cos       float32
		hasVector bool
		bm25Rank  int
		cosRank   int // 1-indexed, 0 = no vector
		rrfScore  float64
	}
	scoredHits := make([]scored, len(hits))
	for i, h := range hits {
		s := scored{hit: h, bm25Rank: i + 1}
		if v, ok := vecs[h.Path]; ok {
			s.cos = searchruntime.CosineSimilarity(queryVec, v)
			s.hasVector = true
		}
		scoredHits[i] = s
	}

	// Assign cosine rank (1-indexed) only to hits with vectors.
	withVec := make([]int, 0, len(scoredHits))
	for i := range scoredHits {
		if scoredHits[i].hasVector {
			withVec = append(withVec, i)
		}
	}
	sort.SliceStable(withVec, func(i, j int) bool {
		return scoredHits[withVec[i]].cos > scoredHits[withVec[j]].cos
	})
	for r, i := range withVec {
		scoredHits[i].cosRank = r + 1
	}

	for i := range scoredHits {
		s := &scoredHits[i]
		s.rrfScore = searchruntime.HybridRankFusionScore(s.bm25Rank, s.cosRank)
	}
	ordered := make([]searchruntime.RerankOrderCandidate, len(scoredHits))
	for i, s := range scoredHits {
		ordered[i] = searchruntime.RerankOrderCandidate{
			Hit:      s.hit,
			Score:    s.rrfScore,
			BaseRank: s.bm25Rank,
		}
	}
	return searchruntime.OrderRerankCandidates(ordered), nil
}

// chunkedDoc carries enough context to embed and store one chunk.
type chunkedDoc struct {
	path     string // e.g. "supabase/foo/bar.md"
	chunkIdx int
	mtimeNs  int64
	text     string
}

func runEmbedChunked(o embedOpts) error {
	docs, err := collectDocsForEmbedding(o.out, o.source)
	if err != nil {
		return err
	}
	if len(docs) == 0 {
		fmt.Fprint(os.Stderr, searchruntime.EmbeddingNoDocsFoundMessage())
		return nil
	}

	db, err := openEmbeddingsDB(o.out, false)
	if err != nil {
		return searchruntime.EmbeddingIndexOpenError(err)
	}
	defer db.Close()

	cache, err := loadChunkedCache(db, o.model, o.chunkSize)
	if err != nil {
		return searchruntime.EmbeddingCacheLoadError(err)
	}

	// Expand docs into chunks. Each chunk gets the title prepended so it
	// has standalone context — important for max-pool cosine, where any
	// single chunk's similarity may decide the doc's score.
	var pending []chunkedDoc
	skipped := 0
	for _, d := range docs {
		chunks := searchruntime.SplitEmbeddingTextChunks(d.body, o.chunkSize)
		fileMtime := d.mtimeNs
		// Cache check: skip the entire doc only if every chunk_idx is
		// already cached at this mtime.
		allCached := !o.reembed
		if allCached {
			for i := range chunks {
				key := chunkCacheKey{path: d.path, idx: i}
				if cached, ok := cache[key]; !ok || cached != fileMtime {
					allCached = false
					break
				}
			}
		}
		if allCached && len(chunks) > 0 {
			skipped++
			continue
		}
		for i, c := range chunks {
			pending = append(pending, chunkedDoc{
				path: d.path, chunkIdx: i, mtimeNs: fileMtime, text: searchruntime.BuildEmbeddingInput(d.title, c),
			})
		}
	}
	if len(pending) == 0 {
		fmt.Print(searchruntime.EmbeddingChunkUpToDateMessage(skipped, o.model, o.chunkSize))
		return nil
	}

	totalChars := 0
	for _, p := range pending {
		totalChars += searchruntime.ClampEmbeddingInputChars(len(p.text))
	}
	estTokens := searchruntime.EstimateEmbeddingTokensFromChars(totalChars)
	estCost := searchruntime.EstimateEmbeddingUsageUSD(o.model, estTokens)

	fmt.Print(searchruntime.EmbeddingChunkPlanMessage(o.model, o.chunkSize, len(pending), skipped, estTokens, estCost))
	if estCost > o.maxCost {
		return searchruntime.EmbeddingMaxCostExceededError(estCost, o.maxCost)
	}
	if o.dryRun {
		return nil
	}

	start := time.Now()
	stored, skippedOversize := 0, 0
	for batch := range chunkBatchesChunked(pending) {
		kept, vecs, dropped, err := embedBatchWithFallbackChunked(o, batch)
		if err != nil {
			return searchruntime.EmbeddingBatchError(err)
		}
		if err := storeChunkedEmbeddings(db, kept, vecs, o.model, o.chunkSize); err != nil {
			return searchruntime.EmbeddingStoreBatchError(err)
		}
		stored += len(kept)
		skippedOversize += dropped
		fmt.Fprint(os.Stderr, searchruntime.EmbeddingChunkProgressMessage(stored, len(pending)))
	}
	if msg := searchruntime.EmbeddingOversizeChunksWarning(skippedOversize); msg != "" {
		fmt.Fprint(os.Stderr, msg)
	}
	fmt.Print(searchruntime.EmbeddingChunkSummaryMessage(stored, time.Since(start).Round(time.Millisecond), o.model, o.chunkSize))
	return nil
}

type chunkCacheKey struct {
	path string
	idx  int
}

func loadChunkedCache(db *sql.DB, model string, chunkSize int) (map[chunkCacheKey]int64, error) {
	rows, err := embeddb.New(db).ListEmbeddingChunkMtimesByModelAndChunkSize(context.Background(), embeddb.ListEmbeddingChunkMtimesByModelAndChunkSizeParams{
		Model:     model,
		ChunkSize: int64(chunkSize),
	})
	if err != nil {
		return nil, err
	}
	out := map[chunkCacheKey]int64{}
	for _, row := range rows {
		out[chunkCacheKey{path: row.Path, idx: int(row.ChunkIdx)}] = row.MtimeNs
	}
	return out, nil
}

func chunkBatchesChunked(docs []chunkedDoc) <-chan []chunkedDoc {
	return searchruntime.ChunkEmbeddingBatchItems(docs, func(d chunkedDoc) int {
		return len(d.text)
	})
}

func embedBatchWithFallbackChunked(o embedOpts, batch []chunkedDoc) ([]chunkedDoc, [][]float32, int, error) {
	return searchruntime.RunOpenAIEmbeddingBatchWithFallback(searchruntime.OpenAIEmbeddingBatchWithFallbackInput[chunkedDoc]{
		OpenAI: searchruntime.OpenAIEmbeddingBatchCallInput{
			Model:     o.model,
			APIKey:    o.apiKey,
			UserAgent: userAgent,
			Do:        o.httpDo,
			CallSite:  o.callSite,
			Record:    recordAIUsage,
		},
		Batch: batch,
		Text: func(d chunkedDoc) string {
			return d.text
		},
		OnDrop: searchruntime.NewEmbeddingOversizeChunkDropCall(os.Stderr, func(d chunkedDoc) string {
			return d.path
		}, func(d chunkedDoc) int {
			return d.chunkIdx
		}),
	})
}

func storeChunkedEmbeddings(db *sql.DB, chunks []chunkedDoc, vecs [][]float32, model string, chunkSize int) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	q := embeddb.New(tx)
	ctx := context.Background()
	for i, c := range chunks {
		blob := serializeVec(vecs[i])
		if err := q.UpsertEmbeddingChunk(ctx, embeddb.UpsertEmbeddingChunkParams{
			Path:      c.path,
			ChunkIdx:  int64(c.chunkIdx),
			ChunkSize: int64(chunkSize),
			MtimeNs:   c.mtimeNs,
			Model:     model,
			Dim:       int64(len(vecs[i])),
			Vec:       blob,
		}); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// hybridCache holds whole-doc embedding vectors keyed by model so the
// per-query cosine scan doesn't re-read 38k rows from SQLite every
// search. Populated lazily on first applyHybridRetrieval call. For eval
// (137-168 queries per run) the first call pays the ~500ms DB read;
// subsequent calls land an in-memory map and run in ~50ms. For
// single-shot interactive search the cache never helps; for hot-path
// hybrid the next step is mmap'd flat .vec or sqlite-vec.
var (
	hybridCacheMu sync.Mutex
	hybridCache   = map[string]map[string][]float32{} // model -> path -> vec
)

func loadHybridCache(db *sql.DB, model string) (map[string][]float32, error) {
	hybridCacheMu.Lock()
	defer hybridCacheMu.Unlock()
	if vecs, ok := hybridCache[model]; ok {
		return vecs, nil
	}
	rows, err := embeddb.New(db).ListEmbeddingVectorsByModel(context.Background(), model)
	if err != nil {
		return nil, err
	}
	vecs := make(map[string][]float32, 40000)
	for _, row := range rows {
		vecs[row.Path] = deserializeVec(row.Vec)
	}
	hybridCache[model] = vecs
	return vecs, nil
}

// applyHybridRetrieval expands the BM25 candidate pool with whole-doc
// embedding cosine top-K before any rerank step runs. Designed for
// natural-language queries where BM25's identifier-token bias misses
// the canonical doc from its top-50 entirely — the embedding signal
// surfaces semantically-relevant docs BM25 couldn't grip, then a
// downstream reranker (LLM cross-encoder, ideally) picks the best of
// the union.
//
// Embeds the query once via OpenAI, scans every whole-doc vector in
// `embeddings`, takes top rerankK by cosine, and unions with bm25Hits
// (dedup by path; BM25 hits keep their order at the front).
//
// Cost per call: 1 OpenAI embed (~$0.000003 + ~200ms) + ~500ms cosine
// scan over 38k whole-doc vectors. Acceptable for eval; for interactive
// search consider sqlite-vec or mmap'd flat file later.
func applyHybridRetrieval(query string, bm25Hits []searchHit, o searchOpts) ([]searchHit, error) {
	// Apply a small depth penalty so version-pinned mirrors of the same
	// doc don't crowd out the canonical (regression observed 2026-05-02
	// on `react native flatlist performance`: 6 copies of the same file
	// at `docs/0.77/`, `docs/0.78/`, etc., all with near-identical
	// cosines, drowned the depth-1 canonical). Mirrors the spirit of
	// `searchDepthPenaltyPerSegment` in BM25 — slight preference for
	// shallower paths when cosines are otherwise tied.
	apiKey, err := searchruntime.ResolveProviderAPIKey(searchruntime.ProviderAPIKeyInput{
		KeyEnv: searchruntime.DefaultEmbeddingAPIKeyEnv,
		Getenv: os.Getenv,
	})
	if err != nil {
		return bm25Hits, err
	}
	return searchruntime.RunHybridRetrieval(searchruntime.HybridRetrievalRunInput[*sql.DB]{
		Query:                  query,
		BM25Hits:               bm25Hits,
		OutDir:                 o.out,
		Model:                  o.rerankModel,
		DefaultModel:           searchruntime.DefaultEmbeddingModel,
		APIKey:                 apiKey,
		APIKeyEnv:              searchruntime.DefaultEmbeddingAPIKeyEnv,
		K:                      o.rerankK,
		DefaultK:               searchruntime.DefaultRerankK,
		DepthPenaltyPerSegment: searchruntime.DefaultHybridDepthPenaltyPerSegment,
		UseHyDE:                o.rerankHyde,
		GenerateHyDE: func(query string) (string, bool) {
			return generateHyDEDoc(query, o)
		},
		EmbedQuery: searchruntime.NewOpenAIEmbeddingQueryCall(searchruntime.OpenAIEmbeddingQueryCallInput{
			UserAgent: userAgent,
			CallSite:  searchruntime.DefaultHybridRetrievalCallSite,
			Record:    recordAIUsage,
		}),
		FlatTopK: searchruntime.NewHybridFlatTopKCall(func(outDir, model string, queryVec []float32, k int, depthPenalty float32) ([]embeddingScoredPath, bool, error) {
			return topFlatEmbeddingPathsForSource(outDir, model, queryVec, k, depthPenalty, o.source)
		}),
		WarnFlatFallback: searchruntime.NewHybridFlatFallbackWarningCall(os.Stderr),
		OpenIndex:        searchruntime.NewReadOnlyEmbeddingIndexOpenCall(openEmbeddingsDB),
		LoadVectors: searchruntime.NewHybridVectorLoadCall(func(db *sql.DB, model string) (map[string][]float32, error) {
			vecs, err := loadHybridCache(db, model)
			if err != nil {
				return nil, err
			}
			return filterEmbeddingVectorsBySource(vecs, o.source), nil
		}),
		ScoreVector: searchruntime.CosineSimilarity,
		Title:       searchruntime.NewHybridTitleLoader(o.out, extractTitle),
	})
}

// rerankCandidatesChunked re-orders hits by max-pool cosine across each
// doc's chunks. For each candidate path, fetches every chunk vector at
// the requested chunk_size and takes the highest cosine against queryVec.
// Same runtime-owned weighted RRF fusion as the whole-doc path, just with a
// stronger cosine signal because the relevant section's embedding isn't
// drowned out by unrelated themes from the rest of the doc.
func rerankCandidatesChunked(db *sql.DB, hits []searchHit, queryVec []float32, model string, chunkSize int) ([]searchHit, error) {
	if len(hits) == 0 || len(queryVec) == 0 {
		return hits, nil
	}
	placeholders := make([]string, len(hits))
	args := make([]interface{}, 0, len(hits)+2)
	for i, h := range hits {
		placeholders[i] = "?"
		args = append(args, h.Path)
	}
	args = append(args, model, chunkSize)
	q := "SELECT path, vec FROM embedding_chunks WHERE path IN (" +
		strings.Join(placeholders, ",") + ") AND model = ? AND chunk_size = ?"
	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	maxCos := map[string]float32{}
	hasVec := map[string]bool{}
	for rows.Next() {
		var p string
		var blob []byte
		if err := rows.Scan(&p, &blob); err != nil {
			return nil, err
		}
		v := deserializeVec(blob)
		c := searchruntime.CosineSimilarity(queryVec, v)
		if !hasVec[p] || c > maxCos[p] {
			maxCos[p] = c
			hasVec[p] = true
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	type scored struct {
		hit       searchHit
		cos       float32
		hasVector bool
		bm25Rank  int
		cosRank   int
		rrfScore  float64
	}
	scoredHits := make([]scored, len(hits))
	for i, h := range hits {
		s := scored{hit: h, bm25Rank: i + 1}
		if c, ok := maxCos[h.Path]; ok {
			s.cos = c
			s.hasVector = true
		}
		scoredHits[i] = s
	}
	withVec := make([]int, 0, len(scoredHits))
	for i := range scoredHits {
		if scoredHits[i].hasVector {
			withVec = append(withVec, i)
		}
	}
	sort.SliceStable(withVec, func(i, j int) bool {
		return scoredHits[withVec[i]].cos > scoredHits[withVec[j]].cos
	})
	for r, i := range withVec {
		scoredHits[i].cosRank = r + 1
	}
	for i := range scoredHits {
		s := &scoredHits[i]
		s.rrfScore = searchruntime.HybridRankFusionScore(s.bm25Rank, s.cosRank)
	}
	ordered := make([]searchruntime.RerankOrderCandidate, len(scoredHits))
	for i, s := range scoredHits {
		ordered[i] = searchruntime.RerankOrderCandidate{
			Hit:      s.hit,
			Score:    s.rrfScore,
			BaseRank: s.bm25Rank,
		}
	}
	return searchruntime.OrderRerankCandidates(ordered), nil
}
