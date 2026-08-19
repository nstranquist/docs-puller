package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestWriteAndVerifyPDFProviderPin(t *testing.T) {
	dir := t.TempDir()
	detect := filepath.Join(dir, "detect-pdf")
	convert := filepath.Join(dir, "pdf2md")
	if err := os.WriteFile(detect, []byte("detect provider"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(convert, []byte("convert provider"), 0o755); err != nil {
		t.Fatal(err)
	}
	pinPath := filepath.Join(dir, "provider-pin.json")
	source := pdfProviderSource{
		Repository: pdfProviderRepository,
		Revision:   "ae6246b",
		Version:    "0.1.7",
	}
	if err := writePDFProviderPin(pinPath, detect, convert, source, false); err != nil {
		t.Fatal(err)
	}
	verification, err := verifyPDFProvider(pinPath, detect, convert)
	if err != nil {
		t.Fatal(err)
	}
	if !verification.Report.OK || len(verification.Report.Artifacts) != 2 {
		t.Fatalf("verification = %+v", verification.Report)
	}
	if verification.DetectPath != detect || verification.ConvertPath != convert {
		t.Fatalf("resolved paths = %q, %q", verification.DetectPath, verification.ConvertPath)
	}
}

func TestVerifyPDFProviderRejectsChecksumMismatch(t *testing.T) {
	dir := t.TempDir()
	detect := filepath.Join(dir, "detect-pdf")
	convert := filepath.Join(dir, "pdf2md")
	if err := os.WriteFile(detect, []byte("detect provider"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(convert, []byte("convert provider"), 0o755); err != nil {
		t.Fatal(err)
	}
	pinPath := filepath.Join(dir, "provider-pin.json")
	if err := writePDFProviderPin(pinPath, detect, convert, pdfProviderSource{
		Repository: pdfProviderRepository,
		Revision:   "ae6246b",
		Version:    "0.1.7",
	}, false); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(convert, []byte("changed provider"), 0o755); err != nil {
		t.Fatal(err)
	}
	verification, err := verifyPDFProvider(pinPath, detect, convert)
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("err = %v, report = %+v", err, verification.Report)
	}
	if verification.Report.OK {
		t.Fatal("mismatched provider reported as healthy")
	}
}

func TestVerifyPDFProviderRejectsNonExecutable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not expose POSIX executable mode bits")
	}
	dir := t.TempDir()
	detect := filepath.Join(dir, "detect-pdf")
	convert := filepath.Join(dir, "pdf2md")
	if err := os.WriteFile(detect, []byte("detect provider"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(convert, []byte("convert provider"), 0o755); err != nil {
		t.Fatal(err)
	}
	pinPath := filepath.Join(dir, "provider-pin.json")
	if err := writePDFProviderPin(pinPath, detect, convert, pdfProviderSource{
		Repository: pdfProviderRepository,
		Revision:   "ae6246b",
		Version:    "0.1.7",
	}, false); err == nil || !strings.Contains(err.Error(), "not executable") {
		t.Fatalf("err = %v, want non-executable rejection", err)
	}
}

func TestLoadPDFProviderPinRejectsUnknownFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "provider-pin.json")
	body, err := json.Marshal(map[string]any{
		"schema_version": 1,
		"provider":       pdfProviderName,
		"source": map[string]string{
			"repository": pdfProviderRepository,
			"revision":   "ae6246b",
			"version":    "0.1.7",
		},
		"artifacts": map[string]map[string]string{
			"detect-pdf": {"sha256": strings.Repeat("a", 64)},
			"pdf2md":     {"sha256": strings.Repeat("b", 64)},
		},
		"unexpected": true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadPDFProviderPin(path); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("err = %v, want unknown-field rejection", err)
	}
}

func TestLoadPDFProviderPinRejectsTrailingJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "provider-pin.json")
	body := `{"schema_version":1,"provider":"pdf-inspector","source":{"repository":"https://github.com/firecrawl/pdf-inspector","revision":"ae6246b","version":"0.1.7"},"artifacts":{"detect-pdf":{"sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},"pdf2md":{"sha256":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}}} {"extra":true}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadPDFProviderPin(path); err == nil || !strings.Contains(err.Error(), "trailing JSON") {
		t.Fatalf("err = %v, want trailing JSON rejection", err)
	}
}

func TestWritePDFProviderPinRefusesOverwrite(t *testing.T) {
	dir := t.TempDir()
	detect := filepath.Join(dir, "detect-pdf")
	convert := filepath.Join(dir, "pdf2md")
	if err := os.WriteFile(detect, []byte("detect provider"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(convert, []byte("convert provider"), 0o755); err != nil {
		t.Fatal(err)
	}
	pinPath := filepath.Join(dir, "provider-pin.json")
	if err := os.WriteFile(pinPath, []byte("existing\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := writePDFProviderPin(pinPath, detect, convert, pdfProviderSource{
		Repository: pdfProviderRepository,
		Revision:   "ae6246b",
		Version:    "0.1.7",
	}, false)
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("err = %v, want overwrite rejection", err)
	}
}
