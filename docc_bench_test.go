package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// Benchmarks for the DocC pipeline. Run with:
//
//	go test -run='^$' -bench=BenchmarkDocC -benchmem ./cmd/docs-puller
//
// Three fixtures with different shapes:
//   - desktop.json (12 KB)         — small leaf symbol, 11 properties
//   - profile-…-keys.json (88 KB)  — collection page, 30 topic groups
//   - swift-array.json (233 KB)    — large symbol page, 31 topics + 43 conformances + huge refs dict
//
// We benchmark the two hot paths separately:
//   - parse: json.Unmarshal into doccNode
//   - render: doccNode → markdown
// And the combined parse-then-render to capture realistic ingest cost.

func loadBenchBytes(b *testing.B, name string) []byte {
	b.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "docc", name))
	if err != nil {
		b.Fatalf("read fixture %s: %v", name, err)
	}
	return data
}

func loadBenchNode(b *testing.B, name string) *doccNode {
	b.Helper()
	var n doccNode
	if err := json.Unmarshal(loadBenchBytes(b, name), &n); err != nil {
		b.Fatalf("parse %s: %v", name, err)
	}
	return &n
}

// ─────────────── Parse ───────────────

func BenchmarkDocCParseDesktop(b *testing.B) {
	data := loadBenchBytes(b, "desktop.json")
	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var n doccNode
		if err := json.Unmarshal(data, &n); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkDocCParseCollection(b *testing.B) {
	data := loadBenchBytes(b, "profile-specific-payload-keys.json")
	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var n doccNode
		if err := json.Unmarshal(data, &n); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkDocCParseSwiftArray(b *testing.B) {
	data := loadBenchBytes(b, "swift-array.json")
	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var n doccNode
		if err := json.Unmarshal(data, &n); err != nil {
			b.Fatal(err)
		}
	}
}

// ─────────────── Render ───────────────

func BenchmarkDocCRenderDesktop(b *testing.B) {
	n := loadBenchNode(b, "desktop.json")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = renderDocC(n)
	}
}

func BenchmarkDocCRenderCollection(b *testing.B) {
	n := loadBenchNode(b, "profile-specific-payload-keys.json")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = renderDocC(n)
	}
}

func BenchmarkDocCRenderSwiftArray(b *testing.B) {
	n := loadBenchNode(b, "swift-array.json")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = renderDocC(n)
	}
}

// ─────────────── Combined parse + render (realistic ingest) ───────────────

func BenchmarkDocCParseRenderDesktop(b *testing.B) {
	data := loadBenchBytes(b, "desktop.json")
	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var n doccNode
		if err := json.Unmarshal(data, &n); err != nil {
			b.Fatal(err)
		}
		_ = renderDocC(&n)
	}
}

func BenchmarkDocCParseRenderCollection(b *testing.B) {
	data := loadBenchBytes(b, "profile-specific-payload-keys.json")
	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var n doccNode
		if err := json.Unmarshal(data, &n); err != nil {
			b.Fatal(err)
		}
		_ = renderDocC(&n)
	}
}

func BenchmarkDocCParseRenderSwiftArray(b *testing.B) {
	data := loadBenchBytes(b, "swift-array.json")
	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var n doccNode
		if err := json.Unmarshal(data, &n); err != nil {
			b.Fatal(err)
		}
		_ = renderDocC(&n)
	}
}

// ─────────────── Inline-only micro ───────────────

func BenchmarkDocCRenderInlines(b *testing.B) {
	n := loadBenchNode(b, "swift-array.json")
	abstracts := [][]doccInline{n.Abstract}
	for _, ref := range n.References {
		if len(ref.Abstract) > 0 {
			abstracts = append(abstracts, ref.Abstract)
		}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for _, a := range abstracts {
			_ = renderInlines(a, n.References)
		}
	}
}
