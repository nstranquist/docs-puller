package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCollectDocsStatusReady(t *testing.T) {
	out := t.TempDir()
	now := time.Now().UTC().Format(time.RFC3339)

	writeFTSDoc(t, filepath.Join(out, "supabase"), "guides/rls.md", "# Row Level Security\n")
	writeFTSDoc(t, filepath.Join(out, "kb"), "apps/acme/docs-puller.md", "# docs-puller\n")
	if err := writeManifestAtomic(filepath.Join(out, "supabase"), manifest{
		Version: manifestVersion,
		Entries: map[string]result{
			"https://supabase.example/rls": {
				URL:       "https://supabase.example/rls",
				Source:    "supabase",
				Path:      "supabase/guides/rls.md",
				FetchedAt: now,
			},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := writeManifestAtomic(filepath.Join(out, "kb"), manifest{
		Version: manifestVersion,
		Entries: map[string]result{
			"file:///kb/docs-puller.md": {
				URL:       "file:///kb/docs-puller.md",
				Source:    "kb",
				Path:      "kb/apps/acme/docs-puller.md",
				FetchedAt: now,
			},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := appendIngestLog(out, logEntry{
		StartedAt: now, FinishedAt: now, Mode: "local",
		Sources: []string{"kb", "supabase"}, URLs: 2, Pulled: 2,
	}); err != nil {
		t.Fatal(err)
	}
	idx, err := openFTSIndex(out)
	if err != nil {
		t.Fatal(err)
	}
	if err := idx.rebuild(out); err != nil {
		t.Fatal(err)
	}
	idx.close()
	embDB, err := openEmbeddingsDB(out, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := embDB.Exec(
		"INSERT INTO embeddings (path, mtime_ns, model, dim, vec) VALUES (?, ?, ?, ?, ?)",
		"supabase/guides/rls.md", 0, "test-model", 2, serializeVec([]float32{1, 0}),
	); err != nil {
		t.Fatal(err)
	}
	if err := writeFlatEmbeddingIndex(embDB, out, "test-model"); err != nil {
		t.Fatal(err)
	}
	embDB.Close()

	got, err := collectDocsStatus(out, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if got.SourceCount != 2 || got.TotalDocs != 2 || got.IndexableDocs != 2 {
		t.Fatalf("source/doc counts = %d/%d/%d, want 2/2/2", got.SourceCount, got.TotalDocs, got.IndexableDocs)
	}
	if !got.FTS.Ready || got.FTS.Docs != 2 || !got.FTS.MatchesCorpus {
		t.Fatalf("unexpected FTS status: %+v", got.FTS)
	}
	if !got.Embeddings.Exists || len(got.Embeddings.Models) != 1 || got.Embeddings.Models[0].Docs != 1 {
		t.Fatalf("unexpected embeddings status: %+v", got.Embeddings)
	}
	if len(got.Embeddings.FlatIndexes) != 1 || !got.Embeddings.FlatIndexes[0].Exists {
		t.Fatalf("unexpected flat index status: %+v", got.Embeddings.FlatIndexes)
	}
	if !got.IngestLog.Exists || got.IngestLog.Entries != 1 || got.IngestLog.Last == nil {
		t.Fatalf("unexpected ingest log status: %+v", got.IngestLog)
	}
	if len(got.Warnings) != 0 {
		t.Fatalf("expected no warnings, got %v", got.Warnings)
	}
}

func TestCollectDocsStatusReportsOperationalState(t *testing.T) {
	out := t.TempDir()
	src := filepath.Join(out, "kb")
	writeFTSDoc(t, src, "keep.md", "# Keep\n\ncanonical alpha content\n")
	writeFTSDoc(t, src, "short.md", "# Short\n\nselector missed content\n")
	m := newManifest()
	m.Entries["https://example.com/keep"] = result{URL: "https://example.com/keep", Source: "kb", Path: "kb/keep.md", FetchedAt: "2026-05-01T00:00:00Z"}
	m.Entries["https://example.com/short"] = result{URL: "https://example.com/short", Source: "kb", Path: "kb/short.md", FetchedAt: "2026-04-30T00:00:00Z", Warning: "low-content (31 bytes)"}
	if err := writeManifestAtomic(src, m); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(out, ".profile"), []byte("acme\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	idx, err := openFTSIndex(out)
	if err != nil {
		t.Fatal(err)
	}
	defer idx.close()
	if err := idx.rebuild(out); err != nil {
		t.Fatal(err)
	}

	status, err := collectDocsStatus(out, 0)
	if err != nil {
		t.Fatal(err)
	}
	if status.SourceCount != 1 || status.TotalDocs != 2 || status.IndexableDocs != 1 {
		t.Fatalf("counts = sources:%d total:%d indexable:%d, want 1/2/1", status.SourceCount, status.TotalDocs, status.IndexableDocs)
	}
	if !status.FTS.Exists || !status.FTS.Ready || status.FTS.Docs != 1 || !status.FTS.MatchesCorpus {
		t.Fatalf("fts status = %+v, want ready 1-doc index matching indexable corpus", status.FTS)
	}
	if status.Profile.Name != "acme" || status.Profile.Reason != "out-pin" {
		t.Fatalf("profile = %+v, want acme/out-pin", status.Profile)
	}
	if status.LastPullNewest != "2026-05-01T00:00:00Z" || status.LastPullOldest != "2026-04-30T00:00:00Z" {
		t.Fatalf("last pulls newest/oldest = %q/%q", status.LastPullNewest, status.LastPullOldest)
	}
}

func TestCollectDocsStatusIndexableDocsMatchesFTSWhitespaceOnlyDocs(t *testing.T) {
	out := t.TempDir()
	src := filepath.Join(out, "kb")
	writeFTSDoc(t, src, "keep.md", "# Keep\n\ncanonical alpha content\n")
	writeFTSDoc(t, src, "blank.md", "\n")

	idx, err := openFTSIndex(out)
	if err != nil {
		t.Fatal(err)
	}
	defer idx.close()
	if err := idx.rebuild(out); err != nil {
		t.Fatal(err)
	}

	status, err := collectDocsStatus(out, 0)
	if err != nil {
		t.Fatal(err)
	}
	if status.TotalDocs != 2 || status.IndexableDocs != 1 {
		t.Fatalf("doc counts = total:%d indexable:%d, want 2/1", status.TotalDocs, status.IndexableDocs)
	}
	if !status.FTS.MatchesCorpus || status.FTS.Docs != 1 {
		t.Fatalf("fts status = %+v, want 1-doc index matching indexable corpus", status.FTS)
	}
	if warningContains(status.Warnings, "FTS5 index has") {
		t.Fatalf("unexpected FTS mismatch warning in %v", status.Warnings)
	}
}

func TestCollectDocsStatusWarnsWhenIndexMissing(t *testing.T) {
	out := t.TempDir()
	writeFTSDoc(t, filepath.Join(out, "supabase"), "guides/rls.md", "# Row Level Security\n")

	got, err := collectDocsStatus(out, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !warningContains(got.Warnings, "FTS5 index missing") {
		t.Fatalf("missing FTS warning in %v", got.Warnings)
	}
	if !warningContains(got.Warnings, "no ingest history") {
		t.Fatalf("missing ingest-log warning in %v", got.Warnings)
	}
}

func TestCollectDocsStatusWarnsOnStaleSource(t *testing.T) {
	out := t.TempDir()
	old := time.Now().UTC().Add(-48 * time.Hour).Format(time.RFC3339)
	writeFTSDoc(t, filepath.Join(out, "supabase"), "guides/rls.md", "# Row Level Security\n")
	if err := writeManifestAtomic(filepath.Join(out, "supabase"), manifest{
		Version: manifestVersion,
		Entries: map[string]result{
			"https://supabase.example/rls": {
				URL:       "https://supabase.example/rls",
				Source:    "supabase",
				Path:      "supabase/guides/rls.md",
				FetchedAt: old,
			},
		},
	}); err != nil {
		t.Fatal(err)
	}

	got, err := collectDocsStatus(out, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.StaleSources) != 1 || got.StaleSources[0].Name != "supabase" {
		t.Fatalf("unexpected stale sources: %+v", got.StaleSources)
	}
	if !warningContains(got.Warnings, "source supabase last pulled") {
		t.Fatalf("missing stale warning in %v", got.Warnings)
	}
}

func TestBuildStatusWarningsWarnsWhenFlatIndexMissing(t *testing.T) {
	status := docsStatus{
		Embeddings: statusEmbeddings{
			Exists: true,
			Models: []statusEmbeddingModel{
				{Model: "test-model", Docs: 2, Dim: 2},
			},
		},
	}
	if !warningContains(buildStatusWarnings(status), "flat embedding index missing for model test-model") {
		t.Fatalf("missing flat-index warning")
	}
}

func TestBuildStatusWarningsWarnsWhenFlatIndexStale(t *testing.T) {
	status := docsStatus{
		Embeddings: statusEmbeddings{
			Exists: true,
			Models: []statusEmbeddingModel{
				{Model: "test-model", Docs: 2, Dim: 2},
			},
			FlatIndexes: []statusFlatIndex{
				{Model: "test-model", Exists: true, Count: 1, Dim: 2},
			},
		},
	}
	if !warningContains(buildStatusWarnings(status), "flat embedding index stale for model test-model") {
		t.Fatalf("missing stale flat-index warning")
	}
}

func TestBuildStatusCheckWarningsIgnoresFlatEmbeddingWarningsByDefault(t *testing.T) {
	status := checkReadyStatus()
	status.Embeddings = statusEmbeddings{
		Exists: true,
		Models: []statusEmbeddingModel{
			{Model: "test-model", Docs: 2, Dim: 2},
		},
	}
	if warningContains(buildStatusWarnings(status), "flat embedding index missing for model test-model") == false {
		t.Fatalf("status output warnings should still include flat-index warning")
	}
	if got := buildStatusCheckWarnings(status, false); len(got) != 0 {
		t.Fatalf("default check warnings = %v, want none", got)
	}
}

func TestBuildStatusCheckWarningsIncludesFlatEmbeddingWarningsWhenRequested(t *testing.T) {
	status := checkReadyStatus()
	status.Embeddings = statusEmbeddings{
		Exists: true,
		Models: []statusEmbeddingModel{
			{Model: "test-model", Docs: 2, Dim: 2},
		},
	}
	if !warningContains(buildStatusCheckWarnings(status, true), "flat embedding index missing for model test-model") {
		t.Fatalf("strict embedding check should include flat-index warning")
	}
}

func checkReadyStatus() docsStatus {
	return docsStatus{
		Out:           "/tmp/docs",
		SourceCount:   1,
		IndexableDocs: 1,
		FTS: statusFTS{
			Path:          "/tmp/docs/.cache/search.db",
			Exists:        true,
			Ready:         true,
			Docs:          1,
			MatchesCorpus: true,
		},
		IngestLog: statusIngestLog{
			Path:    "/tmp/docs/.cache/ingest-log.jsonl",
			Exists:  true,
			Entries: 1,
		},
	}
}

func warningContains(warnings []string, needle string) bool {
	for _, w := range warnings {
		if strings.Contains(w, needle) {
			return true
		}
	}
	return false
}
