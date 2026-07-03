package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/nstranquist/docs-puller/internal/userconfig"
)

func TestConfiguredSourceKeywords(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte(`
source_keywords:
  nicos: [nicos]
  nicos-app: [nicos]
  nicos-stack: [nicos]
  nicos-stack-kb: [nicos, kb]
  nicos-workspace: [nicos]
`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DOCS_PULLER_CONFIG", cfgPath)
	userconfig.Reset()

	extra, err := userconfig.ExtraSourceKeywords()
	if err != nil {
		t.Fatal(err)
	}
	for src, kws := range extra {
		for _, kw := range kws {
			if sourceKeywords[kw] == nil {
				sourceKeywords[kw] = map[string]bool{}
			}
			sourceKeywords[kw][src] = true
		}
	}

	srcs := sourcesFromQueryTokens(tokenizeForFTS("nicos workspace layout"))
	for _, want := range []string{"nicos", "nicos-app", "nicos-stack", "nicos-stack-kb", "nicos-workspace"} {
		if !srcs[want] {
			t.Fatalf("nicos query should source-boost %s; got %+v", want, srcs)
		}
	}
}
