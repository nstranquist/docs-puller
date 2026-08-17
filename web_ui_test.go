package main

import (
	"strings"
	"testing"
)

func TestWebUIIsLocalSearchSurface(t *testing.T) {
	b, err := webFS.ReadFile("web/index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(b)
	for _, want := range []string{
		"<title>docs-puller</title>",
		"Search docs on this machine.",
		"/api/status",
		"/api/search",
		"data-theme",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("web UI missing %q", want)
		}
	}
	if strings.Contains(html, "/Users/") {
		t.Fatal("web UI must not embed a home path")
	}
}
