package main

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestNormalizePDFArgsAllowsPathBeforeFlags(t *testing.T) {
	got, err := normalizePDFArgs([]string{
		"manual.pdf", "--name", "manual", "--out", "/tmp/docs", "--timeout=2s",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"--name", "manual", "--out", "/tmp/docs", "--timeout=2s", "manual.pdf"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("normalized args = %#v, want %#v", got, want)
	}
}

func TestNormalizePDFArgsRejectsMultiplePaths(t *testing.T) {
	_, err := normalizePDFArgs([]string{"one.pdf", "two.pdf"})
	if err == nil || !strings.Contains(err.Error(), "one local PDF path") {
		t.Fatalf("err = %v, want one-path error", err)
	}
}

func TestProcessPDFTextBasedUsesDetectionBeforeConversion(t *testing.T) {
	dir := t.TempDir()
	input := filepath.Join(dir, "Firecrawl manual.pdf")
	if err := os.WriteFile(input, []byte("%PDF-1.7 test"), 0o644); err != nil {
		t.Fatal(err)
	}

	var calls []string
	runner := func(_ context.Context, binary string, args ...string) ([]byte, error) {
		calls = append(calls, binary+" "+strings.Join(args, " "))
		switch binary {
		case "detect-pdf":
			return []byte(`{"pdf_type":"text_based"}`), nil
		case "pdf2md":
			return []byte(`{"pdf_type":"text_based","markdown":"# Firecrawl manual\n\nText."}`), nil
		default:
			t.Fatalf("unexpected provider binary %q", binary)
			return nil, nil
		}
	}

	got, err := processPDF(input, "", "pdf-docs", "detect-pdf", "pdf2md",
		"2026-08-04T19:00:00Z", time.Second, 1<<20, runner)
	if err != nil {
		t.Fatal(err)
	}
	if got.result.Source != "pdf-docs" || got.result.Path != "pdf-docs/firecrawl-manual.md" {
		t.Fatalf("result = %+v", got.result)
	}
	if string(got.body) != "# Firecrawl manual\n\nText." {
		t.Fatalf("body = %q", got.body)
	}
	if len(calls) != 2 || !strings.HasPrefix(calls[0], "detect-pdf ") || !strings.HasPrefix(calls[1], "pdf2md ") {
		t.Fatalf("provider call order = %v", calls)
	}
	if !strings.Contains(calls[1], "--compact") || !strings.Contains(calls[1], "--pages") {
		t.Fatalf("conversion args = %v", calls[1])
	}
}

func TestProcessPDFRejectsNonTextBeforeConversion(t *testing.T) {
	for _, pdfType := range []string{"scanned", "image_based", "mixed"} {
		t.Run(pdfType, func(t *testing.T) {
			dir := t.TempDir()
			input := filepath.Join(dir, "input.pdf")
			if err := os.WriteFile(input, []byte("%PDF test"), 0o644); err != nil {
				t.Fatal(err)
			}
			converted := false
			runner := func(_ context.Context, binary string, _ ...string) ([]byte, error) {
				if binary == "pdf2md" {
					converted = true
				}
				return []byte(`{"pdf_type":"` + pdfType + `"}`), nil
			}
			_, err := processPDF(input, "input", "pdf-docs", "detect-pdf", "pdf2md",
				"2026-08-04T19:00:00Z", time.Second, 1<<20, runner)
			if err == nil || !strings.Contains(err.Error(), "explicit OCR path") {
				t.Fatalf("err = %v, want explicit OCR rejection", err)
			}
			if converted {
				t.Fatal("converter ran for a non-text PDF")
			}
		})
	}
}

func TestProcessPDFRejectsProviderErrorAndMalformedJSON(t *testing.T) {
	dir := t.TempDir()
	input := filepath.Join(dir, "input.pdf")
	if err := os.WriteFile(input, []byte("%PDF test"), 0o644); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		body []byte
		want string
	}{
		{name: "encrypted", body: []byte(`{"error":"PDF is encrypted"}`), want: "PDF is encrypted"},
		{name: "malformed", body: []byte("not json"), want: "invalid JSON"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := processPDF(input, "input", "pdf-docs", "detect-pdf", "pdf2md",
				"2026-08-04T19:00:00Z", time.Second, 1<<20,
				func(context.Context, string, ...string) ([]byte, error) { return tc.body, nil })
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestProcessPDFRejectsOversizedInput(t *testing.T) {
	dir := t.TempDir()
	input := filepath.Join(dir, "large.pdf")
	if err := os.WriteFile(input, []byte("0123456789"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := processPDF(input, "large", "pdf-docs", "detect-pdf", "pdf2md",
		"2026-08-04T19:00:00Z", time.Second, 4,
		func(context.Context, string, ...string) ([]byte, error) {
			t.Fatal("provider ran for an oversized input")
			return nil, nil
		})
	if err == nil || !strings.Contains(err.Error(), "above --max-input-bytes") {
		t.Fatalf("err = %v, want size rejection", err)
	}
}

func TestProcessPDFHonorsProviderContext(t *testing.T) {
	dir := t.TempDir()
	input := filepath.Join(dir, "timeout.pdf")
	if err := os.WriteFile(input, []byte("%PDF test"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := processPDF(input, "timeout", "pdf-docs", "detect-pdf", "pdf2md",
		"2026-08-04T19:00:00Z", 5*time.Millisecond, 1<<20,
		func(ctx context.Context, _ string, _ ...string) ([]byte, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		})
	if err == nil || !strings.Contains(err.Error(), "context deadline exceeded") {
		t.Fatalf("err = %v, want context deadline", err)
	}
}

func TestRunPDFToolRejectsMissingExecutable(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "detect-pdf-missing")
	_, err := runPDFTool(context.Background(), missing, "input.pdf", "--json")
	if err == nil || !strings.Contains(err.Error(), "failed") {
		t.Fatalf("err = %v, want provider failure", err)
	}
}
