package main

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

func TestLoadLLMsTxtCombinedSections(t *testing.T) {
	var base string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("===///overview===\n# Overview\n\n===/build/overview===\n# Grok Build\n\n===/build/overview===\n# duplicate\n"))
	}))
	defer srv.Close()
	base = srv.URL

	got, err := loadLLMsTxt(base + "/llms.txt")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{base + "/overview.md", base + "/build/overview.md"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("loadLLMsTxt() = %v, want %v", got, want)
	}
}

func TestLoadLLMsTxtLinkIndexFallback(t *testing.T) {
	var base string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("# Docs\n\n- " + base + "/one.md\n- " + base + "/two.md\n"))
	}))
	defer srv.Close()
	base = srv.URL

	got, err := loadLLMsTxt(base + "/llms.txt")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{base + "/one.md", base + "/two.md"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("loadLLMsTxt() = %v, want %v", got, want)
	}
}

func TestLoadLLMsTxtRejectsEmptyCorpus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("# no links here\n"))
	}))
	defer srv.Close()

	if _, err := loadLLMsTxt(srv.URL + "/llms.txt"); err == nil {
		t.Fatal("expected empty llms.txt to fail")
	}
}
