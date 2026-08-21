package main

import (
	"archive/tar"
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestValidatePublicURL(t *testing.T) {
	t.Parallel()

	got, err := validatePublicURL("https://go.dev/ref/spec#Package_unsafe", "go")
	if err != nil {
		t.Fatalf("validate allowed URL: %v", err)
	}
	if want := "https://go.dev/ref/spec"; got != want {
		t.Fatalf("canonical URL = %q, want %q", got, want)
	}

	tests := []struct {
		name   string
		value  string
		source string
	}{
		{name: "plain HTTP", value: "http://go.dev/ref/spec", source: "go"},
		{name: "credentials", value: "https://user@go.dev/ref/spec", source: "go"},
		{name: "explicit port", value: "https://go.dev:443/ref/spec", source: "go"},
		{name: "host suffix", value: "https://go.dev.example.com/ref/spec", source: "go"},
		{name: "wrong allowlist", value: "https://sqlite.org/fts5.html", source: "go"},
		{name: "unknown source", value: "https://go.dev/ref/spec", source: "private"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if _, err := validatePublicURL(test.value, test.source); err == nil {
				t.Fatalf("validatePublicURL(%q, %q) succeeded", test.value, test.source)
			}
		})
	}
}

func TestValidateDocumentPath(t *testing.T) {
	t.Parallel()

	if got, err := validateDocumentPath("sqlite/fts5.md", "sqlite"); err != nil || got != "sqlite/fts5.md" {
		t.Fatalf("valid document path = %q, %v", got, err)
	}

	invalid := []string{
		"",
		"/sqlite/fts5.md",
		"sqlite/../sqlite/fts5.md",
		"sqlite//fts5.md",
		"sqlite/./fts5.md",
		"sqlite\\fts5.md",
		"go/fts5.md",
		"sqlite/fts5.txt",
	}
	for _, value := range invalid {
		value := value
		t.Run(strings.ReplaceAll(value, "/", "_"), func(t *testing.T) {
			t.Parallel()
			if _, err := validateDocumentPath(value, "sqlite"); err == nil {
				t.Fatalf("validateDocumentPath(%q) succeeded", value)
			}
		})
	}
}

func TestVerifyLock(t *testing.T) {
	t.Parallel()

	reviewed := fixtureLock()
	current := cloneLock(reviewed)
	current.RetrievedAt = time.Now().UTC().Format(time.RFC3339Nano)
	if err := verifyLock(reviewed, current); err != nil {
		t.Fatalf("verify identical content: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*corpusLock)
	}{
		{name: "wrong identity", mutate: func(lock *corpusLock) { lock.CorpusID = "private" }},
		{name: "invalid timestamp", mutate: func(lock *corpusLock) { lock.RetrievedAt = "yesterday" }},
		{name: "wrong list length", mutate: func(lock *corpusLock) { lock.Documents = lock.Documents[:23] }},
		{name: "changed content", mutate: func(lock *corpusLock) { lock.Documents[0].SHA256 = "sha256:changed" }},
		{name: "changed index", mutate: func(lock *corpusLock) { lock.IndexDigest = "sha256:" + strings.Repeat("f", 64) }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			candidate := cloneLock(reviewed)
			test.mutate(&candidate)
			if err := verifyLock(candidate, cloneLock(current)); err == nil {
				t.Fatal("verifyLock succeeded for a changed lock")
			}
		})
	}
}

func TestTrackedCorpusLockIsSelfConsistent(t *testing.T) {
	t.Parallel()

	lock, err := readLock(filepath.Join("..", "..", "corpus.lock.json"))
	if err != nil {
		t.Fatalf("read tracked corpus lock: %v", err)
	}
	if err := validateReviewedLock(lock); err != nil {
		t.Fatalf("validate tracked corpus lock: %v", err)
	}
}

func TestTrackedSnapshotMatchesReviewedLock(t *testing.T) {
	t.Parallel()

	snapshotRoot := filepath.Join("..", "..", "snapshot")
	if err := validateCorpusTree(snapshotRoot); err != nil {
		t.Fatalf("validate tracked snapshot tree: %v", err)
	}
	current, err := buildLock(snapshotRoot, filepath.Join("..", "..", "..", "..", "eval", "sample-corpus", "sources.md"))
	if err != nil {
		t.Fatalf("build tracked snapshot lock: %v", err)
	}
	if err := validateExactCorpusFiles(snapshotRoot, current.Documents); err != nil {
		t.Fatalf("validate tracked snapshot files: %v", err)
	}
	reviewed, err := readLock(filepath.Join("..", "..", "corpus.lock.json"))
	if err != nil {
		t.Fatalf("read reviewed corpus lock: %v", err)
	}
	// This test proves the tracked document bytes. The separate two-index gate
	// proves and checks the reviewed index identity.
	current.IndexDigest = reviewed.IndexDigest
	if err := verifyLock(reviewed, current); err != nil {
		t.Fatalf("verify tracked snapshot against reviewed lock: %v", err)
	}
}

func TestDecodeJSONFileIsStrict(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		body string
	}{
		{name: "unknown field", body: `{"version":1,"entries":{},"private":true}`},
		{name: "trailing value", body: `{"version":1,"entries":{}} {}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "manifest.json")
			mustWriteFile(t, path, []byte(test.body), 0o644)
			if _, err := readSourceManifest(path); err == nil {
				t.Fatal("strict JSON decoder accepted malformed input")
			}
		})
	}
}

func TestValidateCorpusTreeRejectsUnreviewedContent(t *testing.T) {
	t.Parallel()

	t.Run("unknown root file", func(t *testing.T) {
		root := t.TempDir()
		mustWriteFile(t, filepath.Join(root, "secret.txt"), []byte("no"), 0o644)
		if err := validateCorpusTree(root); err == nil {
			t.Fatal("unknown root file was accepted")
		}
	})

	t.Run("unknown source directory", func(t *testing.T) {
		root := t.TempDir()
		if err := os.Mkdir(filepath.Join(root, "private"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := validateCorpusTree(root); err == nil {
			t.Fatal("unknown source directory was accepted")
		}
	})

	t.Run("nested symlink", func(t *testing.T) {
		root := t.TempDir()
		if err := os.Mkdir(filepath.Join(root, "go"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(root, "outside"), filepath.Join(root, "go", "escape.md")); err != nil {
			t.Skipf("symlinks are unavailable: %v", err)
		}
		if err := validateCorpusTree(root); err == nil {
			t.Fatal("nested symlink was accepted")
		}
	})
}

func TestValidateExactCorpusFilesRejectsExtraFile(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	document := lockedDocument{Path: "go/ref/spec.md"}
	mustWriteFile(t, filepath.Join(root, document.Path), []byte("spec"), 0o644)
	if err := validateExactCorpusFiles(root, []lockedDocument{document}); err != nil {
		t.Fatalf("validate exact corpus: %v", err)
	}
	mustWriteFile(t, filepath.Join(root, "go", "private.md"), []byte("private"), 0o644)
	if err := validateExactCorpusFiles(root, []lockedDocument{document}); err == nil {
		t.Fatal("extra document was accepted")
	}
}

func TestCheckpointAndVerifyIndex(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	dbPath := filepath.Join(root, ".cache", "search.db")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := db.ExecContext(ctx, "PRAGMA journal_mode=WAL"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, "CREATE TABLE docs (id INTEGER PRIMARY KEY)"); err != nil {
		t.Fatal(err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for id := 1; id <= expectedDocuments; id++ {
		if _, err := tx.ExecContext(ctx, "INSERT INTO docs(id) VALUES (?)", id); err != nil {
			_ = tx.Rollback()
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	if err := checkpointAndVerifyIndex(root); err != nil {
		t.Fatalf("checkpoint index: %v", err)
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(dbPath + suffix); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("SQLite sidecar %s remains: %v", suffix, err)
		}
	}
}

func TestCheckpointAndVerifyIndexRejectsWrongCount(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	dbPath := filepath.Join(root, ".cache", "search.db")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("CREATE TABLE docs (id INTEGER PRIMARY KEY)"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := checkpointAndVerifyIndex(root); err == nil || !strings.Contains(err.Error(), "want 24") {
		t.Fatalf("wrong document count error = %v", err)
	}
}

func TestStageBuildContextCopiesOnlyReviewedFiles(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	corpusRoot := filepath.Join(root, "corpus")
	lock := fixtureLock()
	lock.Documents = []lockedDocument{
		{Source: "go", Path: "go/ref/spec.md", URL: "https://go.dev/ref/spec", License: allowedSources["go"].License, SHA256: "sha256:" + strings.Repeat("1", 64), Bytes: 2},
		{Source: "postgresql", Path: "postgresql/sql-select.md", URL: "https://www.postgresql.org/docs/current/sql-select.html", License: allowedSources["postgresql"].License, SHA256: "sha256:" + strings.Repeat("2", 64), Bytes: 2},
		{Source: "sqlite", Path: "sqlite/fts5.md", URL: "https://sqlite.org/fts5.html", License: allowedSources["sqlite"].License, SHA256: "sha256:" + strings.Repeat("3", 64), Bytes: 2},
	}
	for _, source := range sortedSourceIDs() {
		mustWriteFile(t, filepath.Join(corpusRoot, source, "manifest.json"), []byte(`{"version":1,"entries":{"dynamic":{"fetched_at":"first-pull"}}}`+"\n"), 0o644)
	}
	for _, document := range lock.Documents {
		mustWriteFile(t, filepath.Join(corpusRoot, document.Path), []byte("ok"), 0o644)
	}
	mustWriteFile(t, filepath.Join(corpusRoot, ".cache", "search.db"), []byte("index"), 0o644)
	mustWriteFile(t, filepath.Join(corpusRoot, "go", "private.md"), []byte("do not stage"), 0o644)
	binaryPath := filepath.Join(root, "docs-puller-linux-amd64")
	dockerfilePath := filepath.Join(root, "Dockerfile")
	mustWriteFile(t, binaryPath, []byte("binary"), 0o755)
	mustWriteFile(t, dockerfilePath, []byte("FROM scratch\nADD __ROOTFS_ARCHIVE__ /\n"), 0o644)
	buildContext := filepath.Join(root, "deploy", "demo", ".build")
	t.Cleanup(func() {
		_ = makeDirectoriesWritableForRemoval(buildContext)
	})

	manifest, err := stageBuildContext(lock, corpusRoot, buildContext, binaryPath, dockerfilePath, "sha256:index")
	if err != nil {
		t.Fatalf("stage build context: %v", err)
	}
	if manifest.BinaryDigest == "" || manifest.RootFSDigest == "" || manifest.IndexDigest != "sha256:index" {
		t.Fatalf("stage manifest = %#v", manifest)
	}
	wantTime, err := time.Parse(time.RFC3339Nano, lock.RetrievedAt)
	if err != nil {
		t.Fatal(err)
	}
	wantArchive, err := rootFSArchiveName(manifest.RootFSDigest)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.RootFSArchive != wantArchive {
		t.Fatalf("root filesystem archive = %q, want %q", manifest.RootFSArchive, wantArchive)
	}
	names := readTarNames(t, filepath.Join(buildContext, manifest.RootFSArchive), wantTime)
	if !names["usr/local/bin/docs-puller"] || !names["app/docs/go/ref/spec.md"] {
		t.Fatalf("root filesystem entries = %#v", names)
	}
	if names["app/docs/go/private.md"] {
		t.Fatal("unreviewed file was staged in the root filesystem")
	}
	if _, err := os.Stat(filepath.Join(buildContext, "build-manifest.json")); err != nil {
		t.Fatalf("build manifest is missing: %v", err)
	}
	info, err := os.Stat(filepath.Join(buildContext, "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	if !info.ModTime().Equal(wantTime) {
		t.Fatalf("normalized mtime = %s, want %s", info.ModTime(), wantTime)
	}
	dockerfile, err := os.ReadFile(filepath.Join(buildContext, "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(dockerfile, []byte(dockerfileMarker)) || !bytes.Contains(dockerfile, []byte("ADD "+manifest.RootFSArchive+" /")) {
		t.Fatalf("staged Dockerfile does not bind the content-addressed archive:\n%s", dockerfile)
	}
	for _, source := range sortedSourceIDs() {
		mustWriteFile(t, filepath.Join(corpusRoot, source, "manifest.json"), []byte(`{"version":1,"entries":{"dynamic":{"fetched_at":"second-pull"}}}`+"\n"), 0o644)
	}
	second, err := stageBuildContext(lock, corpusRoot, buildContext, binaryPath, dockerfilePath, "sha256:index")
	if err != nil {
		t.Fatalf("replace immutable build context: %v", err)
	}
	if second.RootFSDigest != manifest.RootFSDigest {
		t.Fatalf("root filesystem digest changed: %s != %s", second.RootFSDigest, manifest.RootFSDigest)
	}
	if second.RootFSArchive != manifest.RootFSArchive {
		t.Fatalf("root filesystem archive changed: %s != %s", second.RootFSArchive, manifest.RootFSArchive)
	}
	if err := os.WriteFile(binaryPath, []byte("BINARY"), 0o755); err != nil {
		t.Fatal(err)
	}
	third, err := stageBuildContext(lock, corpusRoot, buildContext, binaryPath, dockerfilePath, "sha256:index")
	if err != nil {
		t.Fatalf("stage changed same-size binary: %v", err)
	}
	if third.RootFSArchive == manifest.RootFSArchive {
		t.Fatalf("same-size binary change reused archive %s", third.RootFSArchive)
	}
}

func readTarNames(t *testing.T, path string, wantTime time.Time) map[string]bool {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := file.Close(); err != nil {
			t.Errorf("close tar: %v", err)
		}
	}()
	reader := tar.NewReader(file)
	names := make(map[string]bool)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names[header.Name] = true
		if header.Uid != 65532 || header.Gid != 65532 {
			t.Fatalf("%s ownership = %d:%d", header.Name, header.Uid, header.Gid)
		}
		if !header.ModTime.Equal(wantTime) {
			t.Fatalf("%s mtime = %s, want %s", header.Name, header.ModTime, wantTime)
		}
	}
	return names
}

func TestValidateBuildContext(t *testing.T) {
	t.Parallel()

	valid := filepath.Join(t.TempDir(), "deploy", "demo", ".build")
	if err := validateBuildContext(valid); err != nil {
		t.Fatalf("valid context: %v", err)
	}
	for _, value := range []string{"/", t.TempDir(), filepath.Join(t.TempDir(), ".build")} {
		if err := validateBuildContext(value); err == nil {
			t.Fatalf("unsafe build context %q was accepted", value)
		}
	}
}

func TestDigestDocumentsIsOrderSensitiveAndStable(t *testing.T) {
	t.Parallel()

	documents := []lockedDocument{
		{Path: "go/a.md", SHA256: "sha256:a", Bytes: 1, URL: "https://go.dev/a", License: "license"},
		{Path: "go/b.md", SHA256: "sha256:b", Bytes: 2, URL: "https://go.dev/b", License: "license"},
	}
	first := digestDocuments("sha256:list", documents)
	second := digestDocuments("sha256:list", documents)
	if first != second || !strings.HasPrefix(first, "sha256:") || len(first) != len("sha256:")+64 {
		t.Fatalf("unstable digest: %q / %q", first, second)
	}
	documents[0], documents[1] = documents[1], documents[0]
	if got := digestDocuments("sha256:list", documents); got == first {
		t.Fatal("document order did not affect the digest")
	}
}

func fixtureLock() corpusLock {
	documents := make([]lockedDocument, expectedDocuments)
	sourceListDigest := "sha256:" + strings.Repeat("a", 64)
	sources := sortedSourceIDs()
	hosts := map[string]string{
		"go":         "https://go.dev/",
		"postgresql": "https://www.postgresql.org/",
		"sqlite":     "https://sqlite.org/",
	}
	for index := range documents {
		source := sources[index/(expectedDocuments/expectedSources)]
		documents[index] = lockedDocument{
			Source:  source,
			Path:    fmt.Sprintf("%s/doc-%02d.md", source, index),
			URL:     fmt.Sprintf("%sdoc-%02d", hosts[source], index),
			License: allowedSources[source].License,
			SHA256:  fmt.Sprintf("sha256:%064x", index+1),
			Bytes:   int64(index + 1),
		}
	}
	return corpusLock{
		SchemaVersion:    lockSchemaVersion,
		CorpusID:         corpusID,
		RetrievedAt:      "2026-08-19T01:06:48Z",
		SourceDateEpoch:  1787101608,
		SourceListDigest: sourceListDigest,
		IndexDigest:      "sha256:" + strings.Repeat("b", 64),
		CorpusDigest:     digestDocuments(sourceListDigest, documents),
		DocumentCount:    expectedDocuments,
		SourceCount:      expectedSources,
		Documents:        documents,
	}
}

func cloneLock(lock corpusLock) corpusLock {
	lock.Documents = append([]lockedDocument(nil), lock.Documents...)
	return lock
}

func mustWriteFile(t *testing.T, path string, data []byte, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, mode); err != nil {
		t.Fatal(err)
	}
}
