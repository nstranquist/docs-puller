package main

import (
	"os"
	"path/filepath"
	"testing"
)

func writeTestFile(t *testing.T, root, rel, body string) {
	t.Helper()
	path := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func docsByRel(docs []crawlDoc) map[string]string {
	out := map[string]string{}
	for _, d := range docs {
		if d.Content != nil {
			out[d.Rel] = string(d.Content)
			continue
		}
		if d.ReadFrom != "" {
			data, err := os.ReadFile(d.ReadFrom)
			if err == nil {
				out[d.Rel] = string(data)
			}
		}
	}
	return out
}
