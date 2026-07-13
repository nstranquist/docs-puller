package main

import (
	"database/sql"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nstranquist/docs-puller/searchruntime"
)

type openAIEmbeddingRequestForTest struct {
	Input []string `json:"input"`
	Model string   `json:"model"`
}

func TestSerializeVecRoundtrip(t *testing.T) {
	cases := [][]float32{
		{},
		{0},
		{1, -1, 0.5, -0.5},
		{0.123456789, math.Pi, math.E, 1e-10, 1e10},
	}
	for _, c := range cases {
		blob := serializeVec(c)
		got := deserializeVec(blob)
		if len(got) != len(c) {
			t.Errorf("serializeVec(%v): roundtrip len = %d, want %d", c, len(got), len(c))
			continue
		}
		for i := range c {
			if got[i] != c[i] {
				t.Errorf("serializeVec(%v): [%d] = %v, want %v", c, i, got[i], c[i])
			}
		}
	}
}

func TestCosineSimilarity(t *testing.T) {
	cases := []struct {
		a, b []float32
		want float32
	}{
		{[]float32{1, 0}, []float32{1, 0}, 1.0},
		{[]float32{1, 0}, []float32{0, 1}, 0.0},
		{[]float32{1, 0}, []float32{-1, 0}, -1.0},
		{[]float32{1, 1}, []float32{1, 1}, 1.0},
		{[]float32{}, []float32{1}, 0},        // length mismatch
		{[]float32{0, 0}, []float32{1, 1}, 0}, // zero vector
	}
	for _, c := range cases {
		got := searchruntime.CosineSimilarity(c.a, c.b)
		if math.Abs(float64(got-c.want)) > 1e-5 {
			t.Errorf("CosineSimilarity(%v, %v) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestEmbedBatchWithFallbackRequestShape(t *testing.T) {
	var captured openAIEmbeddingRequestForTest
	o := embedOpts{
		model:  "text-embedding-3-small",
		apiKey: "test-key",
		httpDo: func(req *http.Request) (*http.Response, error) {
			if got := req.Method; got != http.MethodPost {
				t.Fatalf("method = %q, want POST", got)
			}
			if got := req.Header.Get("Authorization"); got != "Bearer test-key" {
				t.Fatalf("Authorization = %q, want bearer key", got)
			}
			body, err := io.ReadAll(req.Body)
			if err != nil {
				return nil, err
			}
			if err := json.Unmarshal(body, &captured); err != nil {
				return nil, err
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(`{"data":[{"embedding":[0.1,0.2],"index":0}]}`)),
			}, nil
		},
	}
	kept, vecs, dropped, err := embedBatchWithFallback(o, []docToEmbed{{path: "src/hello.md", body: "hello"}})
	if err != nil {
		t.Fatal(err)
	}
	if dropped != 0 || len(kept) != 1 || kept[0].path != "src/hello.md" {
		t.Fatalf("kept=%+v dropped=%d, want one kept doc and no drops", kept, dropped)
	}
	if captured.Model != "text-embedding-3-small" {
		t.Errorf("model = %q, want text-embedding-3-small", captured.Model)
	}
	if len(captured.Input) != 1 || captured.Input[0] != "hello" {
		t.Errorf("input = %v, want [hello]", captured.Input)
	}
	if len(vecs) != 1 || len(vecs[0]) != 2 {
		t.Errorf("vecs = %v, want 1 vec of dim 2", vecs)
	}
}

func TestOpenEmbeddingsDBUsesSeparateStore(t *testing.T) {
	out := t.TempDir()
	db, err := openEmbeddingsDB(out, false)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	if _, err := os.Stat(embeddingsDBPath(out)); err != nil {
		t.Fatalf("embeddings db missing at %s: %v", embeddingsDBPath(out), err)
	}
	if _, err := os.Stat(ftsDBPath(out)); !os.IsNotExist(err) {
		t.Fatalf("openEmbeddingsDB should not create FTS db; stat err=%v", err)
	}
}

func TestEmbeddingsReadDBPathFallsBackToLegacySearchDB(t *testing.T) {
	out := t.TempDir()
	cacheDir := filepath.Join(out, ".cache")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", ftsDBPath(out))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(embedSchema); err != nil {
		t.Fatal(err)
	}
	db.Close()

	if got := embeddingsReadDBPath(out); got != ftsDBPath(out) {
		t.Fatalf("embeddingsReadDBPath = %q, want legacy %q", got, ftsDBPath(out))
	}
}

func TestMigrateLegacyEmbeddingsCopiesToSplitStore(t *testing.T) {
	out := t.TempDir()
	cacheDir := filepath.Join(out, ".cache")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		t.Fatal(err)
	}
	legacy, err := sql.Open("sqlite", ftsDBPath(out))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.Exec(embedSchema); err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.Exec(
		"INSERT INTO embeddings (path, mtime_ns, model, dim, vec) VALUES (?, ?, ?, ?, ?)",
		"src/a.md", 0, "test-model", 2, serializeVec([]float32{1, 0}),
	); err != nil {
		t.Fatal(err)
	}
	legacy.Close()

	if err := migrateLegacyEmbeddings(out); err != nil {
		t.Fatal(err)
	}
	if embeddingsReadDBPath(out) != embeddingsDBPath(out) {
		t.Fatalf("read path = %q, want split store", embeddingsReadDBPath(out))
	}
	db, err := openEmbeddingsDB(out, true)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM embeddings").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("migrated rows = %d, want 1", n)
	}
	metaPath, _ := flatEmbeddingPaths(out, "test-model")
	if _, err := os.Stat(metaPath); err != nil {
		t.Fatalf("flat metadata missing after migration: %v", err)
	}
}

// TestStoreEmbeddingsCoexistsAcrossModels regression-guards the
// 2026-05-04 incident where embed --model X silently overwrote rows for
// model Y because the embeddings table had `path TEXT PRIMARY KEY`. With
// the composite (path, model) PRIMARY KEY, embedding the same path under
// two different models must produce two distinct rows.
func TestStoreEmbeddingsCoexistsAcrossModels(t *testing.T) {
	out := t.TempDir()
	db, err := openEmbeddingsDB(out, false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	docs := []docToEmbed{{path: "src/a.md", mtimeNs: 1}}
	if err := storeEmbeddings(db, docs, [][]float32{{1, 0}}, "model-A"); err != nil {
		t.Fatalf("store model-A: %v", err)
	}
	if err := storeEmbeddings(db, docs, [][]float32{{0, 1}}, "model-B"); err != nil {
		t.Fatalf("store model-B: %v", err)
	}
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM embeddings WHERE path = ?", "src/a.md").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("rows for path = %d, want 2 (one per model)", n)
	}
	// Re-storing model-A must update its row, not the model-B row.
	if err := storeEmbeddings(db, []docToEmbed{{path: "src/a.md", mtimeNs: 99}},
		[][]float32{{2, 0}}, "model-A"); err != nil {
		t.Fatalf("re-store model-A: %v", err)
	}
	rows, err := db.Query("SELECT model, mtime_ns FROM embeddings WHERE path = ? ORDER BY model", "src/a.md")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	got := map[string]int64{}
	for rows.Next() {
		var m string
		var ts int64
		if err := rows.Scan(&m, &ts); err != nil {
			t.Fatal(err)
		}
		got[m] = ts
	}
	if got["model-A"] != 99 {
		t.Errorf("model-A mtime_ns = %d, want 99 (upsert should have updated)", got["model-A"])
	}
	if got["model-B"] != 1 {
		t.Errorf("model-B mtime_ns = %d, want 1 (must NOT have been touched by model-A upsert)", got["model-B"])
	}
}

// TestMigrateEmbeddingsPKMigratesOldSchema covers the auto-migration:
// an existing DB on the OLD schema (path TEXT PRIMARY KEY) must be
// rewritten to the composite-PK shape on first openEmbeddingsDB, so a
// second model's row can be inserted for the same path without violating
// UNIQUE.
func TestMigrateEmbeddingsPKMigratesOldSchema(t *testing.T) {
	out := t.TempDir()
	cacheDir := filepath.Join(out, ".cache")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Create a DB with the OLD schema directly (bypassing openEmbeddingsDB,
	// which would auto-migrate on the way in).
	dbPath := embeddingsDBPath(out)
	raw, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	const oldSchema = `
CREATE TABLE embeddings (
    path     TEXT PRIMARY KEY,
    mtime_ns INTEGER NOT NULL,
    model    TEXT NOT NULL,
    dim      INTEGER NOT NULL,
    vec      BLOB NOT NULL
);`
	if _, err := raw.Exec(oldSchema); err != nil {
		raw.Close()
		t.Fatalf("create old schema: %v", err)
	}
	if _, err := raw.Exec(
		"INSERT INTO embeddings (path, mtime_ns, model, dim, vec) VALUES (?, ?, ?, ?, ?)",
		"src/a.md", 1, "model-A", 2, serializeVec([]float32{1, 0}),
	); err != nil {
		raw.Close()
		t.Fatal(err)
	}
	raw.Close()

	// Now open via the production path — must trigger auto-migration.
	db, err := openEmbeddingsDB(out, false)
	if err != nil {
		t.Fatalf("openEmbeddingsDB after old schema: %v", err)
	}
	defer db.Close()
	// Sanity: the original row survived.
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM embeddings WHERE path = ?", "src/a.md").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("after migration rows for path = %d, want 1 (original)", n)
	}
	// Critical assertion: a second model for the same path must now succeed.
	// On the OLD schema this would have failed with "UNIQUE constraint failed".
	if err := storeEmbeddings(db,
		[]docToEmbed{{path: "src/a.md", mtimeNs: 2}},
		[][]float32{{0, 1}}, "model-B"); err != nil {
		t.Fatalf("store model-B after migration: %v", err)
	}
	if err := db.QueryRow("SELECT COUNT(*) FROM embeddings WHERE path = ?", "src/a.md").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("after second-model insert rows = %d, want 2", n)
	}
	// Verify the schema is now the composite-PK shape.
	var schemaSQL string
	if err := db.QueryRow(
		`SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'embeddings'`,
	).Scan(&schemaSQL); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(schemaSQL, "PRIMARY KEY (path, model)") {
		t.Errorf("schema not migrated: %s", schemaSQL)
	}
}

func TestEmbedBatchWithFallbackSplitsBatchTokenLimit(t *testing.T) {
	var callSizes []int
	o := embedOpts{
		model:  "text-embedding-3-small",
		apiKey: "test-key",
		httpDo: func(req *http.Request) (*http.Response, error) {
			body, _ := io.ReadAll(req.Body)
			var parsed openAIEmbeddingRequestForTest
			_ = json.Unmarshal(body, &parsed)
			callSizes = append(callSizes, len(parsed.Input))
			if len(parsed.Input) > 1 {
				return &http.Response{
					StatusCode: 400,
					Body: io.NopCloser(strings.NewReader(
						`{"error":{"message":"Requested 12000 tokens; max tokens per request is 8192"}}`,
					)),
				}, nil
			}
			return &http.Response{
				StatusCode: 200,
				Body: io.NopCloser(strings.NewReader(
					`{"data":[{"embedding":[1,0],"index":0}]}`,
				)),
			}, nil
		},
	}
	docs := []docToEmbed{
		{path: "src/a.md", body: "alpha"},
		{path: "src/b.md", body: "beta"},
	}
	kept, vecs, dropped, err := embedBatchWithFallback(o, docs)
	if err != nil {
		t.Fatal(err)
	}
	if dropped != 0 || len(kept) != 2 || len(vecs) != 2 {
		t.Fatalf("kept=%d vecs=%d dropped=%d, want 2/2/0", len(kept), len(vecs), dropped)
	}
	if kept[0].path != "src/a.md" || kept[1].path != "src/b.md" {
		t.Fatalf("kept order changed: %+v", kept)
	}
	if len(callSizes) != 3 || callSizes[0] != 2 || callSizes[1] != 1 || callSizes[2] != 1 {
		t.Fatalf("call sizes = %v, want [2 1 1]", callSizes)
	}
}

func TestFlatEmbeddingIndexRanksWithoutSQLiteScan(t *testing.T) {
	out := t.TempDir()
	db, err := openEmbeddingsDB(out, false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows := []struct {
		path string
		vec  []float32
	}{
		{"src/a.md", []float32{1, 0}},
		{"src/b.md", []float32{0, 1}},
	}
	for _, row := range rows {
		_, err := db.Exec(
			"INSERT INTO embeddings (path, mtime_ns, model, dim, vec) VALUES (?, ?, ?, ?, ?)",
			row.path, 0, "test-model", len(row.vec), serializeVec(row.vec))
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := writeFlatEmbeddingIndex(db, out, "test-model"); err != nil {
		t.Fatal(err)
	}
	metaPath, vecPath := flatEmbeddingPaths(out, "test-model")
	if _, err := os.Stat(metaPath); err != nil {
		t.Fatalf("flat meta missing: %v", err)
	}
	if _, err := os.Stat(vecPath); err != nil {
		t.Fatalf("flat vec missing: %v", err)
	}

	got, ok, err := topFlatEmbeddingPaths(out, "test-model", []float32{0, 1}, 2, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("flat index not used")
	}
	if len(got) != 2 || got[0].path != "src/b.md" {
		t.Fatalf("flat top paths = %+v, want src/b.md first", got)
	}
}

func TestFlatEmbeddingIndexSourceScopeFiltersBeforeTopK(t *testing.T) {
	out := t.TempDir()
	db, err := openEmbeddingsDB(out, false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, row := range []struct {
		path string
		vec  []float32
	}{
		{"wanted/a.md", []float32{0.8, 0.2}},
		{"other/b.md", []float32{1, 0}},
	} {
		if _, err := db.Exec(
			"INSERT INTO embeddings (path, mtime_ns, model, dim, vec) VALUES (?, ?, ?, ?, ?)",
			row.path, 0, "test-model", len(row.vec), serializeVec(row.vec),
		); err != nil {
			t.Fatal(err)
		}
	}
	if err := writeFlatEmbeddingIndex(db, out, "test-model"); err != nil {
		t.Fatal(err)
	}

	got, ok, err := topFlatEmbeddingPathsForSource(out, "test-model", []float32{1, 0}, 1, 0, "wanted")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("flat index not used")
	}
	if len(got) != 1 || got[0].path != "wanted/a.md" {
		t.Fatalf("source-scoped flat top paths = %+v, want wanted/a.md", got)
	}
}

func TestFilterEmbeddingVectorsBySourceUsesExactDirectory(t *testing.T) {
	vecs := map[string][]float32{
		"wanted/a.md":        {1},
		"wanted\\windows.md": {4},
		"wanted-extra/b.md":  {2},
		"other/c.md":         {3},
	}
	got := filterEmbeddingVectorsBySource(vecs, "wanted")
	if len(got) != 2 || got["wanted/a.md"] == nil || got["wanted\\windows.md"] == nil {
		t.Fatalf("filtered vectors = %+v, want only wanted source paths", got)
	}
}

func TestRunEmbedWriteFlatOnlyWritesCachedModelWithoutDocs(t *testing.T) {
	out := t.TempDir()
	db, err := openEmbeddingsDB(out, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range []struct {
		path string
		vec  []float32
	}{
		{"src/a.md", []float32{1, 0}},
		{"src/b.md", []float32{0, 1}},
	} {
		if _, err := db.Exec(
			"INSERT INTO embeddings (path, mtime_ns, model, dim, vec) VALUES (?, ?, ?, ?, ?)",
			row.path, 0, "test-model", len(row.vec), serializeVec(row.vec),
		); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()

	if err := runEmbedWriteFlatOnly(embedOpts{out: out, model: "test-model"}); err != nil {
		t.Fatal(err)
	}
	metaPath, vecPath := flatEmbeddingPaths(out, "test-model")
	data, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatalf("flat metadata missing: %v", err)
	}
	var meta flatEmbeddingMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		t.Fatal(err)
	}
	if meta.Model != "test-model" || meta.Count != 2 || meta.Dim != 2 {
		t.Fatalf("flat metadata = %+v, want model/count/dim test-model/2/2", meta)
	}
	if info, err := os.Stat(vecPath); err != nil || info.Size() != 16 {
		t.Fatalf("flat vec stat=%+v err=%v, want 16 bytes", info, err)
	}
}

func TestRunEmbedWriteFlatOnlyRequiresCachedModel(t *testing.T) {
	out := t.TempDir()
	db, err := openEmbeddingsDB(out, false)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	err = runEmbedWriteFlatOnly(embedOpts{out: out, model: "missing-model"})
	if err == nil || !strings.Contains(err.Error(), "no cached embeddings found for model=missing-model") {
		t.Fatalf("err = %v, want missing cached model error", err)
	}
}

func TestRunEmbedWriteFlatOnlyRejectsSourceScope(t *testing.T) {
	err := runEmbedWriteFlatOnly(embedOpts{out: t.TempDir(), model: "test-model", source: "supabase"})
	if err == nil || !strings.Contains(err.Error(), "--write-flat-only writes a model-wide sidecar") {
		t.Fatalf("err = %v, want source-scope rejection", err)
	}
}

func TestPruneStaleEmbeddingsRespectsSourceAndModel(t *testing.T) {
	out := t.TempDir()
	db, err := openEmbeddingsDB(out, false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	for _, row := range []struct {
		path  string
		model string
	}{
		{"alpha/live.md", "text-embedding-3-small"},
		{"alpha/stale.md", "text-embedding-3-small"},
		{"beta/stale.md", "text-embedding-3-small"},
		{"alpha/stale.md", "text-embedding-3-large"},
	} {
		if _, err := db.Exec(
			"INSERT INTO embeddings (path, mtime_ns, model, dim, vec) VALUES (?, ?, ?, ?, ?)",
			row.path, 0, row.model, 1, serializeVec([]float32{1}),
		); err != nil {
			t.Fatal(err)
		}
	}

	pruned, err := pruneStaleEmbeddings(
		db,
		"text-embedding-3-small",
		"alpha",
		[]docToEmbed{{path: "alpha/live.md"}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if pruned != 1 {
		t.Fatalf("pruned = %d, want 1", pruned)
	}

	for _, check := range []struct {
		path  string
		model string
		want  int
	}{
		{"alpha/live.md", "text-embedding-3-small", 1},
		{"alpha/stale.md", "text-embedding-3-small", 0},
		{"beta/stale.md", "text-embedding-3-small", 1},
		{"alpha/stale.md", "text-embedding-3-large", 1},
	} {
		var got int
		if err := db.QueryRow(
			"SELECT COUNT(*) FROM embeddings WHERE path = ? AND model = ?",
			check.path,
			check.model,
		).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != check.want {
			t.Fatalf("count(%s, %s) = %d, want %d", check.path, check.model, got, check.want)
		}
	}
}

func TestRerankCandidatesWeightedRRFPreservesBM25Winner(t *testing.T) {
	dir := t.TempDir()
	cacheDir := filepath.Join(dir, ".cache")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(cacheDir, "search.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(embedSchema); err != nil {
		t.Fatal(err)
	}

	// 4 docs in BM25 order: A, B, C, D. Cosine ranks: C=1, D=2, B=3, A=4.
	// Under 0.7/0.3 weighted RRF, BM25 rank 1 (A) keeps its lead because
	// the cosine arm's vote (rank 4) doesn't overcome BM25's structural
	// advantage. A=0.7/61+0.3/64=0.0162; C=0.7/63+0.3/61=0.0160. A wins.
	// This is the desired behavior on identifier-heavy queries — see the
	// fixture sweep in FOLLOW-UPS for why.
	docs := []struct {
		path string
		vec  []float32
	}{
		{"src/A.md", []float32{0, 0, 1}},      // BM25 1, cos = 0   (rank 4)
		{"src/B.md", []float32{0.1, 0, 0.99}}, // BM25 2, cos ≈ 0.1 (rank 3)
		{"src/C.md", []float32{1, 0, 0}},      // BM25 3, cos = 1   (rank 1, semantic winner)
		{"src/D.md", []float32{0.7, 0, 0.7}},  // BM25 4, cos ≈ 0.7 (rank 2)
	}
	for _, d := range docs {
		_, err := db.Exec(
			"INSERT INTO embeddings (path, mtime_ns, model, dim, vec) VALUES (?, ?, ?, ?, ?)",
			d.path, 0, "test", len(d.vec), serializeVec(d.vec))
		if err != nil {
			t.Fatal(err)
		}
	}
	hits := []searchHit{
		{Path: "src/A.md"},
		{Path: "src/B.md"},
		{Path: "src/C.md"},
		{Path: "src/D.md"},
	}
	queryVec := []float32{1, 0, 0}
	got, err := rerankCandidates(db, hits, queryVec)
	if err != nil {
		t.Fatal(err)
	}
	wantOrder := []string{"src/A.md", "src/B.md", "src/C.md", "src/D.md"}
	for i, h := range got {
		if h.Path != wantOrder[i] {
			t.Errorf("rank %d: got %q, want %q (full order: %v)", i, h.Path, wantOrder[i], pathsOf(got))
		}
	}
}

func TestRerankCandidatesRRFKeepsStrongBM25Winner(t *testing.T) {
	// Identifier-query case: BM25 has the right answer at rank 1 with both
	// strong title/path signals AND a high cosine. RRF should preserve
	// that ranking — pure cosine demoted these in the first prototype run
	// (Hit@1 -31pp on the fixture).
	dir := t.TempDir()
	cacheDir := filepath.Join(dir, ".cache")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(cacheDir, "search.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(embedSchema); err != nil {
		t.Fatal(err)
	}
	docs := []struct {
		path string
		vec  []float32
	}{
		{"src/canonical.md", []float32{1, 0, 0}},   // BM25 1, cos 1
		{"src/related.md", []float32{0.9, 0.1, 0}}, // BM25 2, cos ~0.99
		{"src/tangent.md", []float32{0, 1, 0}},     // BM25 3, cos 0
	}
	for _, d := range docs {
		_, err := db.Exec(
			"INSERT INTO embeddings (path, mtime_ns, model, dim, vec) VALUES (?, ?, ?, ?, ?)",
			d.path, 0, "test", len(d.vec), serializeVec(d.vec))
		if err != nil {
			t.Fatal(err)
		}
	}
	hits := []searchHit{
		{Path: "src/canonical.md"},
		{Path: "src/related.md"},
		{Path: "src/tangent.md"},
	}
	got, err := rerankCandidates(db, hits, []float32{1, 0, 0})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"src/canonical.md", "src/related.md", "src/tangent.md"}
	for i, h := range got {
		if h.Path != want[i] {
			t.Errorf("rank %d: got %q, want %q (full: %v)", i, h.Path, want[i], pathsOf(got))
		}
	}
}

func TestRerankCandidatesGracefulMissingVectors(t *testing.T) {
	dir := t.TempDir()
	cacheDir := filepath.Join(dir, ".cache")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(cacheDir, "search.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(embedSchema); err != nil {
		t.Fatal(err)
	}
	// Only doc-b has an embedding.
	_, err = db.Exec(
		"INSERT INTO embeddings (path, mtime_ns, model, dim, vec) VALUES (?, ?, ?, ?, ?)",
		"src/doc-b.md", 0, "test", 3, serializeVec([]float32{1, 0, 0}))
	if err != nil {
		t.Fatal(err)
	}

	// BM25 order: a, b, c. Query aligns with doc-b's vector.
	hits := []searchHit{
		{Path: "src/doc-a.md"},
		{Path: "src/doc-b.md"},
		{Path: "src/doc-c.md"},
	}
	got, err := rerankCandidates(db, hits, []float32{1, 0, 0})
	if err != nil {
		t.Fatal(err)
	}
	// doc-b ranks first (had a vector). doc-a and doc-c keep BM25 order
	// behind it.
	want := []string{"src/doc-b.md", "src/doc-a.md", "src/doc-c.md"}
	for i, h := range got {
		if h.Path != want[i] {
			t.Errorf("rank %d: got %q, want %q (full: %v)", i, h.Path, want[i], pathsOf(got))
		}
	}
}

func TestChunkTextSplitsOnParagraphBoundary(t *testing.T) {
	text := "alpha alpha alpha alpha\n\nbeta beta beta\n\ncharlie"
	got := searchruntime.SplitEmbeddingTextChunks(text, 32)
	if len(got) != 2 {
		t.Fatalf("chunks = %d, want 2: %#v", len(got), got)
	}
	if got[0] != "alpha alpha alpha alpha" {
		t.Fatalf("first chunk = %q, want paragraph boundary split", got[0])
	}
	if !strings.Contains(got[1], "beta beta beta") || !strings.Contains(got[1], "charlie") {
		t.Fatalf("second chunk lost content: %q", got[1])
	}
}

func TestRerankCandidatesChunkedUsesMaxChunkCosine(t *testing.T) {
	dir := t.TempDir()
	cacheDir := filepath.Join(dir, ".cache")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(cacheDir, "search.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(embedSchema); err != nil {
		t.Fatal(err)
	}
	for _, row := range []struct {
		path string
		idx  int
		vec  []float32
	}{
		{"src/semantic.md", 0, []float32{0, 1}},
		{"src/semantic.md", 1, []float32{1, 0}},
	} {
		_, err := db.Exec(
			"INSERT INTO embedding_chunks (path, chunk_idx, chunk_size, mtime_ns, model, dim, vec) VALUES (?, ?, ?, ?, ?, ?, ?)",
			row.path, row.idx, 1500, 0, "test", len(row.vec), serializeVec(row.vec))
		if err != nil {
			t.Fatal(err)
		}
	}
	hits := []searchHit{
		{Path: "src/bm25-only.md"},
		{Path: "src/semantic.md"},
	}
	got, err := rerankCandidatesChunked(db, hits, []float32{1, 0}, "test", 1500)
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Path != "src/semantic.md" {
		t.Fatalf("chunked rerank top = %q, want semantic.md (full order: %v)", got[0].Path, pathsOf(got))
	}
}

func pathsOf(hits []searchHit) []string {
	out := make([]string, len(hits))
	for i, h := range hits {
		out[i] = h.Path
	}
	return out
}

func TestChunkText(t *testing.T) {
	cases := []struct {
		name      string
		text      string
		chunkSize int
		want      []string
	}{
		{"empty", "", 100, []string{""}},
		{"shorter than size", "hello world", 100, []string{"hello world"}},
		{"exactly size", "hello", 5, []string{"hello"}},
		{"hard split, no boundary", "aaaaabbbbbccccc", 5, []string{"aaaaa", "bbbbb", "ccccc"}},
		{"break at paragraph", "first para text\n\nsecond para text", 16,
			// floor = 16/2 - 1 = 7. Iterate i=16 down to 7. text[15]='\n', text[16]='\n'.
			// bestBreak=15. chunk[0]="first para text", start advances past \n\n.
			[]string{"first para text", "second para text"}},
		{"chunk size 0 returns single", "anything", 0, []string{"anything"}},
		{"chunk size negative returns single", "anything", -1, []string{"anything"}},
		{"break preferred over hard split when in last half",
			// 28 chars. \n\n at positions 16-17 sits in the last half of the
			// 24-char window [floor=11, end=24]. After first chunk, the
			// remaining "opqrstuvwx" fits in one more chunk.
			"abcdef ghij klmn\n\nopqrstuvwx",
			24,
			[]string{"abcdef ghij klmn", "opqrstuvwx"}},
	}
	for _, c := range cases {
		got := searchruntime.SplitEmbeddingTextChunks(c.text, c.chunkSize)
		if len(got) != len(c.want) {
			t.Errorf("%s: got %d chunks, want %d (%v vs %v)", c.name, len(got), len(c.want), got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("%s: chunk[%d] = %q, want %q", c.name, i, got[i], c.want[i])
			}
		}
	}
}
