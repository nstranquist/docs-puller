package main

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/nstranquist/docs-puller/internal/embeddb"
)

type docsStatus struct {
	Out            string             `json:"out"`
	SourceCount    int                `json:"source_count"`
	TotalDocs      int                `json:"total_docs"`
	IndexableDocs  int                `json:"indexable_docs"`
	LastPullNewest string             `json:"last_pull_newest,omitempty"`
	LastPullOldest string             `json:"last_pull_oldest,omitempty"`
	Profile        statusProfile      `json:"profile"`
	FTS            statusFTS          `json:"fts"`
	Embeddings     statusEmbeddings   `json:"embeddings"`
	IngestLog      statusIngestLog    `json:"ingest_log"`
	Pins           statusPins         `json:"pins"`
	StaleSources   []statusSourceInfo `json:"stale_sources,omitempty"`
	DupPathSources []statusSourceInfo `json:"dup_path_sources,omitempty"`
	Warnings       []string           `json:"warnings,omitempty"`
}

type statusSourceInfo struct {
	Name           string `json:"name"`
	Docs           int    `json:"docs"`
	IndexableDocs  int    `json:"indexable_docs"`
	LastPull       string `json:"last_pull,omitempty"`
	LastPullOldest string `json:"last_pull_oldest,omitempty"`
	AgeHours       int    `json:"age_hours,omitempty"`
	MissingFresh   bool   `json:"missing_freshness,omitempty"`
	DupPathEntries int    `json:"dup_path_entries,omitempty"`
}

type statusProfile struct {
	Name   string `json:"name,omitempty"`
	Reason string `json:"reason"`
}

type statusFTS struct {
	Path          string `json:"path"`
	Exists        bool   `json:"exists"`
	Ready         bool   `json:"ready"`
	Docs          int    `json:"docs"`
	SizeBytes     int64  `json:"size_bytes,omitempty"`
	WALSizeBytes  int64  `json:"wal_size_bytes,omitempty"`
	SHMSizeBytes  int64  `json:"shm_size_bytes,omitempty"`
	ModifiedAt    string `json:"modified_at,omitempty"`
	MatchesCorpus bool   `json:"matches_corpus"`
	Error         string `json:"error,omitempty"`
}

type statusEmbeddings struct {
	Path         string                 `json:"path"`
	Exists       bool                   `json:"exists"`
	Legacy       bool                   `json:"legacy,omitempty"`
	SizeBytes    int64                  `json:"size_bytes,omitempty"`
	WALSizeBytes int64                  `json:"wal_size_bytes,omitempty"`
	SHMSizeBytes int64                  `json:"shm_size_bytes,omitempty"`
	ModifiedAt   string                 `json:"modified_at,omitempty"`
	Models       []statusEmbeddingModel `json:"models,omitempty"`
	FlatIndexes  []statusFlatIndex      `json:"flat_indexes,omitempty"`
	Error        string                 `json:"error,omitempty"`
}

type statusEmbeddingModel struct {
	Model string `json:"model"`
	Docs  int    `json:"docs"`
	Dim   int    `json:"dim"`
}

type statusFlatIndex struct {
	Model      string `json:"model"`
	Path       string `json:"path"`
	Exists     bool   `json:"exists"`
	Count      int    `json:"count"`
	Dim        int    `json:"dim"`
	SizeBytes  int64  `json:"size_bytes,omitempty"`
	ModifiedAt string `json:"modified_at,omitempty"`
	Error      string `json:"error,omitempty"`
}

type statusIngestLog struct {
	Path    string    `json:"path"`
	Exists  bool      `json:"exists"`
	Entries int       `json:"entries"`
	Last    *logEntry `json:"last,omitempty"`
	Error   string    `json:"error,omitempty"`
}

type statusPins struct {
	Path    string `json:"path"`
	Exists  bool   `json:"exists"`
	Active  int    `json:"active"`
	Skipped int    `json:"skipped"`
	Orphans int    `json:"orphans"`
	Error   string `json:"error,omitempty"`
}

func cmdStatus(args []string) {
	o := defaultOpts()
	var (
		asJSON          bool
		check           bool
		checkEmbeddings bool
		staleDays       int
	)
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	fs.StringVar(&o.out, "out", o.out, "output root dir")
	fs.BoolVar(&asJSON, "json", false, "emit JSON instead of human-readable")
	fs.BoolVar(&check, "check", false, "exit non-zero when core corpus/index warnings are present")
	fs.BoolVar(&checkEmbeddings, "check-embeddings", false, "with --check, also fail on embedding DB or flat sidecar warnings")
	fs.IntVar(&staleDays, "stale-days", 30, "warn when a source's newest manifest entry is older than this many days (0 disables)")
	fs.Parse(args)

	staleAfter := time.Duration(staleDays) * 24 * time.Hour
	if staleDays <= 0 {
		staleAfter = 0
	}
	status, err := collectDocsStatus(o.out, staleAfter)
	if err != nil {
		die(err)
	}

	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(status); err != nil {
			die(err)
		}
	} else {
		emitDocsStatusText(status)
	}

	if check && len(buildStatusCheckWarnings(status, checkEmbeddings)) > 0 {
		os.Exit(1)
	}
}

func collectDocsStatus(out string, staleAfter time.Duration) (docsStatus, error) {
	sources, err := listSources(out)
	if err != nil {
		return docsStatus{}, err
	}
	status := docsStatus{
		Out:          out,
		Profile:      collectStatusProfile(out),
		StaleSources: []statusSourceInfo{},
		Warnings:     []string{},
	}

	for _, src := range sources {
		info := collectStatusSource(out, src, staleAfter)
		if info.Docs == 0 {
			continue
		}
		status.SourceCount++
		status.TotalDocs += info.Docs
		status.IndexableDocs += info.IndexableDocs
		if info.LastPull != "" {
			if status.LastPullNewest == "" || info.LastPull > status.LastPullNewest {
				status.LastPullNewest = info.LastPull
			}
			oldest := info.LastPullOldest
			if oldest == "" {
				oldest = info.LastPull
			}
			if status.LastPullOldest == "" || oldest < status.LastPullOldest {
				status.LastPullOldest = oldest
			}
		}
		if info.MissingFresh || info.AgeHours > 0 {
			status.StaleSources = append(status.StaleSources, info)
		}
		if info.DupPathEntries > 0 {
			status.DupPathSources = append(status.DupPathSources, info)
		}
	}
	sort.Slice(status.DupPathSources, func(i, j int) bool {
		if status.DupPathSources[i].DupPathEntries != status.DupPathSources[j].DupPathEntries {
			return status.DupPathSources[i].DupPathEntries > status.DupPathSources[j].DupPathEntries
		}
		return status.DupPathSources[i].Name < status.DupPathSources[j].Name
	})
	sort.Slice(status.StaleSources, func(i, j int) bool {
		if status.StaleSources[i].MissingFresh != status.StaleSources[j].MissingFresh {
			return status.StaleSources[i].MissingFresh
		}
		if status.StaleSources[i].AgeHours != status.StaleSources[j].AgeHours {
			return status.StaleSources[i].AgeHours > status.StaleSources[j].AgeHours
		}
		return status.StaleSources[i].Name < status.StaleSources[j].Name
	})

	status.FTS = collectStatusFTS(out, status.IndexableDocs)
	status.Embeddings = collectStatusEmbeddings(out)
	status.IngestLog = collectStatusIngestLog(out)
	status.Pins = collectStatusPins(out)
	status.Warnings = buildStatusWarnings(status)
	return status, nil
}

func collectStatusSource(out, src string, staleAfter time.Duration) statusSourceInfo {
	srcDir := filepath.Join(out, src)
	info := statusSourceInfo{Name: src}
	metaByPath := loadFTSDocMeta(srcDir, src)
	_ = filepath.WalkDir(srcDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		name := d.Name()
		if !strings.HasSuffix(name, ".md") || name == "_INDEX.md" {
			return nil
		}
		rel, relErr := filepath.Rel(srcDir, p)
		if relErr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		info.Docs++
		meta := metaByPath[rel]
		if shouldIndexFTSDocPath(src, rel, meta) && statusFileHasIndexableContent(p) {
			info.IndexableDocs++
		}
		return nil
	})

	_, fetchedByPath := loadManifestMaps(srcDir, src)
	for _, t := range fetchedByPath {
		if t > info.LastPull {
			info.LastPull = t
		}
		if info.LastPullOldest == "" || t < info.LastPullOldest {
			info.LastPullOldest = t
		}
	}
	if m, err := loadOrMigrateManifest(srcDir); err == nil {
		info.DupPathEntries = dedupeManifestPaths(&m)
	}
	if staleAfter <= 0 {
		return info
	}
	if info.LastPull == "" {
		info.MissingFresh = true
		return info
	}
	last, err := time.Parse(time.RFC3339, info.LastPull)
	if err != nil {
		info.MissingFresh = true
		return info
	}
	age := time.Since(last)
	if age > staleAfter {
		info.AgeHours = int(age.Hours())
	}
	return info
}

func statusFileHasIndexableContent(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()

	r := bufio.NewReader(f)
	for {
		ch, _, err := r.ReadRune()
		if err != nil {
			return false
		}
		if !unicode.IsSpace(ch) {
			return true
		}
	}
}

func collectStatusProfile(out string) statusProfile {
	cwd, _ := os.Getwd()
	name, reason := ResolveActiveProfile(ResolveOpts{Out: out, Cwd: cwd})
	return statusProfile{Name: name, Reason: reason}
}

func collectStatusFTS(out string, totalDocs int) statusFTS {
	path := ftsDBPath(out)
	status := statusFTS{Path: path}
	if st, err := os.Stat(path); err == nil {
		status.Exists = true
		status.SizeBytes = st.Size()
		status.ModifiedAt = st.ModTime().UTC().Format(time.RFC3339)
	} else if !os.IsNotExist(err) {
		status.Error = err.Error()
		return status
	}
	status.WALSizeBytes = fileSize(path + "-wal")
	status.SHMSizeBytes = fileSize(path + "-shm")

	if !status.Exists {
		return status
	}
	idx, err := openFTSIndexReadOnly(out)
	if err != nil {
		status.Error = err.Error()
		return status
	}
	defer idx.close()
	n, err := idx.totalDocs()
	if err != nil {
		status.Error = err.Error()
		return status
	}
	status.Docs = n
	status.Ready = n > 0
	status.MatchesCorpus = totalDocs == 0 || n == totalDocs
	return status
}

func collectStatusEmbeddings(out string) statusEmbeddings {
	path := embeddingsReadDBPath(out)
	status := statusEmbeddings{Path: path, FlatIndexes: collectStatusFlatIndexes(out)}
	if path == ftsDBPath(out) {
		status.Legacy = true
	}
	if st, err := os.Stat(path); err == nil {
		status.Exists = true
		status.SizeBytes = st.Size()
		status.ModifiedAt = st.ModTime().UTC().Format(time.RFC3339)
	} else if !os.IsNotExist(err) {
		status.Error = err.Error()
		return status
	}
	status.WALSizeBytes = fileSize(path + "-wal")
	status.SHMSizeBytes = fileSize(path + "-shm")
	if !status.Exists {
		return status
	}

	db, err := openEmbeddingsDB(out, true)
	if err != nil {
		status.Error = err.Error()
		return status
	}
	defer db.Close()
	models, err := collectEmbeddingModels(db)
	if err != nil {
		status.Error = err.Error()
		return status
	}
	status.Models = models
	return status
}

func collectEmbeddingModels(db *sql.DB) ([]statusEmbeddingModel, error) {
	rows, err := embeddb.New(db).ListEmbeddingModels(context.Background())
	if err != nil {
		return nil, err
	}
	models := make([]statusEmbeddingModel, 0, len(rows))
	for _, row := range rows {
		models = append(models, statusEmbeddingModel{
			Model: row.Model,
			Docs:  int(row.Docs),
			Dim:   int(row.Dim),
		})
	}
	return models, nil
}

func collectStatusFlatIndexes(out string) []statusFlatIndex {
	cacheDir := filepath.Join(out, ".cache")
	matches, _ := filepath.Glob(filepath.Join(cacheDir, "embeddings-*.json"))
	sort.Strings(matches)
	indexes := make([]statusFlatIndex, 0, len(matches))
	for _, metaPath := range matches {
		idx := statusFlatIndex{Path: metaPath}
		data, err := os.ReadFile(metaPath)
		if err != nil {
			idx.Error = err.Error()
			indexes = append(indexes, idx)
			continue
		}
		var meta flatEmbeddingMeta
		if err := json.Unmarshal(data, &meta); err != nil {
			idx.Error = err.Error()
			indexes = append(indexes, idx)
			continue
		}
		idx.Model = meta.Model
		idx.Count = meta.Count
		idx.Dim = meta.Dim
		vecPath := filepath.Join(filepath.Dir(metaPath), meta.VectorFile)
		if meta.VectorFile == "" {
			_, vecPath = flatEmbeddingPaths(out, meta.Model)
		}
		if st, err := os.Stat(vecPath); err == nil {
			idx.Exists = true
			idx.SizeBytes = st.Size()
			idx.ModifiedAt = st.ModTime().UTC().Format(time.RFC3339)
		} else if !os.IsNotExist(err) {
			idx.Error = err.Error()
		}
		indexes = append(indexes, idx)
	}
	return indexes
}

func collectStatusIngestLog(out string) statusIngestLog {
	path := filepath.Join(out, ingestLogFile)
	status := statusIngestLog{Path: path}
	if _, err := os.Stat(path); err == nil {
		status.Exists = true
	} else if !os.IsNotExist(err) {
		status.Error = err.Error()
		return status
	}
	entries, err := readIngestLog(out, 0)
	if err != nil {
		status.Error = err.Error()
		return status
	}
	status.Entries = len(entries)
	if len(entries) > 0 {
		last := entries[0]
		status.Last = &last
	}
	return status
}

func collectStatusPins(out string) statusPins {
	path := filepath.Join(out, docsPinsFile)
	status := statusPins{Path: path}
	if _, err := os.Stat(path); err == nil {
		status.Exists = true
	} else if !os.IsNotExist(err) {
		status.Error = err.Error()
		return status
	}
	pins, err := loadDocsPins(out)
	if err != nil {
		if os.IsNotExist(err) {
			return status
		}
		status.Error = err.Error()
		return status
	}
	status.Active = len(pins.Pins)
	status.Skipped = len(pins.Skipped)
	status.Orphans = len(pins.Orphans)
	return status
}

func buildStatusWarnings(status docsStatus) []string {
	return buildStatusWarningsWithOptions(status, true)
}

func buildStatusCheckWarnings(status docsStatus, includeEmbeddings bool) []string {
	return buildStatusWarningsWithOptions(status, includeEmbeddings)
}

func buildStatusWarningsWithOptions(status docsStatus, includeEmbeddings bool) []string {
	var warnings []string
	if status.SourceCount == 0 {
		warnings = append(warnings, fmt.Sprintf("no source dirs found under %s", status.Out))
	}
	if !status.FTS.Exists {
		warnings = append(warnings, fmt.Sprintf("FTS5 index missing at %s; run `docs-puller reindex`", status.FTS.Path))
	} else if status.FTS.Error != "" {
		warnings = append(warnings, fmt.Sprintf("FTS5 index unreadable: %s; run `docs-puller reindex`", status.FTS.Error))
	} else if !status.FTS.Ready {
		warnings = append(warnings, "FTS5 index has zero docs; run `docs-puller reindex`")
	} else if !status.FTS.MatchesCorpus {
		warnings = append(warnings, fmt.Sprintf("FTS5 index has %d docs but indexable corpus has %d; run `docs-puller reindex`", status.FTS.Docs, status.IndexableDocs))
	}
	if includeEmbeddings {
		if status.Embeddings.Exists && status.Embeddings.Error != "" {
			warnings = append(warnings, fmt.Sprintf("embeddings db unreadable: %s; rerun `docs-puller embed`", status.Embeddings.Error))
		}
		if status.Embeddings.Exists && status.Embeddings.Error == "" {
			warnings = append(warnings, flatEmbeddingWarnings(status.Embeddings)...)
		}
	}
	if status.IngestLog.Error != "" {
		warnings = append(warnings, fmt.Sprintf("ingest log unreadable: %s", status.IngestLog.Error))
	} else if !status.IngestLog.Exists || status.IngestLog.Entries == 0 {
		warnings = append(warnings, fmt.Sprintf("no ingest history at %s", status.IngestLog.Path))
	}
	if status.Pins.Error != "" {
		warnings = append(warnings, fmt.Sprintf("docs pins unreadable: %s; rerun `docs-puller pins refresh --write`", status.Pins.Error))
	}
	for _, src := range status.StaleSources {
		if src.MissingFresh {
			warnings = append(warnings, fmt.Sprintf("source %s has no usable last_pull freshness marker", src.Name))
			continue
		}
		warnings = append(warnings, fmt.Sprintf("source %s last pulled %d hours ago", src.Name, src.AgeHours))
	}
	for _, src := range status.DupPathSources {
		warnings = append(warnings, fmt.Sprintf("source %s has %d duplicate-path manifest entr%s (older URL variants of the same file); the next pull of that source prunes them", src.Name, src.DupPathEntries, map[bool]string{true: "y", false: "ies"}[src.DupPathEntries == 1]))
	}
	return warnings
}

func flatEmbeddingWarnings(status statusEmbeddings) []string {
	byModel := make(map[string]statusFlatIndex, len(status.FlatIndexes))
	for _, idx := range status.FlatIndexes {
		if idx.Model != "" {
			byModel[idx.Model] = idx
		}
	}
	var warnings []string
	for _, model := range status.Models {
		idx, ok := byModel[model.Model]
		switch {
		case !ok:
			warnings = append(warnings, fmt.Sprintf("flat embedding index missing for model %s; rerun `docs-puller embed --model %s`", model.Model, model.Model))
		case idx.Error != "":
			warnings = append(warnings, fmt.Sprintf("flat embedding index for model %s unreadable: %s; rerun `docs-puller embed --model %s`", model.Model, idx.Error, model.Model))
		case !idx.Exists:
			warnings = append(warnings, fmt.Sprintf("flat embedding vector sidecar missing for model %s; rerun `docs-puller embed --model %s`", model.Model, model.Model))
		case idx.Count != model.Docs || idx.Dim != model.Dim:
			warnings = append(warnings, fmt.Sprintf("flat embedding index stale for model %s: flat count/dim %d/%d, embeddings count/dim %d/%d; rerun `docs-puller embed --model %s`",
				model.Model, idx.Count, idx.Dim, model.Docs, model.Dim, model.Model))
		}
	}
	return warnings
}

func emitDocsStatusText(status docsStatus) {
	if status.IndexableDocs != status.TotalDocs {
		fmt.Printf("docs corpus: %d sources, %d docs (%d indexable)\n", status.SourceCount, status.TotalDocs, status.IndexableDocs)
	} else {
		fmt.Printf("docs corpus: %d sources, %d docs\n", status.SourceCount, status.TotalDocs)
	}
	fmt.Printf("out: %s\n", status.Out)
	if status.LastPullNewest != "" {
		fmt.Printf("last pulls: newest %s", status.LastPullNewest)
		if status.LastPullOldest != "" && status.LastPullOldest != status.LastPullNewest {
			fmt.Printf(", oldest %s", status.LastPullOldest)
		}
		fmt.Println()
	}
	if status.Profile.Name != "" {
		fmt.Printf("profile: %s (%s)\n", status.Profile.Name, status.Profile.Reason)
	} else {
		fmt.Printf("profile: none (%s)\n", status.Profile.Reason)
	}
	fmt.Printf("fts5: ")
	switch {
	case !status.FTS.Exists:
		fmt.Printf("missing (%s)\n", status.FTS.Path)
	case status.FTS.Error != "":
		fmt.Printf("error (%s)\n", status.FTS.Error)
	default:
		fmt.Printf("%s, %d docs, %s", readyWord(status.FTS.Ready), status.FTS.Docs, byteSize(status.FTS.SizeBytes))
		if status.FTS.ModifiedAt != "" {
			fmt.Printf(", updated %s", status.FTS.ModifiedAt)
		}
		if status.FTS.WALSizeBytes > 0 {
			fmt.Printf(", wal %s", byteSize(status.FTS.WALSizeBytes))
		}
		fmt.Println()
	}
	fmt.Printf("embeddings: ")
	switch {
	case status.Embeddings.Error != "":
		fmt.Printf("error (%s)\n", status.Embeddings.Error)
	case !status.Embeddings.Exists:
		fmt.Printf("missing (%s; optional for rerank/hybrid)\n", status.Embeddings.Path)
	default:
		models := make([]string, 0, len(status.Embeddings.Models))
		for _, m := range status.Embeddings.Models {
			models = append(models, fmt.Sprintf("%s:%d", m.Model, m.Docs))
		}
		if len(models) == 0 {
			models = append(models, "no models")
		}
		legacy := ""
		if status.Embeddings.Legacy {
			legacy = ", legacy search.db"
		}
		fmt.Printf("%s%s [%s]", byteSize(status.Embeddings.SizeBytes), legacy, strings.Join(models, ", "))
		if len(status.Embeddings.FlatIndexes) > 0 {
			ready := 0
			for _, idx := range status.Embeddings.FlatIndexes {
				if idx.Exists && idx.Error == "" {
					ready++
				}
			}
			fmt.Printf(", flat %d/%d", ready, len(status.Embeddings.FlatIndexes))
		}
		fmt.Println()
	}
	fmt.Printf("ingest log: ")
	switch {
	case status.IngestLog.Error != "":
		fmt.Printf("error (%s)\n", status.IngestLog.Error)
	case !status.IngestLog.Exists || status.IngestLog.Entries == 0:
		fmt.Printf("missing (%s)\n", status.IngestLog.Path)
	default:
		fmt.Printf("%d entries", status.IngestLog.Entries)
		if status.IngestLog.Last != nil {
			fmt.Printf(", last %s %s", status.IngestLog.Last.StartedAt, status.IngestLog.Last.Mode)
			if len(status.IngestLog.Last.Sources) > 0 {
				fmt.Printf(" [%s]", strings.Join(status.IngestLog.Last.Sources, ","))
			}
		}
		fmt.Println()
	}
	fmt.Printf("pins: ")
	switch {
	case status.Pins.Error != "":
		fmt.Printf("error (%s)\n", status.Pins.Error)
	case !status.Pins.Exists:
		fmt.Printf("missing (%s; optional until versioned docs are needed)\n", status.Pins.Path)
	default:
		fmt.Printf("%d active, %d skipped, %d orphans\n", status.Pins.Active, status.Pins.Skipped, status.Pins.Orphans)
	}
	if len(status.Warnings) == 0 {
		fmt.Println("status: ok")
		return
	}
	fmt.Println("warnings:")
	for _, w := range status.Warnings {
		fmt.Printf("  - %s\n", w)
	}
}

func readyWord(ok bool) string {
	if ok {
		return "ready"
	}
	return "not ready"
}

func fileSize(path string) int64 {
	st, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return st.Size()
}

func byteSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for n >= div*unit && exp < 4 {
		div *= unit
		exp++
	}
	value := float64(n) / float64(div)
	suffix := []string{"KiB", "MiB", "GiB", "TiB", "PiB"}[exp]
	return fmt.Sprintf("%.1f %s", value, suffix)
}
