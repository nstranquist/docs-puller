package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	defaultPDFSource       = "pdf-docs"
	defaultPDFTimeout      = 5 * time.Minute
	defaultPDFMaxInputSize = 100 << 20
	maxPDFToolOutput       = 64 << 20
)

type pdfDetectionOutput struct {
	PDFType string `json:"pdf_type"`
	Error   string `json:"error"`
}

type pdfMarkdownOutput struct {
	PDFType  string `json:"pdf_type"`
	Error    string `json:"error"`
	Markdown string `json:"markdown"`
}

type pdfToolRunner func(context.Context, string, ...string) ([]byte, error)

type preparedPDF struct {
	result result
	body   []byte
}

// limitedBuffer prevents a broken provider from returning an unbounded result
// to the Go process. The child still receives the context timeout.
type limitedBuffer struct {
	bytes.Buffer
	limit    int
	exceeded bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if b.exceeded {
		return len(p), nil
	}
	remaining := b.limit - b.Len()
	if remaining <= 0 {
		b.exceeded = true
		return len(p), nil
	}
	if len(p) > remaining {
		_, _ = b.Buffer.Write(p[:remaining])
		b.exceeded = true
		return len(p), nil
	}
	return b.Buffer.Write(p)
}

func runPDFTool(ctx context.Context, binary string, args ...string) ([]byte, error) {
	if strings.TrimSpace(binary) == "" {
		return nil, fmt.Errorf("PDF provider executable is empty")
	}
	cmd := exec.CommandContext(ctx, binary, args...)
	var stdout, stderr limitedBuffer
	stdout.limit = maxPDFToolOutput
	stderr.limit = 8 << 10
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("%s timed out: %w", filepath.Base(binary), ctx.Err())
		}
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = err.Error()
		}
		return nil, fmt.Errorf("%s failed: %s", filepath.Base(binary), message)
	}
	if stdout.exceeded {
		return nil, fmt.Errorf("%s returned more than %d bytes", filepath.Base(binary), maxPDFToolOutput)
	}
	return stdout.Bytes(), nil
}

func cmdPullPDF(args []string) {
	o := defaultOpts()
	fs := flag.NewFlagSet("pull-pdf", flag.ExitOnError)
	name := fs.String("name", "", "document name (default: PDF filename without extension)")
	source := fs.String("source", defaultPDFSource, "output source name")
	detectBinary := fs.String("detect-pdf", "detect-pdf", "pdf-inspector classifier executable")
	convertBinary := fs.String("pdf2md", "pdf2md", "pdf-inspector Markdown executable")
	providerPin := fs.String("provider-pin", strings.TrimSpace(os.Getenv("DOCS_PULLER_PDF_PROVIDER_PIN")), "required pdf-inspector pin JSON")
	timeout := fs.Duration("timeout", defaultPDFTimeout, "provider subprocess timeout")
	maxInputBytes := fs.Int64("max-input-bytes", defaultPDFMaxInputSize, "maximum PDF input size in bytes")
	bindOpts(fs, &o)
	normalizedArgs, err := normalizePDFArgs(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pull-pdf: %s\n", err)
		os.Exit(2)
	}
	fs.Parse(normalizedArgs)
	if *timeout <= 0 {
		fmt.Fprintln(os.Stderr, "pull-pdf: --timeout must be greater than zero")
		os.Exit(2)
	}
	if *maxInputBytes <= 0 {
		fmt.Fprintln(os.Stderr, "pull-pdf: --max-input-bytes must be greater than zero")
		os.Exit(2)
	}

	input := fs.Arg(0)
	start := time.Now()
	now := start.UTC().Format(time.RFC3339)
	if strings.TrimSpace(*providerPin) == "" {
		fmt.Fprintln(os.Stderr, "pull-pdf: --provider-pin is required; run pdf-doctor to create or verify one")
		os.Exit(2)
	}
	verification, err := verifyPDFProvider(*providerPin, *detectBinary, *convertBinary)
	if err != nil {
		recordPDFIngest(o, args, now, start, "", 1, 0)
		fmt.Printf("SKIP %s — provider verification failed: %s\n", input, err)
		os.Exit(1)
	}
	prepared, err := processPDF(input, *name, *source, verification.DetectPath, verification.ConvertPath,
		now, *timeout, *maxInputBytes, runPDFTool)
	if err != nil {
		recordPDFIngest(o, args, now, start, "", 1, 0)
		fmt.Printf("SKIP %s — %s\n", input, err)
		os.Exit(1)
	}
	r := prepared.result

	if err := withWriteLock(o.out, func() error {
		outPath := filepath.Join(o.out, r.Path)
		if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
			return fmt.Errorf("create PDF output directory: %w", err)
		}
		unchanged, err := writeFileIfChanged(outPath, prepared.body)
		if err != nil {
			return fmt.Errorf("write PDF Markdown: %w", err)
		}
		r.Unchanged = unchanged
		if err := writeManifests(o.out, []result{r}, false, nil); err != nil {
			return err
		}
		if err := regenerateIndex(o.out, []string{r.Source}); err != nil {
			return err
		}
		idx, err := openFTSIndex(o.out)
		if err != nil {
			return err
		}
		defer idx.close()
		if err := idx.updateFTS(o.out, []string{r.Path}); err != nil {
			return err
		}
		return appendIngestLog(o.out, logEntry{
			StartedAt:  now,
			FinishedAt: time.Now().UTC().Format(time.RFC3339),
			ElapsedMs:  time.Since(start).Milliseconds(),
			Mode:       "pull-pdf",
			Args:       append([]string{"pull-pdf"}, args...),
			Sources:    []string{r.Source},
			URLs:       1,
			Pulled:     1,
			Unchanged:  boolInt(r.Unchanged),
		})
	}); err != nil {
		die(err)
	}

	fmt.Printf("pulled %s\n  → %s (%s %s)\n", input, r.Path, r.Mode, "text_based")
}

func normalizePDFArgs(args []string) ([]string, error) {
	return normalizeSinglePositionalArgs(args, map[string]bool{
		"name":            true,
		"source":          true,
		"detect-pdf":      true,
		"pdf2md":          true,
		"provider-pin":    true,
		"timeout":         true,
		"max-input-bytes": true,
		"out":             true,
		"source-cache":    true,
		"concurrency":     true,
	}, "local PDF path")
}

func processPDF(input, name, source, detectBinary, convertBinary string,
	now string, timeout time.Duration, maxInputBytes int64, runner pdfToolRunner) (preparedPDF, error) {
	abs, err := filepath.Abs(input)
	if err != nil {
		return preparedPDF{}, fmt.Errorf("resolve PDF path: %w", err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return preparedPDF{}, fmt.Errorf("read PDF path: %w", err)
	}
	if !info.Mode().IsRegular() {
		return preparedPDF{}, fmt.Errorf("input is not a regular file")
	}
	if info.Size() == 0 {
		return preparedPDF{}, fmt.Errorf("input is empty")
	}
	if info.Size() > maxInputBytes {
		return preparedPDF{}, fmt.Errorf("input is %d bytes, above --max-input-bytes %d", info.Size(), maxInputBytes)
	}
	if runner == nil {
		return preparedPDF{}, fmt.Errorf("PDF provider runner is nil")
	}

	outputSource := sanitizeSourceName(source)
	docName := name
	if docName == "" {
		docName = strings.TrimSuffix(filepath.Base(abs), filepath.Ext(abs))
	}
	docName = sanitizeSourceName(docName)

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	detectData, err := runner(ctx, detectBinary, abs, "--json")
	if err != nil {
		return preparedPDF{}, fmt.Errorf("pdf detection: %w", err)
	}
	var detection pdfDetectionOutput
	if err := json.Unmarshal(detectData, &detection); err != nil {
		return preparedPDF{}, fmt.Errorf("pdf detection returned invalid JSON: %w", err)
	}
	if detection.Error != "" {
		return preparedPDF{}, fmt.Errorf("pdf detection failed: %s", detection.Error)
	}
	if detection.PDFType == "" {
		return preparedPDF{}, fmt.Errorf("pdf detection returned no pdf_type")
	}
	if detection.PDFType != "text_based" {
		return preparedPDF{}, fmt.Errorf("pdf-inspector classified input as %s; use an explicit OCR path", detection.PDFType)
	}

	markdownData, err := runner(ctx, convertBinary, abs, "--json", "--compact", "--pages")
	if err != nil {
		return preparedPDF{}, fmt.Errorf("pdf conversion: %w", err)
	}
	var converted pdfMarkdownOutput
	if err := json.Unmarshal(markdownData, &converted); err != nil {
		return preparedPDF{}, fmt.Errorf("pdf conversion returned invalid JSON: %w", err)
	}
	if converted.Error != "" {
		return preparedPDF{}, fmt.Errorf("pdf conversion failed: %s", converted.Error)
	}
	if converted.PDFType != "" && converted.PDFType != "text_based" {
		return preparedPDF{}, fmt.Errorf("pdf conversion returned unsupported type %s", converted.PDFType)
	}
	if strings.TrimSpace(converted.Markdown) == "" {
		return preparedPDF{}, fmt.Errorf("pdf conversion returned empty Markdown")
	}

	rel := filepath.Join(outputSource, docName+".md")
	body := []byte(converted.Markdown)
	sum := sha256.Sum256(body)
	fileURL := (&url.URL{Scheme: "file", Path: abs}).String()
	return preparedPDF{
		result: result{
			URL:       fileURL,
			Source:    outputSource,
			Path:      filepath.ToSlash(rel),
			Mode:      "pdf-inspector",
			SHA256:    hex.EncodeToString(sum[:]),
			FetchedAt: now,
		},
		body: body,
	}, nil
}

func recordPDFIngest(o pullOpts, args []string, startedAt string, start time.Time, source string, skipped, pulled int) {
	_ = withWriteLock(o.out, func() error {
		return appendIngestLog(o.out, logEntry{
			StartedAt:  startedAt,
			FinishedAt: time.Now().UTC().Format(time.RFC3339),
			ElapsedMs:  time.Since(start).Milliseconds(),
			Mode:       "pull-pdf",
			Args:       append([]string{"pull-pdf"}, args...),
			Sources:    nonEmptyStrings(source),
			URLs:       1,
			Pulled:     pulled,
			Skipped:    skipped,
		})
	})
}

func nonEmptyStrings(value string) []string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return []string{value}
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
