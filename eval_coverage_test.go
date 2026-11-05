package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFixtureCoverageAlternatives(t *testing.T) {
	root := t.TempDir()
	os.Mkdir(filepath.Join(root, "demo"), 0700)
	os.WriteFile(filepath.Join(root, "demo", "a.md"), []byte("content"), 0600)
	fix := &evalFixture{Queries: []evalQuery{{Q: "one", Expect: []string{"demo/a.md", "demo/missing.md"}}, {Q: "two", Expect: []string{"demo/gone.md"}}}}
	c := fixtureCoverage(fix, root)
	if c.ExpectedPaths != 3 || c.MissingPaths != 2 || c.QueriesWithoutAvailableTarget != 1 {
		t.Fatalf("%+v", c)
	}
}
