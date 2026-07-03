package main

import (
	"bytes"
	"context"
	"database/sql"
	_ "embed"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/nstranquist/docs-puller/internal/ftsdb"
	"github.com/nstranquist/docs-puller/searchruntime"

	_ "modernc.org/sqlite"
)

// FTS5-backed search. Index lives at <out>/.cache/search.db.
//
// Schema choices:
//   - Porter stemmer + unicode61 tokenizer: word-boundary matching (so "RAG"
//     no longer matches "stoRAGe") plus light stemming ("row" matches "rows").
//   - title, path_tokens, body are indexed; path/source/url are UNINDEXED
//     metadata. path_tokens is the relative path stuffed verbatim into a
//     searchable column — porter+unicode61 already splits on /, _, ., -, so
//     "platform/_schema/tables/public.users.md" indexes as the tokens
//     {platform, schema, tables, public, users, md}.
//   - bm25() column weights of (title=5, path_tokens=3, body=1) give path
//     components more weight than body but less than title. This is what
//     lets queries like "clickhouse introspection" rank the canonical
//     `docs/local-dev/clickhouse-introspection.md` above body-mention pages
//     in other clickhouse docs.
//
// Snippets aren't stored — they're extracted from body on each query, same
// shape as the substring scan path so callers don't care which backend ran.

//go:embed sqlc/runtime_schema.sql
var ftsSchema string

const (
	ftsSchemaDocsTableName = "docs"
)

type ftsIndex struct {
	db *sql.DB
	q  *ftsdb.Queries
}

const (
	ftsReadModeReadOnly  = "ro"
	ftsReadModeImmutable = "immutable"
)

// cand holds a search candidate with its body (for deferred snippet
// extraction) and flags for which qualification tier(s) it came from.
// Title-tier and path-tier each provide a base score floor so canonical
// hub pages with short titles aren't buried by BM25 length norm. Used by
// both `search` and the helper `runTier`.
type cand struct {
	hit       searchHit
	body      string
	fromTitle bool
	fromPath  bool
	tieBreak  int
}

func ftsDBPath(out string) string {
	return filepath.Join(out, ".cache", "search.db")
}

// openFTSIndex opens (or creates) the index. Caller must close.
func openFTSIndex(out string) (*ftsIndex, error) {
	return openFTSIndexMode(out, false, nil)
}

// openFTSIndexReadOnly opens an existing index in SQLite read-only mode.
// Used by the long-lived serve connection so it never acquires write locks
// — without this, an out-of-process `pull-url` writer hits SQLITE_BUSY
// when serve's idle connection holds a SHARED-lock-equivalent state. Caller
// must guarantee the file already exists with a populated schema (the
// read-only opener intentionally skips the CREATE TABLE init step).
func openFTSIndexReadOnly(out string) (*ftsIndex, error) {
	return openFTSIndexReadMode(out, ftsReadModeReadOnly)
}

// openFTSIndexImmutable opens an existing checkpointed index as an immutable
// SQLite URI. This is benchmark-only: immutable readers avoid SQLite file
// locking, but they must not be used when a WAL file contains uncheckpointed
// writes.
func openFTSIndexImmutable(out string) (*ftsIndex, error) {
	return openFTSIndexReadMode(out, ftsReadModeImmutable)
}

func openFTSIndexReadMode(out string, readMode string) (*ftsIndex, error) {
	switch normalizeFTSReadMode(readMode) {
	case ftsReadModeReadOnly:
		return openFTSIndexMode(out, true, nil)
	case ftsReadModeImmutable:
		if err := requireCheckpointedFTSIndex(out); err != nil {
			return nil, err
		}
		return openFTSIndexMode(out, true, url.Values{
			"immutable": []string{"1"},
			"mode":      []string{"ro"},
		})
	default:
		return nil, searchruntime.UnsupportedFTSReadModeError(readMode)
	}
}

func normalizeFTSReadMode(readMode string) string {
	if strings.TrimSpace(readMode) == "" {
		return ftsReadModeReadOnly
	}
	return strings.TrimSpace(strings.ToLower(readMode))
}

func requireCheckpointedFTSIndex(out string) error {
	walPath := ftsDBPath(out) + "-wal"
	info, err := os.Stat(walPath)
	if err == nil && info.Size() > 0 {
		return searchruntime.ImmutableFTSReadRequiresCheckpointError(walPath)
	}
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func openFTSIndexMode(out string, readOnly bool, readQuery url.Values) (*ftsIndex, error) {
	ctx := context.Background()
	cacheDir := filepath.Join(out, ".cache")
	if readOnly {
		if _, err := os.Stat(cacheDir); err != nil {
			return nil, err
		}
	} else {
		if err := os.MkdirAll(cacheDir, 0o755); err != nil {
			return nil, err
		}
	}
	dsn := ftsDBPath(out)
	if readOnly {
		if readQuery == nil {
			readQuery = url.Values{"mode": []string{"ro"}}
		}
		dsn = sqliteFileURI(dsn, readQuery)
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	// Wait up to 5s under contention instead of returning SQLITE_BUSY
	// immediately. The ftsIndex uses one connection so this PRAGMA applies
	// consistently for the lifetime of the handle. 5s lets a slow reindex
	// finish without making interactive callers feel hung.
	if _, err := db.ExecContext(ctx, "PRAGMA busy_timeout=5000"); err != nil {
		db.Close()
		return nil, searchruntime.FTSIndexBusyTimeoutError(err)
	}
	if !readOnly {
		// Switch to WAL on every R/W open. Persistent on the file — no-op
		// after the first writer enables it. Without WAL, the rollback
		// journal blocks readers during writes (and writers during reads),
		// which silently degrades concurrent eval/search workloads to scan
		// mode via the SQLITE_BUSY fallback.
		if _, err := db.ExecContext(ctx, "PRAGMA journal_mode=WAL"); err != nil {
			db.Close()
			return nil, searchruntime.FTSIndexJournalModeError(err)
		}
		// search.db is a derived, rebuildable FTS cache. WAL + NORMAL keeps
		// transactions consistent while avoiding the extra fsync cost of FULL
		// durability for a file we can recreate from the markdown corpus.
		if _, err := db.ExecContext(ctx, "PRAGMA synchronous=NORMAL"); err != nil {
			db.Close()
			return nil, searchruntime.FTSIndexSynchronousModeError(err)
		}
		// Schema migration: when the indexed-column set changes (e.g.
		// adding `path_tokens`), CREATE TABLE IF NOT EXISTS is a no-op
		// and the new column never lands. Detect the column shape and
		// drop+rebuild if it doesn't match. Caller is expected to follow
		// up with rebuild() to repopulate; otherwise the index is empty
		// after migration. This is checked once per open (cheap).
		if err := migrateFTSSchema(db); err != nil {
			db.Close()
			return nil, searchruntime.FTSIndexMigrateSchemaError(err)
		}
		if _, err := db.ExecContext(ctx, ftsSchema); err != nil {
			db.Close()
			return nil, searchruntime.FTSIndexInitSchemaError(err)
		}
	}
	return &ftsIndex{db: db, q: ftsdb.New(db)}, nil
}

func sqliteFileURI(path string, query url.Values) string {
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(path)}
	u.RawQuery = query.Encode()
	return u.String()
}

// migrateFTSSchema drops the docs / docs_path tables when their column
// shape doesn't match the current ftsSchema. The next CREATE TABLE IF NOT
// EXISTS in openFTSIndexMode rebuilds them empty; the caller is expected
// to run rebuild() to repopulate. We probe by SELECTing the new columns —
// SQLite returns "no such column" if the schema is stale.
func migrateFTSSchema(db *sql.DB) error {
	ctx := context.Background()
	var n int
	row := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, ftsSchemaDocsTableName)
	if err := row.Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		return nil // fresh DB, CREATE will land the new schema
	}
	// docs exists — verify it has the latest indexed columns. The cheapest
	// probe is a no-op SELECT against an empty result; FTS5 errors on
	// unknown columns. Probe for the *most recent* added column (is_hub) so
	// adding future columns just needs the probe updated.
	q := ftsdb.New(db)
	if _, err := q.ProbeDocsIsHub(ctx); err == nil {
		return nil // schema is current
	}
	fmt.Fprintln(os.Stderr, "fts5: schema migration — dropping docs/docs_path to add is_hub column (full rebuild required)")
	if err := q.DropDocs(ctx); err != nil {
		return err
	}
	if err := q.DropDocsPath(ctx); err != nil {
		return err
	}
	return nil
}

func (f *ftsIndex) close() error {
	if f == nil || f.db == nil {
		return nil
	}
	return f.db.Close()
}

// liveFTSIndex wraps an ftsIndex with mtime-based reopen so the persistent
// `serve` connection picks up out-of-process reindex/pull updates without a
// server restart. Cost per search is one Stat (~µs); reopen happens only
// when search.db's mtime advances. The mutex serializes search calls — fine
// for a single-user dev tool, ensures the old connection isn't closed mid-
// request after a swap.
type liveFTSIndex struct {
	out   string
	mu    sync.Mutex
	idx   *ftsIndex
	mtime time.Time
}

// newLiveFTSIndex opens the index if it exists. Returns (nil, nil) when no
// index file is present yet — caller should fall back to per-request open.
func newLiveFTSIndex(out string) (*liveFTSIndex, error) {
	if !ftsIndexExists(out) {
		return nil, nil
	}
	idx, err := openFTSIndexReadOnly(out)
	if err != nil {
		return nil, err
	}
	l := &liveFTSIndex{out: out, idx: idx}
	if info, err := os.Stat(ftsDBPath(out)); err == nil {
		l.mtime = info.ModTime()
	}
	return l, nil
}

// withSearch holds the mutex for the duration of fn so a concurrent reopen
// can't close the *ftsIndex while fn is using it. fn receives nil when the
// index is unavailable (file gone, reopen failed) — match dispatchSearch's
// behavior of falling back to substring scan.
func (l *liveFTSIndex) withSearch(fn func(*ftsIndex)) {
	if l == nil {
		fn(nil)
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.maybeReopen()
	fn(l.idx)
}

// maybeReopen swaps in a fresh connection when search.db's mtime has
// advanced since the last reopen. Called under l.mu.
func (l *liveFTSIndex) maybeReopen() {
	info, err := os.Stat(ftsDBPath(l.out))
	if err != nil {
		return
	}
	if !info.ModTime().After(l.mtime) {
		return
	}
	fresh, err := openFTSIndexReadOnly(l.out)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fts5: reopen failed (using stale connection): %v\n", err)
		return
	}
	if l.idx != nil {
		l.idx.close()
	}
	l.idx = fresh
	l.mtime = info.ModTime()
}

func (l *liveFTSIndex) close() {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.idx != nil {
		l.idx.close()
		l.idx = nil
	}
}

// updateFTS picks the cheapest valid strategy: incremental upsert when the
// index already has docs, full rebuild on a cold start (so the corpus from
// prior pulls — which the caller didn't pass — gets indexed too). Callers
// pass the per-pull list of relative paths ("<source>/<rel>.md") that just
// changed on disk; on cold start the list is ignored.
//
// Tradeoff: if the on-disk corpus and the FTS5 index drift (e.g. a doc was
// rm'd between pulls), incremental updates won't catch the deletion. The
// `reindex` command stays as the authoritative full rebuild.
func (f *ftsIndex) updateFTS(out string, changedPaths []string) error {
	n, _ := f.totalDocs()
	if n == 0 {
		return f.rebuild(out)
	}
	return f.upsertPaths(out, changedPaths)
}

// replaceSources refreshes complete source directories after an on-disk
// replacement. It deletes every indexed row for each source, then re-indexes
// the current files under <out>/<source> in one transaction. This is the
// source-scoped counterpart to rebuild for pinned overlays that replace their
// whole directory at once.
func (f *ftsIndex) replaceSources(out string, sources []string) error {
	sources = uniqueStrings(sources)
	if len(sources) == 0 {
		return nil
	}
	n, _ := f.totalDocs()
	if n == 0 {
		return f.rebuild(out)
	}
	ctx := context.Background()
	tx, err := f.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	q := f.q.WithTx(tx)

	inserter := ftsPathInserter{
		out:       out,
		manifests: map[string]map[string]ftsDocMeta{},
		ctx:       ctx,
		q:         q,
	}
	for _, src := range sources {
		src = strings.TrimSpace(src)
		if src == "" || strings.ContainsAny(src, `/\`) {
			continue
		}
		if err := q.DeletePathsBySource(ctx, src); err != nil {
			return err
		}
		if err := q.DeleteDocsBySource(ctx, src); err != nil {
			return err
		}
		srcDir := filepath.Join(out, src)
		walkErr := filepath.WalkDir(srcDir, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			name := d.Name()
			if !strings.HasSuffix(name, ".md") || name == "_INDEX.md" {
				return nil
			}
			rel, err := filepath.Rel(srcDir, p)
			if err != nil {
				return err
			}
			return inserter.insert(filepath.Join(src, rel), "replace-source")
		})
		if walkErr != nil {
			if os.IsNotExist(walkErr) {
				continue
			}
			return walkErr
		}
	}
	return tx.Commit()
}

type ftsMemoryDoc struct {
	Path     string
	AbsPath  string
	Body     []byte
	URL      string
	Warning  string
	Title    string
	HubKnown bool
	IsHub    bool
}

// applyFTSMemoryHubFlags derives hub-page metadata from a complete memory
// corpus. Use it only when docs represents the full copied corpus; incremental
// callers without a complete path set should leave HubKnown false and keep the
// existing filesystem-backed isHubDoc check.
func applyFTSMemoryHubFlags(docs []ftsMemoryDoc) {
	dirs := map[string]struct{}{}
	for _, doc := range docs {
		relPath := filepath.ToSlash(doc.Path)
		dir := filepath.Dir(relPath)
		for dir != "." && dir != "/" && dir != "" {
			dirs[dir] = struct{}{}
			next := filepath.Dir(dir)
			if next == dir {
				break
			}
			dir = next
		}
	}
	for i := range docs {
		relPath := filepath.ToSlash(docs[i].Path)
		docs[i].HubKnown = true
		if !strings.HasSuffix(relPath, ".md") {
			docs[i].IsHub = false
			continue
		}
		_, ok := dirs[strings.TrimSuffix(relPath, ".md")]
		docs[i].IsHub = ok
	}
}

func (f *ftsIndex) updateFTSFromMemory(out string, changedPaths []string, docs []ftsMemoryDoc, coversCorpus bool) error {
	if len(docs) == 0 {
		return f.updateFTS(out, changedPaths)
	}
	n, _ := f.totalDocs()
	if n == 0 {
		if coversCorpus {
			return f.rebuildFromMemory(docs)
		}
		return f.rebuild(out)
	}
	return f.upsertMemoryDocs(changedPaths, docs)
}

// upsertPaths replaces (or inserts) FTS5 rows for the given paths. Each
// path is rooted at <out> (e.g. "supabase/guides/rls.md"). Inside one
// transaction: SELECT rowid via the docs_path side table, DELETE that
// rowid from docs, INSERT fresh, REPLACE docs_path with the new rowid.
// Caller is expected to hold the write lock.
//
// A path whose underlying file no longer exists results in just the DELETE
// taking effect — the index converges toward truth even when files vanish.
func (f *ftsIndex) upsertPaths(out string, paths []string) error {
	if len(paths) == 0 {
		return nil
	}
	ctx := context.Background()
	tx, err := f.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	q := f.q.WithTx(tx)

	// Cache per-source manifest reads so a 50-URL pull from one sitemap
	// doesn't re-parse the same manifest.json 50 times.
	inserter := ftsPathInserter{
		out:       out,
		manifests: map[string]map[string]ftsDocMeta{},
		ctx:       ctx,
		q:         q,
	}

	for _, relPath := range paths {
		i := strings.IndexByte(relPath, '/')
		if i < 0 {
			continue
		}

		var oldRowID int64
		switch oldRowID, err = q.LookupDocRowIDByPath(ctx, relPath); err {
		case nil:
			if err := q.DeleteDocByRowID(ctx, oldRowID); err != nil {
				return err
			}
			if err := q.DeletePathByPath(ctx, relPath); err != nil {
				return err
			}
		case sql.ErrNoRows:
			// Not previously indexed; insert is fresh.
		default:
			return err
		}

		if err := inserter.insert(relPath, "upsert"); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (f *ftsIndex) upsertMemoryDocs(paths []string, docs []ftsMemoryDoc) error {
	if len(paths) == 0 {
		return nil
	}
	docByPath := map[string]ftsMemoryDoc{}
	for _, doc := range docs {
		if doc.Path != "" {
			docByPath[doc.Path] = doc
		}
	}
	ctx := context.Background()
	tx, err := f.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	q := f.q.WithTx(tx)

	inserter := ftsMemoryInserter{
		ctx: ctx,
		q:   q,
	}
	for _, relPath := range paths {
		if _, _, ok := splitFTSPath(relPath); !ok {
			continue
		}
		var oldRowID int64
		switch oldRowID, err = q.LookupDocRowIDByPath(ctx, relPath); err {
		case nil:
			if err := q.DeleteDocByRowID(ctx, oldRowID); err != nil {
				return err
			}
			if err := q.DeletePathByPath(ctx, relPath); err != nil {
				return err
			}
		case sql.ErrNoRows:
			// Not previously indexed; insert is fresh.
		default:
			return err
		}
		doc, ok := docByPath[relPath]
		if !ok {
			// Preserve deletion convergence for callers that pass a path whose
			// file disappeared after change detection.
			continue
		}
		if err := inserter.insert(doc, "upsert"); err != nil {
			return err
		}
	}
	return tx.Commit()
}

type ftsPathInserter struct {
	out       string
	manifests map[string]map[string]ftsDocMeta
	ctx       context.Context
	q         *ftsdb.Queries
}

func (w *ftsPathInserter) insert(relPath, op string) error {
	src, rel, ok := splitFTSPath(relPath)
	if !ok {
		return nil
	}
	metaByPath, ok := w.manifests[src]
	if !ok {
		metaByPath = loadFTSDocMeta(filepath.Join(w.out, src), src)
		w.manifests[src] = metaByPath
	}

	absPath := filepath.Join(w.out, relPath)
	data, err := os.ReadFile(absPath)
	if err != nil {
		return nil
	}
	meta := metaByPath[rel]
	return insertFTSDoc(w.ctx, w.q, relPath, absPath, data, meta, op)
}

type ftsMemoryInserter struct {
	ctx context.Context
	q   *ftsdb.Queries
}

func (w *ftsMemoryInserter) insert(doc ftsMemoryDoc, op string) error {
	absPath := doc.AbsPath
	if absPath == "" {
		absPath = doc.Path
	}
	meta := ftsDocMeta{URL: doc.URL, Warning: doc.Warning, Title: doc.Title, HubKnown: doc.HubKnown, IsHub: doc.IsHub}
	return insertFTSDoc(w.ctx, w.q, doc.Path, absPath, doc.Body, meta, op)
}

type ftsDocInsertPayload struct {
	relPath string
	src     string
	title   string
	body    string
	url     string
	isHub   int
}

func makeFTSDocInsertPayload(relPath, absPath string, data []byte, meta ftsDocMeta) (ftsDocInsertPayload, bool) {
	src, rel, ok := splitFTSPath(relPath)
	if !ok {
		return ftsDocInsertPayload{}, false
	}
	if !shouldIndexFTSDoc(src, rel, data, meta) {
		return ftsDocInsertPayload{}, false
	}
	isHub := false
	if meta.HubKnown {
		isHub = meta.IsHub
	} else {
		isHub = isHubDoc(absPath)
	}
	return ftsDocInsertPayload{
		relPath: relPath,
		src:     src,
		title:   ftsTitle(absPath, data, meta),
		body:    string(data),
		url:     meta.URL,
		isHub:   boolToInt(isHub),
	}, true
}

func ftsTitle(absPath string, data []byte, meta ftsDocMeta) string {
	if meta.Title != "" {
		return meta.Title
	}
	return extractTitleFromBytes(absPath, data)
}

func insertFTSDoc(ctx context.Context, q *ftsdb.Queries, relPath, absPath string, data []byte, meta ftsDocMeta, op string) error {
	payload, ok := makeFTSDocInsertPayload(relPath, absPath, data, meta)
	if !ok {
		return nil
	}
	res, err := q.InsertDoc(ctx, ftsdb.InsertDocParams{
		Path:       payload.relPath,
		Source:     payload.src,
		Title:      &payload.title,
		PathTokens: payload.relPath,
		Body:       payload.body,
		Url:        &payload.url,
		IsHub:      int64(payload.isHub),
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "fts5 %s %s: %v\n", op, relPath, err)
		return nil
	}
	newRowID, err := res.LastInsertId()
	if err != nil {
		return err
	}
	if err := q.InsertPath(ctx, ftsdb.InsertPathParams{Path: relPath, Rowid: newRowID}); err != nil {
		return err
	}
	return nil
}

func insertFTSDocWithRowID(ctx context.Context, q *ftsdb.Queries, rowID int64, relPath, absPath string, data []byte, meta ftsDocMeta, op string) (bool, error) {
	payload, ok := makeFTSDocInsertPayload(relPath, absPath, data, meta)
	if !ok {
		return false, nil
	}
	if err := q.InsertDocWithRowID(ctx, ftsdb.InsertDocWithRowIDParams{
		Rowid:      rowID,
		Path:       payload.relPath,
		Source:     payload.src,
		Title:      &payload.title,
		PathTokens: payload.relPath,
		Body:       payload.body,
		Url:        &payload.url,
		IsHub:      int64(payload.isHub),
	}); err != nil {
		fmt.Fprintf(os.Stderr, "fts5 %s %s: %v\n", op, relPath, err)
		return false, nil
	}
	return true, nil
}

func splitFTSPath(relPath string) (string, string, bool) {
	relPath = filepath.ToSlash(relPath)
	i := strings.IndexByte(relPath, '/')
	if i < 0 {
		return "", "", false
	}
	return relPath[:i], relPath[i+1:], true
}

// rebuild clears and re-populates the index from <out>/<source>/*.md.
// Also rebuilds the docs_path side table — both stay in sync as a single
// transaction so concurrent reads see a consistent snapshot.
func (f *ftsIndex) rebuild(out string) error {
	sources, err := listSources(out)
	if err != nil {
		return err
	}
	// Atomic rebuild: DELETE + INSERT all in one transaction, so concurrent
	// readers (eval, serve, agent searches) see the OLD complete state via
	// WAL snapshot semantics until COMMIT swaps to the NEW state. Without
	// this, the DELETEs ran outside the txn and committed immediately,
	// leaving readers in a `count=0` window for the entire INSERT phase
	// (was failure mode #4 in FOLLOW-UPS — partial-corpus eval pollution).
	ctx := context.Background()
	tx, err := f.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	q := f.q.WithTx(tx)
	if err := q.DeleteAllDocs(ctx); err != nil {
		return err
	}
	if err := q.DeleteAllDocsPath(ctx); err != nil {
		return err
	}

	nextRowID := int64(1)
	for _, src := range sources {
		srcDir := filepath.Join(out, src)
		metaByPath := loadFTSDocMeta(srcDir, src)
		walkErr := filepath.WalkDir(srcDir, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			name := d.Name()
			if !strings.HasSuffix(name, ".md") || name == "_INDEX.md" {
				return nil
			}
			data, err := os.ReadFile(p)
			if err != nil {
				return nil
			}
			rel, _ := filepath.Rel(srcDir, p)
			meta := metaByPath[rel]
			relPath := filepath.Join(src, rel)
			inserted, err := insertFTSDocWithRowID(ctx, q, nextRowID, relPath, p, data, meta, "insert")
			if err != nil {
				return err
			}
			if inserted {
				nextRowID++
			}
			return nil
		})
		if walkErr != nil {
			return walkErr
		}
	}
	if err := q.RebuildPaths(ctx); err != nil {
		return err
	}
	return tx.Commit()
}

func (f *ftsIndex) rebuildFromMemory(docs []ftsMemoryDoc) error {
	ctx := context.Background()
	tx, err := f.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	q := f.q.WithTx(tx)
	if err := q.DeleteAllDocs(ctx); err != nil {
		return err
	}
	if err := q.DeleteAllDocsPath(ctx); err != nil {
		return err
	}

	nextRowID := int64(1)
	for _, doc := range docs {
		absPath := doc.AbsPath
		if absPath == "" {
			absPath = doc.Path
		}
		meta := ftsDocMeta{URL: doc.URL, Warning: doc.Warning, Title: doc.Title, HubKnown: doc.HubKnown, IsHub: doc.IsHub}
		inserted, err := insertFTSDocWithRowID(ctx, q, nextRowID, doc.Path, absPath, doc.Body, meta, "insert")
		if err != nil {
			return err
		}
		if inserted {
			nextRowID++
		}
	}
	if err := q.RebuildPaths(ctx); err != nil {
		return err
	}
	return tx.Commit()
}

func (f *ftsIndex) totalDocs() (int, error) {
	n, err := f.q.CountDocs(context.Background())
	return int(n), err
}

type ftsDocMeta struct {
	URL      string
	Warning  string
	Title    string
	HubKnown bool
	IsHub    bool
}

func loadFTSDocMeta(srcDir, srcName string) map[string]ftsDocMeta {
	out := map[string]ftsDocMeta{}
	m, err := loadOrMigrateManifest(srcDir)
	if err != nil {
		return out
	}
	prefix := srcName + "/"
	for _, r := range m.Entries {
		rel := strings.TrimPrefix(r.Path, prefix)
		if rel == "" || rel == r.Path {
			continue
		}
		out[rel] = ftsDocMeta{URL: r.URL, Warning: r.Warning}
	}
	return out
}

func shouldIndexFTSDoc(source, rel string, data []byte, meta ftsDocMeta) bool {
	if !shouldIndexFTSDocPath(source, rel, meta) {
		return false
	}
	return len(bytes.TrimSpace(data)) > 0
}

func shouldIndexFTSDocPath(source, rel string, meta ftsDocMeta) bool {
	rel = filepath.ToSlash(rel)
	if rel == "" || rel == "_INDEX.md" || strings.HasSuffix(rel, "/_INDEX.md") {
		return false
	}
	if strings.Contains(strings.ToLower(meta.Warning), "low-content") {
		return false
	}
	if isNonEnglishLocalePath(rel) {
		return false
	}
	if isDocsPullerBenchmarkNote(source, rel) {
		return false
	}
	return true
}

func isNonEnglishLocalePath(rel string) bool {
	segments := strings.Split(filepath.ToSlash(rel), "/")
	max := len(segments)
	if max > 3 {
		max = 3
	}
	for i := 0; i < max; i++ {
		seg := strings.TrimSuffix(strings.ToLower(segments[i]), ".md")
		if nonEnglishLocaleCodes[seg] {
			return true
		}
	}
	return false
}

var nonEnglishLocaleCodes = map[string]bool{
	"ar": true, "bg": true, "cs": true, "da": true, "de": true,
	"el": true, "es": true, "fi": true, "fr": true, "he": true,
	"hi": true, "id": true, "it": true, "ja": true, "ko": true,
	"nl": true, "no": true, "pl": true, "pt": true, "pt-br": true,
	"ro": true, "ru": true, "sv": true, "th": true, "tr": true,
	"uk": true, "vi": true, "zh": true, "zh-cn": true, "zh-hans": true,
	"zh-hant": true, "zh-tw": true,
}

// benchmarkNoteSources are note-corpus source names checked for
// self-referential docs-puller benchmark notes: notes ABOUT the retrieval
// bench pollute results FOR the bench, so the indexer skips them. Internal
// builds extend both lists (see the nicosinternal build tag).
var benchmarkNoteSources = map[string]bool{"kb": true, "vault": true}

var benchmarkNoteNameMarkers = []string{
	"docs-puller-retrieval-bench",
	"docs-puller-session-handoff",
	"docs-puller-handoff-prompt",
}

func isDocsPullerBenchmarkNote(source, rel string) bool {
	if !benchmarkNoteSources[source] {
		return false
	}
	name := strings.ToLower(filepath.Base(rel))
	for _, marker := range benchmarkNoteNameMarkers {
		if strings.Contains(name, marker) {
			return true
		}
	}
	return false
}

// isHubDoc reports whether absPath identifies a "hub" doc — a Markdown
// file that documents the parent topic for a sibling subdirectory of the
// same basename. For `<dir>/<basename>.md`, the doc is a hub iff
// `<dir>/<basename>/` exists as a directory. Examples that qualify:
//
//	chrome/docs/extensions/reference/manifest.md  (sibling manifest/ dir
//	   contains <field>.md sub-pages — manifest.md is the topic landing
//	   page for the whole manifest reference)
//	microsoft-learn/cli/azure/group.md  (sibling group/ dir with subcommand
//	   docs — group.md is the canonical "az group" landing page)
//
// At rerank, hub docs get +searchHubBoost so they win their own subtree
// against descendant pages whose long titles + paths qualify them for
// title-tier and path-tier on multi-token queries. The check is one os.Stat
// per indexed doc, amortized into the rebuild walk — no measurable cost.
func isHubDoc(absPath string) bool {
	if !strings.HasSuffix(absPath, ".md") {
		return false
	}
	siblingDir := absPath[:len(absPath)-3]
	info, err := os.Stat(siblingDir)
	if err != nil {
		return false
	}
	return info.IsDir()
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// profileBoostFromEnv reads DOCS_PROFILE_BOOST / DOCS_PROFILE_SUB_BOOST
// at search time so eval sweeps can tune the lift without recompiling.
// Falls back to the calibrated package-level defaults
// (searchProfileBoost / searchProfileSubBoost). Negative values are
// clamped to 0 to keep the boost monotonic.
func profileBoostFromEnv() (boost, subBoost int) {
	boost = searchProfileBoost
	subBoost = searchProfileSubBoost
	if v := os.Getenv("DOCS_PROFILE_BOOST"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			boost = n
		}
	}
	if v := os.Getenv("DOCS_PROFILE_SUB_BOOST"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			subBoost = n
		}
	}
	return boost, subBoost
}

// search runs an FTS5 query, returns hits ranked by BM25 (lower bm25 = better
// match in SQLite FTS5; we negate so larger Score is still better in our
// uniform output struct). Snippets re-extracted from the body row.
//
// Two-tier strategy:
//
//  1. Body-tier: standard `docs MATCH q` with bm25(title=5, body=1) — finds
//     docs by overall keyword density. Subject to BM25 length-norm penalty.
//
//  2. Title-tier: `docs MATCH 'title:(q)'` with bm25(title=1, body=0) —
//     finds docs whose title contains every query token. Critical for
//     long reference docs (e.g. 245 KB go/ref/spec.md) that BM25 length-
//     norm buries below shorter blog posts. Title-tier candidates that
//     would otherwise miss the body-tier cut are added to the pool with
//     a base-score floor.
//
// In-Go re-rank applies on top of both: per-token title boost, exact
// title match, source-keyword boost.
func (f *ftsIndex) search(query, source string, limit int, exact bool, profile *Profile, strict bool) ([]searchHit, error) {
	return f.searchWithOptions(query, source, limit, exact, profile, strict, true)
}

func (f *ftsIndex) searchWithOptions(query, source string, limit int, exact bool, profile *Profile, strict bool, includeSnippets bool) ([]searchHit, error) {
	q := ftsBuildQuery(query, exact)
	if q == "" {
		return nil, nil
	}

	// Over-fetch so the in-Go re-rank has candidates to reorder.
	sqlLimit := limit*5 + 10

	candByPath := make(map[string]*cand, sqlLimit*2)

	// 1. Title-tier query. bm25 weights are (title, path_tokens, body) =
	//    (1, 0, 0) so only the title contributes to ranking on this pass.
	//    Source-aware: source-keyword tokens like "supabase" are stripped so
	//    canonical short-titled docs ("Storage", "Edge Functions") qualify.
	//    When we strip, we ALSO constrain the title-tier to the inferred
	//    source — without that, "sentry releases cli" with "sentry" stripped
	//    would flood the pool with Slack changelog pages whose title also
	//    has "cli". Explicit --source filter wins over inferred.
	const titleSQL = `SELECT path, source, title, url, body, bm25(docs, 1, 0, 0) AS rank
	                  FROM docs WHERE docs MATCH ?`
	const titleSQLNoBody = `SELECT path, source, title, url, bm25(docs, 1, 0, 0) AS rank
	                        FROM docs WHERE docs MATCH ?`
	titleQ, inferredSource := ftsBuildTitleQuery(query, exact)
	titleSourceFilter := source
	if titleSourceFilter == "" {
		titleSourceFilter = inferredSource
	}
	titleLimit := limit*2 + 5
	if titleQ != "" {
		if err := f.runTier(candByPath, titleSQL, titleSQLNoBody, titleQ, titleSourceFilter, titleLimit, true, false, includeSnippets); err != nil {
			return nil, err
		}
	}

	// 1b. Path-tier query — same shape as title-tier but matches against
	//     the `path_tokens` column. Lifts canonical hub pages whose paths
	//     encode the topic but whose short titles don't have all query
	//     tokens (chrome's `extensions/reference/manifest.md` titled just
	//     "Manifest file format" — fails title-tier, qualifies for path-tier
	//     because path tokens cover [extensions, manifest]). Same +200 base
	//     score floor: a path-encoded canonical deserves the same lift as a
	//     title-matching canonical.
	//
	//     Larger over-fetch than title-tier because path tokens are far less
	//     unique than titles: vendor docs commonly have 50+ paths sharing
	//     the same matching tokens (chrome has 53 paths matching
	//     `path_tokens:(extensions manifest)`). FTS5's BM25 over path_tokens
	//     surprisingly favors longer paths (sub-pages outrank the canonical
	//     hub doc), so the canonical lands near the BOTTOM of the BM25-
	//     sorted list. Without a generous fetch window the canonical
	//     doesn't even enter the candidate pool, regardless of the +200
	//     floor and depth-penalty rerank. The body-tier fetch ratio
	//     (sqlLimit = limit*5 + 10) is the right baseline; bump slightly
	//     so chrome-grade verbose hierarchies fit comfortably.
	const pathSQL = `SELECT path, source, title, url, body, bm25(docs, 0, 1, 0) AS rank
	                 FROM docs WHERE docs MATCH ?`
	const pathSQLNoBody = `SELECT path, source, title, url, bm25(docs, 0, 1, 0) AS rank
	                       FROM docs WHERE docs MATCH ?`
	pathQ, pathInferredSource := ftsBuildPathQuery(query, exact)
	pathSourceFilter := source
	if pathSourceFilter == "" {
		pathSourceFilter = pathInferredSource
	}
	pathLimit := limit*8 + 30
	if pathQ != "" {
		if err := f.runTier(candByPath, pathSQL, pathSQLNoBody, pathQ, pathSourceFilter, pathLimit, false, true, includeSnippets); err != nil {
			return nil, err
		}
	}

	// 2. Body-tier query. bm25 weights (title, path_tokens, body) = (5, 3, 1).
	//    path_tokens > body lifts canonical docs whose basename or path
	//    encodes the topic ("clickhouse-introspection.md" should beat docs
	//    that mention clickhouse and introspection only in body) but stays
	//    below title since title is the strongest signal.
	const bodySQL = `SELECT path, source, title, url, body, bm25(docs, 5, 3, 1) AS rank
	                 FROM docs WHERE docs MATCH ?`
	const bodySQLNoBody = `SELECT path, source, title, url, bm25(docs, 5, 3, 1) AS rank
	                       FROM docs WHERE docs MATCH ?`
	if err := f.runTier(candByPath, bodySQL, bodySQLNoBody, q, source, sqlLimit, false, false, includeSnippets); err != nil {
		return nil, err
	}

	qLower := strings.TrimSpace(strings.ToLower(query))
	qTokens := ftsScoringTokens(query, exact)
	intendedSources := sourcesFromQueryTokens(qTokens)

	profileBoost, profileSubBoost := profileBoostFromEnv()
	cands := make([]*cand, 0, len(candByPath))
	for _, c := range candByPath {
		// Profile membership check. Strict mode drops non-members so they
		// never compete in the rerank or get returned. Boost mode adds an
		// additive lift so canonical stack docs win over equivalents from
		// off-stack sources without losing the long tail.
		if profile != nil {
			rel := searchruntime.RelPathInSource(c.hit.Path, c.hit.Source)
			in, sub := profile.Match(c.hit.Source, rel)
			if strict && !in {
				continue
			}
			if in {
				c.hit.InProfile = true
				c.hit.InProfileSub = sub
			}
		}
		// Title-tier / path-tier base-score floor: a candidate whose title
		// OR path_tokens matches every query token deserves to compete even
		// if BM25 length-norm buried it on the body pass. Take max(BM25
		// score, base) so docs with strong body BM25 don't get downgraded.
		// Path-tier shares the same floor as title-tier — a path-encoded
		// canonical deserves the same lift as a title-matching canonical.
		if (c.fromTitle || c.fromPath) && c.hit.Score < searchTitleTierBaseScore {
			c.hit.Score = searchTitleTierBaseScore
		}
		// Title-boost layers (apply after the floor so title-tier hits
		// can still earn the exact-match +50 on top):
		//  - exact full-title match → big bonus (canonical doc wins decisively).
		//  - per-token presence in title → small bonus.
		//
		// The per-token loop intentionally skips a token that's a source
		// keyword for this candidate's source (e.g. "supabase" for a doc
		// under supabase/). The source-keyword boost (+30) already
		// credited that match; counting it a second time as a title
		// boost penalizes canonical short titles like "Edge Functions"
		// against off-canonical longer titles that redundantly repeat
		// the vendor name like "Consuming Supabase Queue Messages with
		// Edge Functions". Surfaced by Phase A1 regression on
		// `supabase edge functions` slipping rank 5→6.
		titleLower := strings.ToLower(strings.TrimSpace(c.hit.Title))
		if titleLower != "" && titleLower == qLower {
			c.hit.Score += searchTitleExactBoost
		}
		titleTokenMatches := 0
		pathTokenMatches := 0
		basenameTokenMatches := 0
		for _, tok := range qTokens {
			if !strings.Contains(titleLower, tok) {
				continue
			}
			if srcs, ok := sourceKeywords[tok]; ok && srcs[c.hit.Source] {
				continue
			}
			titleTokenMatches++
			c.hit.Score += searchTitleBoost
		}
		// Source-keyword boost: when the query mentions a vendor name,
		// boost candidates from the matching source dir.
		if len(intendedSources) > 0 && intendedSources[c.hit.Source] {
			c.hit.Score += searchSourceBoost
		}
		// Path-segment boost: query tokens appearing anywhere in the
		// path (after the source-dir prefix) get +searchPathBoost. Targets
		// canonical docs whose title is too short to qualify for title-
		// tier but whose URL clearly identifies them — e.g.
		// `slack/authentication/tokens.md` for "slack auth tokens"
		// (title is just "Tokens"; path has `/authentication/tokens.md`).
		// Source-keyword tokens skipped — already counted by source-boost.
		pathSansSource := strings.ToLower(c.hit.Path)
		if i := strings.IndexByte(pathSansSource, '/'); i > 0 {
			pathSansSource = pathSansSource[i+1:]
		}
		// Basename without .md/.mdx extension. Used by the basename-exact
		// boost below — distinct from substring path-segment match.
		basename := pathSansSource
		if i := strings.LastIndexByte(basename, '/'); i >= 0 {
			basename = basename[i+1:]
		}
		basename = strings.TrimSuffix(basename, ".md")
		basename = strings.TrimSuffix(basename, ".mdx")

		if c.hit.Source == "anthropic" && strings.HasPrefix(pathSansSource, "build-with-claude/") {
			c.hit.Score += searchAnthropicBuildGuideBoost
		}

		for _, tok := range qTokens {
			if _, isSource := sourceKeywords[tok]; isSource {
				continue
			}
			if strings.Contains(pathSansSource, tok) {
				pathTokenMatches++
				c.hit.Score += searchPathBoost
			}
			if strings.Contains(basename, tok) {
				basenameTokenMatches++
			}
			// Basename-exact match: the URL author chose this filename
			// to identify the topic. Targets `slack/authentication/
			// tokens.md` for "...tokens", `claude-code/skills.md` for
			// "...skills", etc. Skip synonym-class tokens — query "auth"
			// shouldn't trip a basename match on `auth.md` when the
			// canonical answer is `tokens.md` and the query meant the
			// authentication concept generally. Stem-equality (singular
			// vs plural) is checked separately so e.g. query "jwt"
			// matches basename "jwts" and "hyperloglog" matches
			// "hyperloglogs".
			if len(synonymsByToken[tok]) == 0 && basenameStemMatch(basename, tok) {
				c.hit.Score += searchBasenameExactBoost
			}
		}
		basenameTitle := strings.ReplaceAll(strings.ReplaceAll(basename, "-", " "), "_", " ")
		titleMatchesBasename := titleLower != "" && titleLower == basenameTitle
		if (titleTokenMatches >= 2 && basenameTokenMatches >= 2) ||
			(titleMatchesBasename && titleTokenMatches >= 1 && basenameTokenMatches >= 1) {
			c.hit.Score += searchTitleBasenameAlignmentBoost
		}
		if titleMatchesBasename && titleTokenMatches >= 1 && basenameTokenMatches >= 1 {
			c.tieBreak++
		}
		// Path-depth penalty: subtract per slash in pathSansSource so
		// hub docs aren't crowded out by their own sub-pages on hub
		// queries. See searchDepthPenaltyPerSegment / Cap doc comments.
		c.hit.Score += depthPenalty(pathSansSource)

		// Profile boost: lift in-stack candidates above equally-ranked
		// off-stack candidates. Sub-source matches get an extra lift so
		// scoped slices (e.g. azure/cli/**) outrank their non-scoped
		// neighbors inside the same source. The constants are
		// eval-tuned defaults; DOCS_PROFILE_BOOST / DOCS_PROFILE_SUB_BOOST
		// override at runtime for sweep workflows.
		if c.hit.InProfile {
			c.hit.Score += profileBoost
			if c.hit.InProfileSub {
				c.hit.Score += profileSubBoost
			}
		}
		cands = append(cands, c)
	}
	// Re-sort by adjusted score so the title bonus actually moves results.
	sort.SliceStable(cands, func(i, j int) bool {
		if cands[i].hit.Score != cands[j].hit.Score {
			return cands[i].hit.Score > cands[j].hit.Score
		}
		if cands[i].tieBreak != cands[j].tieBreak {
			return cands[i].tieBreak > cands[j].tieBreak
		}
		return cands[i].hit.Path < cands[j].hit.Path
	})
	if limit > 0 && len(cands) > limit {
		cands = cands[:limit]
	}
	hits := make([]searchHit, len(cands))
	for i, c := range cands {
		if includeSnippets {
			c.hit.Snippets = extractSnippetsFromText(c.body, qLower)
		} else {
			c.hit.Snippets = nil
		}
		hits[i] = c.hit
	}
	return hits, nil
}

// runTier executes one FTS5 tier query and merges results into candByPath.
// Existing entries (already pulled in by an earlier tier) keep their better
// score but get the fromTitle flag OR'd on, so the body-tier sees that the
// candidate also matched titles. Failures from a malformed `title:(...)`
// expression are logged and ignored — the body tier alone still gives a
// usable result, which is the right degradation for a Tier-1 search-quality
// optimization (no full-search outage if SQL syntax is off).
func (f *ftsIndex) runTier(candByPath map[string]*cand, sqlWithBody, sqlWithoutBody, q, source string, sqlLimit int, fromTitle, fromPath bool, includeBody bool) error {
	ctx := context.Background()
	sqlBase := sqlWithBody
	if !includeBody {
		sqlBase = sqlWithoutBody
	}
	var rows *sql.Rows
	var err error
	if source != "" {
		rows, err = f.db.QueryContext(ctx, sqlBase+` AND source = ? ORDER BY rank LIMIT ?`,
			q, source, sqlLimit)
	} else {
		rows, err = f.db.QueryContext(ctx, sqlBase+` ORDER BY rank LIMIT ?`, q, sqlLimit)
	}
	if err != nil {
		// Log title-tier / path-tier syntax errors but don't propagate; the
		// body tier still works on its own.
		if fromTitle || fromPath {
			label := "title-tier"
			if fromPath {
				label = "path-tier"
			}
			fmt.Fprintf(os.Stderr, "fts5: %s query failed (using body-tier only): %v\n", label, err)
			return nil
		}
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			h    searchHit
			body string
			rank float64
		)
		if includeBody {
			if err := rows.Scan(&h.Path, &h.Source, &h.Title, &h.URL, &body, &rank); err != nil {
				return err
			}
		} else {
			if err := rows.Scan(&h.Path, &h.Source, &h.Title, &h.URL, &rank); err != nil {
				return err
			}
		}
		// BM25 returns negative-magnitude scores; flip + scale to int.
		bm25Score := int(-rank * 10)
		if existing, ok := candByPath[h.Path]; ok {
			// Already collected — keep the better raw score, OR the flags on.
			if bm25Score > existing.hit.Score {
				existing.hit.Score = bm25Score
			}
			if fromTitle {
				existing.fromTitle = true
			}
			if fromPath {
				existing.fromPath = true
			}
			continue
		}
		h.Score = bm25Score
		candByPath[h.Path] = &cand{hit: h, body: body, fromTitle: fromTitle, fromPath: fromPath}
	}
	return rows.Err()
}

// ftsBuildQuery converts a freeform user query into an FTS5 expression.
// Each whitespace-separated token becomes a quoted phrase; tokens are joined
// with implicit AND. Stripping non-alphanumerics defends against injection
// of FTS5 operators (NEAR, OR, prefix *, etc.) and avoids syntax errors on
// queries containing punctuation.
//
// Tokens with known synonyms (see synonymClasses) expand to a parenthesized
// OR group so porter-stem mismatches like "spec" ↔ "specification" find
// their canonical doc.
//
// When `exact` is true, the whole input becomes one quoted FTS5 phrase
// (`"row level security"`) — adjacent and in order, no AND-of-disjoint-
// tokens, no synonym expansion. Useful when an agent knows the canonical
// phrasing and wants to avoid false positives from tokenize-and-AND.
func ftsBuildQuery(q string, exact bool) string {
	tokens := tokenizeForFTS(q)
	if len(tokens) == 0 {
		return ""
	}
	if exact {
		return `"` + strings.Join(tokens, " ") + `"`
	}
	tokens = rewriteFTSNaturalLanguageTokens(tokens)
	tokens = filterFTSStopWords(tokens)
	return joinFTSTokens(tokens)
}

// ftsBuildTitleQuery builds a `title:(...)` column-filter expression used
// by the title-tier search pass. When the user's query contains a known
// source-keyword token (e.g. "supabase", "azure"), we strip it from the
// title expression — canonical short-titled docs ("Storage", "Edge
// Functions", "Indexes") don't repeat the vendor name in their title and
// would fail title-tier qualification otherwise. The body-tier query
// keeps the source-keyword (drives the source-keyword score boost).
//
// Returns the title-only FTS5 expression and an inferred source name when
// stripping was applied to a query that uniquely identifies one source.
// The caller uses that source to constrain the title-tier query — without
// that constraint, stripping "sentry" from "sentry releases cli" would
// pull in every Slack changelog page whose title also has "cli" and
// crowd out the actual sentry doc. Inferred source is "" when the query
// has zero or multiple source keywords (no safe constraint).
//
// Edge cases:
//
//   - Empty input: returns "", "".
//   - Bare source name (e.g. "slack" alone, or "supabase azure" where every
//     token is a source keyword): titleQ falls back to ALL original tokens
//     and inferredSource is reset to "". This is intentional — better a wide
//     title-tier match than skipping it. The caller can still title-rank
//     "slack" against docs whose titles contain "slack".
//   - Exact mode with a partially-stripped query: `--exact "supabase storage"`
//     becomes `title:("storage")` for the title tier. Body tier still searches
//     for the full quoted phrase. Title-tier exactness applies to the
//     post-strip token set, not the user input verbatim.
func ftsBuildTitleQuery(q string, exact bool) (titleQ, inferredSource string) {
	tokens := tokenizeForFTS(q)
	if len(tokens) == 0 {
		return "", ""
	}
	if !exact {
		tokens = rewriteFTSNaturalLanguageTokens(tokens)
		tokens = filterFTSStopWords(tokens)
	}
	stripped, matchedSources := stripSourceIntentTokens(tokens)
	// Only constrain to a source when the query identifies exactly one.
	// "azure" → microsoft-learn (1 source) → safe to constrain.
	// (No multi-source keywords today; future-proofed against e.g.
	// "react" mapping to both react-native and react.dev.)
	if len(matchedSources) == 1 {
		for s := range matchedSources {
			inferredSource = s
		}
	}
	// If every token was a source keyword, fall back to all tokens — better
	// to do a wide title-tier match than to skip title-tier entirely.
	if len(stripped) == 0 {
		stripped = tokens
		inferredSource = "" // no stripping happened, no constraint needed
	}
	if exact {
		titleQ = `title:("` + strings.Join(stripped, " ") + `")`
		return
	}
	inner := joinFTSTokens(stripped)
	// Phrase-only fallback for phraseSynonyms tokens. The strict-AND form
	// requires every non-source token in the title; for queries like
	// "postgres recursive cte", `queries-with.md` (titled "WITH Queries
	// (Common Table Expressions)") fails because "recursive" isn't in the
	// title. Appending `OR "common table expression"` gives such docs a
	// second qualification path: any doc whose title contains the
	// canonical-phrase form qualifies for title-tier even without the
	// other query tokens. The base-score floor is the same; rerank still
	// orders the pool, so irrelevant docs that happen to contain the
	// phrase end up far below docs that match all tokens.
	phraseAlts := make([]string, 0, len(stripped))
	for _, t := range stripped {
		if phrase, ok := phraseSynonyms[t]; ok {
			phraseAlts = append(phraseAlts, `"`+phrase+`"`)
		}
	}
	if len(phraseAlts) > 0 {
		inner = "(" + inner + ") OR " + strings.Join(phraseAlts, " OR ")
	}
	titleQ = "title:(" + inner + ")"
	return
}

// ftsBuildPathQuery is the path-tier analog of ftsBuildTitleQuery. Builds a
// `path_tokens:(...)` column-filter expression matching every non-source
// token in the query against the indexed path-tokens column. Source-keyword
// stripping + inferred-source constraint work the same way — a path-encoded
// canonical like `chrome/docs/extensions/reference/manifest.md` qualifies
// for path-tier even when its short title "Manifest file format" fails
// title-tier. Same exact-mode and phrase-synonym semantics as title-tier;
// path_tokens is tokenized with porter+unicode61 so it splits on -/_/. and
// stems plurals just like title.
func ftsBuildPathQuery(q string, exact bool) (pathQ, inferredSource string) {
	tokens := tokenizeForFTS(q)
	if len(tokens) == 0 {
		return "", ""
	}
	if !exact {
		tokens = rewriteFTSNaturalLanguageTokens(tokens)
		tokens = filterFTSStopWords(tokens)
	}
	stripped, matchedSources := stripSourceIntentTokens(tokens)
	if len(matchedSources) == 1 {
		for s := range matchedSources {
			inferredSource = s
		}
	}
	if len(stripped) == 0 {
		stripped = tokens
		inferredSource = ""
	}
	if exact {
		pathQ = `path_tokens:("` + strings.Join(stripped, " ") + `")`
		return
	}
	inner := joinFTSTokens(stripped)
	phraseAlts := make([]string, 0, len(stripped))
	for _, t := range stripped {
		if phrase, ok := phraseSynonyms[t]; ok {
			phraseAlts = append(phraseAlts, `"`+phrase+`"`)
		}
	}
	if len(phraseAlts) > 0 {
		inner = "(" + inner + ") OR " + strings.Join(phraseAlts, " OR ")
	}
	pathQ = "path_tokens:(" + inner + ")"
	return
}

// tokenizeForFTS strips non-alphanumeric chars (defends against injection
// of FTS5 operators) and splits on whitespace. Hyphens and underscores are
// preserved because they're meaningful in CLI flags and identifiers.
func tokenizeForFTS(q string) []string {
	mapped := strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '-' {
			return r
		}
		return ' '
	}, q)
	raw := strings.Fields(mapped)
	out := make([]string, len(raw))
	for i, t := range raw {
		out[i] = strings.ToLower(t)
	}
	return out
}

func filterFTSStopWords(tokens []string) []string {
	filtered := make([]string, 0, len(tokens))
	for _, tok := range tokens {
		if ftsStopWords[tok] {
			continue
		}
		filtered = append(filtered, tok)
	}
	if len(filtered) == 0 {
		return tokens
	}
	return filtered
}

func rewriteFTSNaturalLanguageTokens(tokens []string) []string {
	if rewrite := naturalLanguageCanonicalRewrite(tokens); len(rewrite) > 0 {
		return rewrite
	}
	if !hasFTSToken(tokens, "inspect") || !hasFTSToken(tokens, "query") || !hasFTSToken(tokens, "plan") {
		return tokens
	}
	out := append([]string(nil), tokens...)
	for i, tok := range out {
		if tok == "inspect" {
			out[i] = "explain"
			break
		}
	}
	return out
}

func naturalLanguageCanonicalRewrite(tokens []string) []string {
	for _, rule := range naturalLanguageCanonicalQueries {
		if !hasAllFTSTokens(tokens, rule.all) {
			continue
		}
		if len(rule.oneOf) > 0 && !hasAnyFTSToken(tokens, rule.oneOf) {
			continue
		}
		return append([]string(nil), rule.rewrite...)
	}
	return nil
}

func hasAllFTSTokens(tokens []string, wants []string) bool {
	for _, want := range wants {
		if !hasFTSToken(tokens, want) {
			return false
		}
	}
	return true
}

func hasAnyFTSToken(tokens []string, wants []string) bool {
	for _, want := range wants {
		if hasFTSToken(tokens, want) {
			return true
		}
	}
	return false
}

func hasFTSToken(tokens []string, want string) bool {
	for _, tok := range tokens {
		if tok == want {
			return true
		}
	}
	return false
}

func ftsScoringTokens(q string, exact bool) []string {
	tokens := tokenizeForFTS(q)
	if exact {
		return tokens
	}
	tokens = rewriteFTSNaturalLanguageTokens(tokens)
	filtered := make([]string, 0, len(tokens))
	for _, tok := range tokens {
		if ftsStopWords[tok] && !keepFTSScoringStopWord(tok, tokens) {
			continue
		}
		filtered = append(filtered, tok)
	}
	if len(filtered) == 0 {
		return tokens
	}
	return filtered
}

func keepFTSScoringStopWord(tok string, all []string) bool {
	if tok != "use" {
		return false
	}
	for _, other := range all {
		if other == "tool" {
			// Generic in prose, but meaningful in API/doc names like
			// Anthropic "tool use". Keep it out of broad FTS body ANDs,
			// but preserve it for title/path scoring in that phrase.
			return true
		}
	}
	return false
}

var ftsStopWords = map[string]bool{
	"a": true, "an": true, "and": true, "are": true, "as": true,
	"at": true, "be": true, "been": true, "being": true, "by": true,
	"can": true, "could": true, "do": true, "does": true, "for": true,
	"from": true, "how": true, "i": true, "in": true, "into": true,
	"is": true, "me": true, "my": true, "of": true, "on": true,
	"define": true, "or": true, "project": true, "quick": true,
	"quickest": true, "run": true, "should": true, "the": true, "to": true,
	"up": true, "use": true, "way": true, "we": true, "what": true, "when": true,
	"where": true, "which": true, "why": true, "with": true, "would": true,
	"write": true, "you": true,
}

// joinFTSTokens converts a token list into an FTS5 boolean expression with
// synonym expansion. Tokens with synonyms emit a parenthesized OR group;
// AND is explicit when any token expanded (FTS5 implicit-AND doesn't work
// across paren boundaries — it errors `syntax error near "OR"`).
func joinFTSTokens(tokens []string) string {
	parts := make([]string, 0, len(tokens))
	hasGroup := false
	for _, tok := range tokens {
		expanded := expandSynonyms(tok)
		if strings.HasPrefix(expanded, "(") {
			hasGroup = true
		}
		parts = append(parts, expanded)
	}
	// FTS5's implicit-AND only works between bare terms; mixing in a
	// parenthesized OR-group requires explicit AND or it errors with
	// `syntax error near "OR"`. Use AND throughout when any token expanded.
	sep := " "
	if hasGroup {
		sep = " AND "
	}
	return strings.Join(parts, sep)
}

// depthPenalty returns a non-positive score adjustment proportional to how
// many directory segments separate the doc from its source root. Counts
// slashes in pathSansSource (the path with the source-dir prefix already
// stripped). Cap floors the worst case so genuinely-deep canonicals don't
// fall off a cliff. See searchDepthPenaltyPerSegment doc comment for the
// motivating chrome-extensions-manifest case.
func depthPenalty(pathSansSource string) int {
	segs := strings.Count(pathSansSource, "/")
	pen := segs * searchDepthPenaltyPerSegment
	if pen < searchDepthPenaltyCap {
		return searchDepthPenaltyCap
	}
	return pen
}

// basenameStemMatch reports whether the path basename and a query token
// match modulo a trailing plural `s` or `es`. Examples:
//
//	jwt    matches jwts
//	hyperloglog matches hyperloglogs
//	tokens matches tokens (exact)
//	policy matches policies (Y → IES rule, conservative — only
//	   handles consonant + Y → consonant + IES; covers "policies"/
//	   "categories" and skips false hits like "boy"/"buoys".)
//
// Calibrated to match what porter stemmer does for body matching, so
// basename-exact boost lands on the same docs as title/body matches.
// Strict enough to avoid false positives like token→tokenize. Doesn't
// modify the underlying values — caller still passes raw lowercase.
func basenameStemMatch(basename, tok string) bool {
	if basename == tok {
		return true
	}
	// Singular query → plural basename, or vice versa. Both forms tried.
	for _, pair := range [2][2]string{{basename, tok}, {tok, basename}} {
		long, short := pair[0], pair[1]
		if long == short+"s" || long == short+"es" {
			return true
		}
		// Y-IES rule: e.g. policy → policies.
		if strings.HasSuffix(long, "ies") && strings.HasSuffix(short, "y") &&
			long[:len(long)-3] == short[:len(short)-1] {
			return true
		}
	}
	return false
}

// expandSynonyms returns either a quoted token ("foo") or — when the token
// has token-level synonyms or an acronym-phrase expansion — a parenthesized
// OR group. Token-level alternatives come from synonymsByToken; an acronym
// phrase expansion (e.g. "rls" → "row level security") is appended as a
// quoted phrase, which FTS5 matches as adjacent-in-order. Already-lowercased
// input.
func expandSynonyms(tok string) string {
	tokenSyns, hasTokenSyns := synonymsByToken[tok]
	phrase, hasPhrase := phraseSynonyms[tok]
	if !hasTokenSyns && !hasPhrase {
		return `"` + tok + `"`
	}
	all := make([]string, 0, len(tokenSyns)+2)
	all = append(all, `"`+tok+`"`)
	for _, s := range tokenSyns {
		all = append(all, `"`+s+`"`)
	}
	if hasPhrase {
		all = append(all, `"`+phrase+`"`)
	}
	return "(" + strings.Join(all, " OR ") + ")"
}

// extractSnippetsFromText returns ranked line snippets from body. A line
// matches if it contains any token from qLower; lines are ranked by token
// density (more distinct tokens = higher rank). Ties broken by earliest line.
//
// maxN and snippetLen default to searchSnippetMax / searchSnippetLen when
// passed as 0 — agents can override via --snippets / --snippet-len flags.
func extractSnippetsFromText(body, qLower string) []searchSnippet {
	return extractSnippetsTuned(body, qLower, 0, 0)
}

func extractSnippetsTuned(body, qLower string, maxN, snippetLen int) []searchSnippet {
	if maxN <= 0 {
		maxN = searchSnippetMax
	}
	if snippetLen <= 0 {
		snippetLen = searchSnippetLen
	}
	tokens := strings.Fields(qLower)
	if len(tokens) == 0 {
		return nil
	}
	type cand struct {
		snip  searchSnippet
		score int
	}
	var cands []cand
	for i, line := range strings.Split(body, "\n") {
		lower := strings.ToLower(line)
		hits := 0
		for _, tok := range tokens {
			if strings.Contains(lower, tok) {
				hits++
			}
		}
		if hits == 0 {
			continue
		}
		snip := strings.TrimSpace(line)
		if len(snip) > snippetLen {
			snip = snip[:snippetLen-3] + "..."
		}
		cands = append(cands, cand{
			snip:  searchSnippet{Line: i + 1, Text: snip},
			score: hits,
		})
	}
	sort.SliceStable(cands, func(i, j int) bool {
		if cands[i].score != cands[j].score {
			return cands[i].score > cands[j].score
		}
		return cands[i].snip.Line < cands[j].snip.Line
	})
	if len(cands) > maxN {
		cands = cands[:maxN]
	}
	out := make([]searchSnippet, len(cands))
	for i, c := range cands {
		out[i] = c.snip
	}
	return out
}

// ftsIndexExists returns true if the search.db file is present AND has at
// least one indexed doc. An empty index would fall back to scan.
func ftsIndexExists(out string) bool {
	if _, err := os.Stat(ftsDBPath(out)); err != nil {
		return false
	}
	db, err := sql.Open("sqlite", ftsDBPath(out))
	if err != nil {
		return false
	}
	defer db.Close()
	n, err := ftsdb.New(db).CountDocs(context.Background())
	if err != nil {
		return false
	}
	return n > 0
}
