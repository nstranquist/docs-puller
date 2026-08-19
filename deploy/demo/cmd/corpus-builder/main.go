// Command corpus-builder verifies and stages the immutable public demo corpus.
// It never copies a file that is not named by the reviewed lock.
package main

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	pathpkg "path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const (
	lockSchemaVersion  = 1
	stageSchemaVersion = 3
	dockerfileMarker   = "__ROOTFS_ARCHIVE__"
	corpusID           = "public-sample-v1"
	expectedDocuments  = 24
	expectedSources    = 3
	maxDocumentBytes   = 2 << 20
	maxCorpusBytes     = 16 << 20
)

var allowedSources = map[string]sourcePolicy{
	"sqlite": {
		Hosts:   map[string]bool{"sqlite.org": true, "www.sqlite.org": true},
		License: "SQLite documentation is in the public domain",
	},
	"go": {
		Hosts:   map[string]bool{"go.dev": true},
		License: "CC BY 4.0 website content, unless noted otherwise",
	},
	"postgresql": {
		Hosts:   map[string]bool{"postgresql.org": true, "www.postgresql.org": true},
		License: "PostgreSQL documentation is distributed under the PostgreSQL License",
	},
}

type sourcePolicy struct {
	Hosts   map[string]bool
	License string
}

type corpusLock struct {
	SchemaVersion    int              `json:"schema_version"`
	CorpusID         string           `json:"corpus_id"`
	RetrievedAt      string           `json:"retrieved_at"`
	SourceDateEpoch  int64            `json:"source_date_epoch"`
	SourceListDigest string           `json:"source_list_digest"`
	CorpusDigest     string           `json:"corpus_digest"`
	DocumentCount    int              `json:"document_count"`
	SourceCount      int              `json:"source_count"`
	Documents        []lockedDocument `json:"documents"`
}

type lockedDocument struct {
	Source  string `json:"source"`
	Path    string `json:"path"`
	URL     string `json:"url"`
	License string `json:"license"`
	SHA256  string `json:"sha256"`
	Bytes   int64  `json:"bytes"`
}

type sourceManifest struct {
	Version int                    `json:"version"`
	Entries map[string]sourceEntry `json:"entries"`
}

type sourceEntry struct {
	URL       string `json:"url"`
	Source    string `json:"source"`
	Path      string `json:"path"`
	Mode      string `json:"mode"`
	SHA256    string `json:"sha256"`
	FetchedAt string `json:"fetched_at"`
}

type stageManifest struct {
	SchemaVersion   int              `json:"schema_version"`
	CorpusID        string           `json:"corpus_id"`
	RetrievedAt     string           `json:"retrieved_at"`
	SourceDateEpoch int64            `json:"source_date_epoch"`
	CorpusDigest    string           `json:"corpus_digest"`
	IndexDigest     string           `json:"index_digest"`
	BinaryDigest    string           `json:"binary_digest"`
	RootFSDigest    string           `json:"rootfs_digest,omitempty"`
	RootFSArchive   string           `json:"rootfs_archive,omitempty"`
	DocumentCount   int              `json:"document_count"`
	SourceCount     int              `json:"source_count"`
	Documents       []lockedDocument `json:"documents"`
}

type commandResult struct {
	OK             bool   `json:"ok"`
	CorpusID       string `json:"corpus_id"`
	CorpusDigest   string `json:"corpus_digest"`
	IndexDigest    string `json:"index_digest"`
	RootFSDigest   string `json:"rootfs_digest,omitempty"`
	RootFSArchive  string `json:"rootfs_archive,omitempty"`
	DocumentCount  int    `json:"document_count"`
	SourceCount    int    `json:"source_count"`
	BuildContext   string `json:"build_context,omitempty"`
	LockWasWritten bool   `json:"lock_was_written"`
}

func main() {
	var corpusRoot string
	var sourceList string
	var lockPath string
	var writeLock bool
	var buildContext string
	var binaryPath string
	var dockerfilePath string

	flag.StringVar(&corpusRoot, "corpus", "", "path to the pulled and indexed public corpus")
	flag.StringVar(&sourceList, "sources", "eval/sample-corpus/sources.md", "reviewed public URL list")
	flag.StringVar(&lockPath, "lock", "deploy/demo/corpus.lock.json", "reviewed content lock")
	flag.BoolVar(&writeLock, "write-lock", false, "write a new lock from the current corpus")
	flag.StringVar(&buildContext, "build-context", "", "optional deploy/demo/.build output")
	flag.StringVar(&binaryPath, "binary", "", "linux/amd64 docs-puller binary for staging")
	flag.StringVar(&dockerfilePath, "dockerfile", "deploy/demo/Dockerfile", "pinned runtime Dockerfile")
	flag.Parse()

	if flag.NArg() != 0 || strings.TrimSpace(corpusRoot) == "" {
		fatalf("usage: corpus-builder --corpus DIR [--write-lock] [--build-context deploy/demo/.build --binary FILE]")
	}

	if err := run(corpusRoot, sourceList, lockPath, writeLock, buildContext, binaryPath, dockerfilePath); err != nil {
		fatalf("%v", err)
	}
}

func run(corpusRoot, sourceList, lockPath string, writeLock bool, buildContext, binaryPath, dockerfilePath string) error {
	if err := validateCorpusTree(corpusRoot); err != nil {
		return err
	}

	current, err := buildLock(corpusRoot, sourceList)
	if err != nil {
		return err
	}
	if err := validateExactCorpusFiles(corpusRoot, current.Documents); err != nil {
		return err
	}
	if writeLock {
		if err := writeJSONAtomic(lockPath, current, 0o644); err != nil {
			return fmt.Errorf("write corpus lock: %w", err)
		}
	}

	reviewed, err := readLock(lockPath)
	if err != nil {
		return err
	}
	if err := verifyLock(reviewed, current); err != nil {
		return err
	}
	if err := checkpointAndVerifyIndex(corpusRoot); err != nil {
		return err
	}

	indexDigest, _, err := fileDigest(filepath.Join(corpusRoot, ".cache", "search.db"))
	if err != nil {
		return fmt.Errorf("digest search index: %w", err)
	}
	result := commandResult{
		OK:             true,
		CorpusID:       reviewed.CorpusID,
		CorpusDigest:   reviewed.CorpusDigest,
		IndexDigest:    indexDigest,
		DocumentCount:  reviewed.DocumentCount,
		SourceCount:    reviewed.SourceCount,
		LockWasWritten: writeLock,
	}
	if buildContext != "" {
		manifest, err := stageBuildContext(reviewed, corpusRoot, buildContext, binaryPath, dockerfilePath, indexDigest)
		if err != nil {
			return err
		}
		result.BuildContext = buildContext
		result.IndexDigest = manifest.IndexDigest
		result.RootFSDigest = manifest.RootFSDigest
		result.RootFSArchive = manifest.RootFSArchive
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(result)
}

func buildLock(corpusRoot, sourceList string) (corpusLock, error) {
	listedURLs, sourceListDigest, err := readSourceList(sourceList)
	if err != nil {
		return corpusLock{}, err
	}
	seenURLs := make(map[string]bool, len(listedURLs))
	documents := make([]lockedDocument, 0, expectedDocuments)
	latestFetch := time.Time{}
	var totalBytes int64

	for _, source := range sortedSourceIDs() {
		manifestPath := filepath.Join(corpusRoot, source, "manifest.json")
		manifest, err := readSourceManifest(manifestPath)
		if err != nil {
			return corpusLock{}, err
		}
		if manifest.Version != 1 {
			return corpusLock{}, fmt.Errorf("%s: unsupported manifest version %d", source, manifest.Version)
		}
		for manifestURL, entry := range manifest.Entries {
			if manifestURL != entry.URL {
				return corpusLock{}, fmt.Errorf("%s: manifest key does not match entry URL", entry.Path)
			}
			if entry.Source != source || entry.Mode != "http" {
				return corpusLock{}, fmt.Errorf("%s: invalid source or mode for %s", source, entry.Path)
			}
			canonicalURL, err := validatePublicURL(entry.URL, source)
			if err != nil {
				return corpusLock{}, err
			}
			if !listedURLs[canonicalURL] {
				return corpusLock{}, fmt.Errorf("%s: URL is not in the reviewed source list", canonicalURL)
			}
			if seenURLs[canonicalURL] {
				return corpusLock{}, fmt.Errorf("%s: duplicate manifest URL", canonicalURL)
			}
			seenURLs[canonicalURL] = true

			relPath, err := validateDocumentPath(entry.Path, source)
			if err != nil {
				return corpusLock{}, err
			}
			fullPath := filepath.Join(corpusRoot, filepath.FromSlash(relPath))
			digest, size, err := regularFileDigest(fullPath)
			if err != nil {
				return corpusLock{}, err
			}
			if size > maxDocumentBytes {
				return corpusLock{}, fmt.Errorf("%s: document exceeds %d bytes", relPath, maxDocumentBytes)
			}
			if strings.TrimPrefix(digest, "sha256:") != entry.SHA256 {
				return corpusLock{}, fmt.Errorf("%s: manifest digest does not match file", relPath)
			}
			fetchedAt, err := time.Parse(time.RFC3339, entry.FetchedAt)
			if err != nil {
				return corpusLock{}, fmt.Errorf("%s: invalid fetched_at: %w", relPath, err)
			}
			if fetchedAt.After(latestFetch) {
				latestFetch = fetchedAt
			}
			totalBytes += size
			documents = append(documents, lockedDocument{
				Source:  source,
				Path:    relPath,
				URL:     canonicalURL,
				License: allowedSources[source].License,
				SHA256:  digest,
				Bytes:   size,
			})
		}
	}

	if len(documents) != expectedDocuments || len(seenURLs) != len(listedURLs) {
		return corpusLock{}, fmt.Errorf("corpus has %d documents and %d reviewed URLs; want %d", len(documents), len(seenURLs), expectedDocuments)
	}
	if totalBytes > maxCorpusBytes {
		return corpusLock{}, fmt.Errorf("corpus documents use %d bytes; limit is %d", totalBytes, maxCorpusBytes)
	}
	for listedURL := range listedURLs {
		if !seenURLs[listedURL] {
			return corpusLock{}, fmt.Errorf("%s: reviewed URL is missing from manifests", listedURL)
		}
	}
	sort.Slice(documents, func(i, j int) bool { return documents[i].Path < documents[j].Path })
	lock := corpusLock{
		SchemaVersion:    lockSchemaVersion,
		CorpusID:         corpusID,
		RetrievedAt:      latestFetch.UTC().Format(time.RFC3339Nano),
		SourceDateEpoch:  latestFetch.Unix(),
		SourceListDigest: sourceListDigest,
		DocumentCount:    len(documents),
		SourceCount:      expectedSources,
		Documents:        documents,
	}
	lock.CorpusDigest = digestDocuments(lock.SourceListDigest, documents)
	return lock, nil
}

func verifyLock(reviewed, current corpusLock) error {
	if err := validateReviewedLock(reviewed); err != nil {
		return err
	}
	current.RetrievedAt = reviewed.RetrievedAt
	current.SourceDateEpoch = reviewed.SourceDateEpoch
	if !locksEqual(reviewed, current) {
		return fmt.Errorf("corpus content differs from deploy/demo/corpus.lock.json; refresh requires review")
	}
	return nil
}

func validateReviewedLock(reviewed corpusLock) error {
	if reviewed.SchemaVersion != lockSchemaVersion || reviewed.CorpusID != corpusID {
		return fmt.Errorf("reviewed corpus lock has an unsupported identity")
	}
	if reviewed.DocumentCount != expectedDocuments || reviewed.SourceCount != expectedSources {
		return fmt.Errorf("reviewed corpus lock has %d documents and %d sources", reviewed.DocumentCount, reviewed.SourceCount)
	}
	if len(reviewed.Documents) != reviewed.DocumentCount {
		return fmt.Errorf("reviewed corpus lock declares %d documents but lists %d", reviewed.DocumentCount, len(reviewed.Documents))
	}
	retrievedAt, err := time.Parse(time.RFC3339Nano, reviewed.RetrievedAt)
	if err != nil {
		return fmt.Errorf("reviewed corpus lock has an invalid retrieved_at: %w", err)
	}
	if reviewed.SourceDateEpoch != retrievedAt.Unix() {
		return fmt.Errorf("reviewed corpus lock source_date_epoch does not match retrieved_at")
	}
	if err := validateSHA256(reviewed.SourceListDigest, "source_list_digest"); err != nil {
		return err
	}
	if err := validateSHA256(reviewed.CorpusDigest, "corpus_digest"); err != nil {
		return err
	}

	sources := make(map[string]bool, expectedSources)
	var totalBytes int64
	previousPath := ""
	for _, document := range reviewed.Documents {
		policy, ok := allowedSources[document.Source]
		if !ok {
			return fmt.Errorf("reviewed corpus lock contains unknown source %q", document.Source)
		}
		path, err := validateDocumentPath(document.Path, document.Source)
		if err != nil {
			return err
		}
		if previousPath != "" && path <= previousPath {
			return fmt.Errorf("reviewed corpus lock paths are not strictly sorted")
		}
		previousPath = path
		canonicalURL, err := validatePublicURL(document.URL, document.Source)
		if err != nil {
			return err
		}
		if canonicalURL != document.URL {
			return fmt.Errorf("%s: reviewed URL is not canonical", document.URL)
		}
		if document.License != policy.License {
			return fmt.Errorf("%s: reviewed license does not match the source policy", document.Path)
		}
		if err := validateSHA256(document.SHA256, document.Path+" sha256"); err != nil {
			return err
		}
		if document.Bytes <= 0 || document.Bytes > maxDocumentBytes {
			return fmt.Errorf("%s: reviewed size %d is outside the allowed range", document.Path, document.Bytes)
		}
		totalBytes += document.Bytes
		sources[document.Source] = true
	}
	if len(sources) != expectedSources {
		return fmt.Errorf("reviewed corpus lock covers %d sources; want %d", len(sources), expectedSources)
	}
	if totalBytes > maxCorpusBytes {
		return fmt.Errorf("reviewed corpus lock declares %d bytes; limit is %d", totalBytes, maxCorpusBytes)
	}
	if digestDocuments(reviewed.SourceListDigest, reviewed.Documents) != reviewed.CorpusDigest {
		return fmt.Errorf("reviewed corpus lock digest does not match its document metadata")
	}
	return nil
}

func validateSHA256(value, name string) error {
	const prefix = "sha256:"
	if len(value) != len(prefix)+sha256.Size*2 || !strings.HasPrefix(value, prefix) || value != strings.ToLower(value) {
		return fmt.Errorf("%s is not a canonical SHA-256 digest", name)
	}
	if _, err := hex.DecodeString(strings.TrimPrefix(value, prefix)); err != nil {
		return fmt.Errorf("%s is not a canonical SHA-256 digest: %w", name, err)
	}
	return nil
}

func locksEqual(first, second corpusLock) bool {
	if first.SchemaVersion != second.SchemaVersion ||
		first.CorpusID != second.CorpusID ||
		first.RetrievedAt != second.RetrievedAt ||
		first.SourceDateEpoch != second.SourceDateEpoch ||
		first.SourceListDigest != second.SourceListDigest ||
		first.CorpusDigest != second.CorpusDigest ||
		first.DocumentCount != second.DocumentCount ||
		first.SourceCount != second.SourceCount ||
		len(first.Documents) != len(second.Documents) {
		return false
	}
	for index := range first.Documents {
		if first.Documents[index] != second.Documents[index] {
			return false
		}
	}
	return true
}

func checkpointAndVerifyIndex(corpusRoot string) error {
	dbPath := filepath.Join(corpusRoot, ".cache", "search.db")
	if err := checkpointIndexDatabase(dbPath); err != nil {
		return err
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		path := dbPath + suffix
		info, err := os.Stat(path)
		if err == nil && info.Size() > 0 {
			return fmt.Errorf("search index is not checkpointed: %s has %d bytes", path, info.Size())
		}
		if err == nil {
			if err := os.Remove(path); err != nil {
				return fmt.Errorf("remove empty SQLite sidecar %s: %w", path, err)
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("inspect SQLite sidecar %s: %w", path, err)
		}
	}
	return nil
}

func checkpointIndexDatabase(dbPath string) (returnErr error) {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return fmt.Errorf("open search index: %w", err)
	}
	defer func() {
		if closeErr := db.Close(); closeErr != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("close search index: %w", closeErr))
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := db.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		return fmt.Errorf("checkpoint search index: %w", err)
	}
	if _, err := db.ExecContext(ctx, "PRAGMA journal_mode=DELETE"); err != nil {
		return fmt.Errorf("set search index journal mode: %w", err)
	}
	var integrity string
	if err := db.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&integrity); err != nil {
		return fmt.Errorf("check search index integrity: %w", err)
	}
	if integrity != "ok" {
		return fmt.Errorf("search index integrity check returned %q", integrity)
	}
	var documents int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM docs").Scan(&documents); err != nil {
		return fmt.Errorf("count indexed documents: %w", err)
	}
	if documents != expectedDocuments {
		return fmt.Errorf("search index contains %d documents; want %d", documents, expectedDocuments)
	}
	return nil
}

func stageBuildContext(lock corpusLock, corpusRoot, buildContext, binaryPath, dockerfilePath, indexDigest string) (stageManifest, error) {
	if err := validateBuildContext(buildContext); err != nil {
		return stageManifest{}, err
	}
	if strings.TrimSpace(binaryPath) == "" {
		return stageManifest{}, fmt.Errorf("--binary is required with --build-context")
	}
	if err := makeDirectoriesWritableForRemoval(buildContext); err != nil && !errors.Is(err, os.ErrNotExist) {
		return stageManifest{}, fmt.Errorf("prepare existing build context for replacement: %w", err)
	}
	if err := os.RemoveAll(buildContext); err != nil {
		return stageManifest{}, fmt.Errorf("clear build context: %w", err)
	}
	corpusOut := filepath.Join(buildContext, "corpus")
	if err := os.MkdirAll(filepath.Join(corpusOut, ".cache"), 0o755); err != nil {
		return stageManifest{}, err
	}
	for _, source := range sortedSourceIDs() {
		if err := os.MkdirAll(filepath.Join(corpusOut, source), 0o755); err != nil {
			return stageManifest{}, err
		}
		if err := writeCanonicalSourceManifest(
			filepath.Join(corpusOut, source, "manifest.json"),
			lock,
			source,
		); err != nil {
			return stageManifest{}, err
		}
	}
	for _, document := range lock.Documents {
		destination := filepath.Join(corpusOut, filepath.FromSlash(document.Path))
		if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
			return stageManifest{}, err
		}
		if err := copyFile(filepath.Join(corpusRoot, filepath.FromSlash(document.Path)), destination, 0o444); err != nil {
			return stageManifest{}, err
		}
	}
	if err := copyFile(
		filepath.Join(corpusRoot, ".cache", "search.db"),
		filepath.Join(corpusOut, ".cache", "search.db"),
		0o444,
	); err != nil {
		return stageManifest{}, err
	}
	if err := copyFile(binaryPath, filepath.Join(buildContext, "docs-puller"), 0o555); err != nil {
		return stageManifest{}, fmt.Errorf("stage docs-puller binary: %w", err)
	}
	binaryDigest, _, err := regularFileDigest(binaryPath)
	if err != nil {
		return stageManifest{}, err
	}
	manifest := stageManifest{
		SchemaVersion:   stageSchemaVersion,
		CorpusID:        lock.CorpusID,
		RetrievedAt:     lock.RetrievedAt,
		SourceDateEpoch: lock.SourceDateEpoch,
		CorpusDigest:    lock.CorpusDigest,
		IndexDigest:     indexDigest,
		BinaryDigest:    binaryDigest,
		DocumentCount:   lock.DocumentCount,
		SourceCount:     lock.SourceCount,
		Documents:       lock.Documents,
	}
	if err := writeJSONAtomic(filepath.Join(corpusOut, "demo-manifest.json"), manifest, 0o444); err != nil {
		return stageManifest{}, err
	}
	rootfsPath := filepath.Join(buildContext, ".rootfs.pending.tar")
	if err := createRootFSTar(rootfsPath, filepath.Join(buildContext, "docs-puller"), corpusOut, lock.RetrievedAt); err != nil {
		return stageManifest{}, err
	}
	rootfsDigest, _, err := regularFileDigest(rootfsPath)
	if err != nil {
		return stageManifest{}, fmt.Errorf("digest root filesystem archive: %w", err)
	}
	manifest.RootFSDigest = rootfsDigest
	rootfsArchive, err := rootFSArchiveName(rootfsDigest)
	if err != nil {
		return stageManifest{}, err
	}
	if err := os.Rename(rootfsPath, filepath.Join(buildContext, rootfsArchive)); err != nil {
		return stageManifest{}, fmt.Errorf("content-address root filesystem archive: %w", err)
	}
	manifest.RootFSArchive = rootfsArchive
	if err := stageDockerfile(dockerfilePath, filepath.Join(buildContext, "Dockerfile"), rootfsArchive); err != nil {
		return stageManifest{}, err
	}
	if err := os.RemoveAll(corpusOut); err != nil {
		return stageManifest{}, fmt.Errorf("remove loose corpus from build context: %w", err)
	}
	if err := os.Remove(filepath.Join(buildContext, "docs-puller")); err != nil {
		return stageManifest{}, fmt.Errorf("remove loose binary from build context: %w", err)
	}
	if err := writeJSONAtomic(filepath.Join(buildContext, "build-manifest.json"), manifest, 0o444); err != nil {
		return stageManifest{}, err
	}
	if err := makeBuildContextImmutable(buildContext, lock.RetrievedAt); err != nil {
		return stageManifest{}, err
	}
	return manifest, nil
}

// writeCanonicalSourceManifest removes pull-time metadata from the immutable
// runtime. Every retained field is derived from the reviewed corpus lock. The
// snapshot timestamp is intentionally shared by all entries so two clean pulls
// of unchanged documents produce the same root filesystem archive.
func writeCanonicalSourceManifest(destination string, lock corpusLock, source string) error {
	manifest := sourceManifest{
		Version: 1,
		Entries: make(map[string]sourceEntry),
	}
	for _, document := range lock.Documents {
		if document.Source != source {
			continue
		}
		if _, exists := manifest.Entries[document.URL]; exists {
			return fmt.Errorf("canonical source manifest contains duplicate URL %s", document.URL)
		}
		manifest.Entries[document.URL] = sourceEntry{
			URL:       document.URL,
			Source:    document.Source,
			Path:      document.Path,
			Mode:      "http",
			SHA256:    strings.TrimPrefix(document.SHA256, "sha256:"),
			FetchedAt: lock.RetrievedAt,
		}
	}
	if len(manifest.Entries) == 0 {
		return fmt.Errorf("canonical source manifest has no documents for %s", source)
	}
	if err := writeJSONAtomic(destination, manifest, 0o444); err != nil {
		return fmt.Errorf("write canonical source manifest for %s: %w", source, err)
	}
	return nil
}

func rootFSArchiveName(digest string) (string, error) {
	const prefix = "sha256:"
	hexDigest := strings.TrimPrefix(digest, prefix)
	if !strings.HasPrefix(digest, prefix) || len(hexDigest) != sha256.Size*2 {
		return "", fmt.Errorf("invalid root filesystem digest %q", digest)
	}
	if _, err := hex.DecodeString(hexDigest); err != nil {
		return "", fmt.Errorf("invalid root filesystem digest %q: %w", digest, err)
	}
	return "rootfs-" + hexDigest + ".tar", nil
}

func stageDockerfile(templatePath, destination, rootfsArchive string) error {
	if filepath.Base(rootfsArchive) != rootfsArchive || strings.ContainsAny(rootfsArchive, `/\\`) {
		return fmt.Errorf("invalid root filesystem archive name %q", rootfsArchive)
	}
	info, err := os.Lstat(templatePath)
	if err != nil {
		return fmt.Errorf("inspect Dockerfile template: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("dockerfile template is not a regular file")
	}
	if info.Size() > 64<<10 {
		return fmt.Errorf("dockerfile template is larger than 64 KiB")
	}
	template, err := os.ReadFile(templatePath)
	if err != nil {
		return fmt.Errorf("read Dockerfile template: %w", err)
	}
	if bytes.Count(template, []byte(dockerfileMarker)) != 1 {
		return fmt.Errorf("dockerfile template must contain exactly one %s marker", dockerfileMarker)
	}
	staged := bytes.Replace(template, []byte(dockerfileMarker), []byte(rootfsArchive), 1)
	if err := writeFileExclusive(destination, staged, 0o444); err != nil {
		return fmt.Errorf("stage Dockerfile: %w", err)
	}
	return nil
}

type rootFSEntry struct {
	source string
	target string
	mode   fs.FileMode
	isDir  bool
}

func createRootFSTar(destination, binaryPath, corpusRoot, timestamp string) (returnErr error) {
	normalizedTime, err := time.Parse(time.RFC3339Nano, timestamp)
	if err != nil {
		return fmt.Errorf("parse root filesystem timestamp: %w", err)
	}
	entries := []rootFSEntry{
		{target: "app/", mode: 0o555, isDir: true},
		{target: "app/docs/", mode: 0o555, isDir: true},
		{target: "usr/", mode: 0o555, isDir: true},
		{target: "usr/local/", mode: 0o555, isDir: true},
		{target: "usr/local/bin/", mode: 0o555, isDir: true},
		{source: binaryPath, target: "usr/local/bin/docs-puller", mode: 0o555},
	}
	if err := filepath.WalkDir(corpusRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("root filesystem input contains symlink %s", path)
		}
		relative, err := filepath.Rel(corpusRoot, path)
		if err != nil {
			return err
		}
		if relative == "." {
			return nil
		}
		target := pathpkg.Join("app/docs", filepath.ToSlash(relative))
		if entry.IsDir() {
			target += "/"
			entries = append(entries, rootFSEntry{target: target, mode: 0o555, isDir: true})
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("root filesystem input is not regular: %s", path)
		}
		entries = append(entries, rootFSEntry{source: path, target: target, mode: 0o444})
		return nil
	}); err != nil {
		return fmt.Errorf("collect root filesystem: %w", err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].target < entries[j].target })

	output, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o444)
	if err != nil {
		return fmt.Errorf("create root filesystem archive: %w", err)
	}
	closed := false
	defer func() {
		if !closed {
			if closeErr := output.Close(); closeErr != nil {
				returnErr = errors.Join(returnErr, fmt.Errorf("close root filesystem archive: %w", closeErr))
			}
		}
	}()
	tape := tar.NewWriter(output)
	tapeClosed := false
	defer func() {
		if !tapeClosed {
			if closeErr := tape.Close(); closeErr != nil {
				returnErr = errors.Join(returnErr, fmt.Errorf("close root filesystem tar stream: %w", closeErr))
			}
		}
	}()
	for _, entry := range entries {
		header := &tar.Header{
			Name:     entry.target,
			Mode:     int64(entry.mode.Perm()),
			Uid:      65532,
			Gid:      65532,
			ModTime:  normalizedTime,
			Typeflag: tar.TypeReg,
			Format:   tar.FormatPAX,
		}
		if entry.isDir {
			header.Typeflag = tar.TypeDir
		} else {
			info, err := os.Stat(entry.source)
			if err != nil {
				return fmt.Errorf("stat root filesystem input %s: %w", entry.source, err)
			}
			header.Size = info.Size()
		}
		if err := tape.WriteHeader(header); err != nil {
			return fmt.Errorf("write root filesystem header %s: %w", entry.target, err)
		}
		if !entry.isDir {
			if err := appendFileToTar(tape, entry.source); err != nil {
				return err
			}
		}
	}
	if err := tape.Close(); err != nil {
		return fmt.Errorf("close root filesystem tar stream: %w", err)
	}
	tapeClosed = true
	if err := output.Sync(); err != nil {
		return fmt.Errorf("sync root filesystem archive: %w", err)
	}
	if err := output.Close(); err != nil {
		return fmt.Errorf("close root filesystem archive: %w", err)
	}
	closed = true
	return nil
}

func appendFileToTar(tape *tar.Writer, path string) (returnErr error) {
	input, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open root filesystem input %s: %w", path, err)
	}
	defer func() {
		if closeErr := input.Close(); closeErr != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("close root filesystem input %s: %w", path, closeErr))
		}
	}()
	if _, err := io.Copy(tape, input); err != nil {
		return fmt.Errorf("copy root filesystem input %s: %w", path, err)
	}
	return nil
}

func validateCorpusTree(root string) error {
	rootInfo, err := os.Stat(root)
	if err != nil {
		return fmt.Errorf("open corpus root: %w", err)
	}
	if !rootInfo.IsDir() {
		return fmt.Errorf("corpus root is not a directory")
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	allowedRootFiles := map[string]bool{"_INDEX.md": true, "_INGEST_LOG.jsonl": true}
	for _, entry := range entries {
		name := entry.Name()
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("corpus root contains symlink %s", name)
		}
		if entry.IsDir() {
			if name != ".cache" {
				if _, ok := allowedSources[name]; !ok {
					return fmt.Errorf("corpus contains unreviewed source directory %s", name)
				}
			}
		} else if !allowedRootFiles[name] {
			return fmt.Errorf("corpus root contains unreviewed file %s", name)
		}
	}
	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("corpus contains symlink %s", path)
		}
		return nil
	})
}

func validateExactCorpusFiles(root string, documents []lockedDocument) error {
	allowed := map[string]bool{
		"_INDEX.md":            true,
		"_INGEST_LOG.jsonl":    true,
		".cache/.write-lock":   true,
		".cache/search.db":     true,
		".cache/search.db-shm": true,
		".cache/search.db-wal": true,
	}
	for _, source := range sortedSourceIDs() {
		allowed[source+"/manifest.json"] = true
		allowed[source+"/.titles.json"] = true
		allowed[source+"/_INDEX.md"] = true
	}
	for _, document := range documents {
		allowed[document.Path] = true
	}
	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		if !allowed[relative] {
			return fmt.Errorf("corpus contains file outside the reviewed boundary: %s", relative)
		}
		return nil
	})
}

func validateBuildContext(path string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	if filepath.Base(abs) != ".build" || filepath.Base(filepath.Dir(abs)) != "demo" {
		return fmt.Errorf("build context must be the explicit deploy/demo/.build directory")
	}
	if abs == string(filepath.Separator) || len(strings.Split(filepath.Clean(abs), string(filepath.Separator))) < 4 {
		return fmt.Errorf("refusing broad build context %s", abs)
	}
	return nil
}

func readSourceList(path string) (map[string]bool, string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, "", fmt.Errorf("read source list: %w", err)
	}
	urls := make(map[string]bool)
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "https://") {
			continue
		}
		parsed, err := url.Parse(line)
		if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil {
			return nil, "", fmt.Errorf("source list contains invalid URL %q", line)
		}
		canonical := parsed.String()
		if urls[canonical] {
			return nil, "", fmt.Errorf("source list contains duplicate URL %s", canonical)
		}
		urls[canonical] = true
	}
	if len(urls) != expectedDocuments {
		return nil, "", fmt.Errorf("source list contains %d URLs; want %d", len(urls), expectedDocuments)
	}
	digest := sha256.Sum256(data)
	return urls, "sha256:" + hex.EncodeToString(digest[:]), nil
}

func readSourceManifest(path string) (sourceManifest, error) {
	return decodeJSONFile[sourceManifest](path, "source manifest")
}

func readLock(path string) (corpusLock, error) {
	return decodeJSONFile[corpusLock](path, "corpus lock")
}

func decodeJSONFile[T any](path, label string) (T, error) {
	var value T
	data, err := os.ReadFile(path)
	if err != nil {
		return value, fmt.Errorf("read %s %s: %w", label, path, err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return value, fmt.Errorf("decode %s %s: %w", label, path, err)
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return value, fmt.Errorf("decode %s %s: trailing JSON content", label, path)
	}
	return value, nil
}

func validatePublicURL(value, source string) (string, error) {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil || parsed.Port() != "" {
		return "", fmt.Errorf("%s: invalid public URL", value)
	}
	policy, ok := allowedSources[source]
	if !ok || !policy.Hosts[parsed.Hostname()] {
		return "", fmt.Errorf("%s: host is outside the %s allowlist", value, source)
	}
	parsed.Fragment = ""
	return parsed.String(), nil
}

func validateDocumentPath(value, source string) (string, error) {
	if value == "" || strings.ContainsAny(value, "\\\x00") {
		return "", fmt.Errorf("%s: invalid corpus document path", value)
	}
	cleaned := pathpkg.Clean(value)
	if cleaned != value || cleaned == "." || strings.HasPrefix(cleaned, "/") || !strings.HasPrefix(cleaned, source+"/") || !strings.HasSuffix(cleaned, ".md") {
		return "", fmt.Errorf("%s: invalid corpus document path", value)
	}
	return cleaned, nil
}

func digestDocuments(sourceListDigest string, documents []lockedDocument) string {
	payload := []byte(sourceListDigest + "\n")
	for _, document := range documents {
		payload = fmt.Appendf(payload, "%s\x00%s\x00%d\x00%s\x00%s\n", document.Path, document.SHA256, document.Bytes, document.URL, document.License)
	}
	digest := sha256.Sum256(payload)
	return "sha256:" + hex.EncodeToString(digest[:])
}

func regularFileDigest(path string) (string, int64, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", 0, fmt.Errorf("stat %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return "", 0, fmt.Errorf("%s is not a regular file", path)
	}
	return fileDigest(path)
}

func fileDigest(path string) (digest string, size int64, returnErr error) {
	file, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer func() {
		if closeErr := file.Close(); closeErr != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("close %s: %w", path, closeErr))
		}
	}()
	hash := sha256.New()
	size, err = io.Copy(hash, file)
	if err != nil {
		return "", 0, fmt.Errorf("digest %s: %w", path, err)
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil)), size, nil
}

func copyFile(source, destination string, mode fs.FileMode) (returnErr error) {
	input, err := os.Open(source)
	if err != nil {
		return fmt.Errorf("open %s: %w", source, err)
	}
	defer func() {
		if closeErr := input.Close(); closeErr != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("close %s: %w", source, closeErr))
		}
	}()
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return err
	}
	output, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return fmt.Errorf("create %s: %w", destination, err)
	}
	_, copyErr := io.Copy(output, input)
	closeErr := output.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Chmod(destination, mode)
}

func writeFileExclusive(path string, data []byte, mode fs.FileMode) (returnErr error) {
	output, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	closed := false
	defer func() {
		if !closed {
			if closeErr := output.Close(); closeErr != nil {
				returnErr = errors.Join(returnErr, closeErr)
			}
		}
	}()
	if _, err := output.Write(data); err != nil {
		return err
	}
	if err := output.Sync(); err != nil {
		return err
	}
	if err := output.Close(); err != nil {
		return err
	}
	closed = true
	return os.Chmod(path, mode)
}

func writeJSONAtomic(path string, value any, mode fs.FileMode) (returnErr error) {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".corpus-builder-*.tmp")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	closed := false
	defer func() {
		if !closed {
			if closeErr := temporary.Close(); closeErr != nil {
				returnErr = errors.Join(returnErr, fmt.Errorf("close temporary file: %w", closeErr))
			}
		}
		if removeErr := os.Remove(temporaryPath); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			returnErr = errors.Join(returnErr, fmt.Errorf("remove temporary file: %w", removeErr))
		}
	}()
	if _, err := temporary.Write(data); err != nil {
		return fmt.Errorf("write temporary file: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		return fmt.Errorf("sync temporary file: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close temporary file: %w", err)
	}
	closed = true
	if err := os.Chmod(temporaryPath, mode); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("replace %s: %w", path, err)
	}
	return syncDirectory(filepath.Dir(path))
}

func syncDirectory(path string) (returnErr error) {
	if runtime.GOOS == "windows" {
		// Windows does not support fsync on directory handles. The temporary
		// file itself is synced before rename, which is the strongest portable
		// durability guarantee available on that platform.
		return nil
	}
	directory, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open parent directory %s: %w", path, err)
	}
	defer func() {
		if closeErr := directory.Close(); closeErr != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("close parent directory %s: %w", path, closeErr))
		}
	}()
	if err := directory.Sync(); err != nil {
		return fmt.Errorf("sync parent directory %s: %w", path, err)
	}
	return nil
}

func makeBuildContextImmutable(root, timestamp string) error {
	normalizedTime, err := time.Parse(time.RFC3339Nano, timestamp)
	if err != nil {
		return fmt.Errorf("normalize build context timestamp: %w", err)
	}
	var directories []string
	if err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("build context contains symlink %s", path)
		}
		if entry.IsDir() {
			directories = append(directories, path)
			return nil
		}
		if err := os.Chtimes(path, normalizedTime, normalizedTime); err != nil {
			return fmt.Errorf("normalize build file timestamp %s: %w", path, err)
		}
		return nil
	}); err != nil {
		return err
	}
	sort.Slice(directories, func(i, j int) bool { return len(directories[i]) > len(directories[j]) })
	for _, directory := range directories {
		if err := os.Chmod(directory, 0o555); err != nil {
			return fmt.Errorf("make build directory read-only %s: %w", directory, err)
		}
		if err := os.Chtimes(directory, normalizedTime, normalizedTime); err != nil {
			return fmt.Errorf("normalize build directory timestamp %s: %w", directory, err)
		}
	}
	return nil
}

func makeDirectoriesWritableForRemoval(root string) error {
	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if err := os.Chmod(path, 0o755); err != nil {
				return fmt.Errorf("make build directory removable %s: %w", path, err)
			}
		}
		return nil
	})
}

func sortedSourceIDs() []string {
	sources := make([]string, 0, len(allowedSources))
	for source := range allowedSources {
		sources = append(sources, source)
	}
	sort.Strings(sources)
	return sources
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "corpus-builder: "+format+"\n", args...)
	os.Exit(1)
}
