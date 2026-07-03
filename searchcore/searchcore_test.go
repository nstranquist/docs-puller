package searchcore

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestParseTopLevelArray(t *testing.T) {
	hits, err := parseHits([]byte(`[{"path":"a.md","score":42}]`))
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Path != "a.md" || hits[0].Score != 42 {
		t.Fatalf("unexpected: %+v", hits)
	}
}

func TestParseWrapperResults(t *testing.T) {
	hits, err := parseHits([]byte(`{"results":[{"path":"b.md","score":7,"snippets":[{"line":4,"text":"snippet body"}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Path != "b.md" {
		t.Fatalf("unexpected: %+v", hits)
	}
	if len(hits[0].Snippets) != 1 || hits[0].Snippets[0].Text != "snippet body" {
		t.Fatalf("snippets not parsed: %+v", hits[0].Snippets)
	}
}

func TestParseWrapperHits(t *testing.T) {
	hits, err := parseHits([]byte(`{"hits":[{"path":"c.md"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Path != "c.md" {
		t.Fatalf("unexpected: %+v", hits)
	}
}

func TestParseEmpty(t *testing.T) {
	hits, err := parseHits([]byte(`   `))
	if err != nil || len(hits) != 0 {
		t.Fatalf("expected empty, got %+v err=%v", hits, err)
	}
}

func TestExecAdapterInvokesBinary(t *testing.T) {
	bin := writeFakeBinary(t, `#!/bin/sh
echo "$@" > "$ARGS_FILE"
cat <<JSON
[{"path":"hit.md","title":"Hit","score":99,"source":"fake"}]
JSON
`)
	argsFile := filepath.Join(t.TempDir(), "args.txt")
	t.Setenv("ARGS_FILE", argsFile)
	a := NewExecAdapter(bin)
	hits, err := a.Search(context.Background(), Query{Text: "hello world", Limit: 5, Source: "fake", Rerank: true})
	if err != nil {
		t.Fatal(err)
	}
	want := []Hit{{Path: "hit.md", Title: "Hit", Score: 99, Source: "fake"}}
	if !reflect.DeepEqual(hits, want) {
		t.Fatalf("got %+v want %+v", hits, want)
	}
	gotArgs, _ := os.ReadFile(argsFile)
	for _, want := range []string{"search", "--json", "hello world", "--limit", "5", "--source", "fake", "--rerank-llm"} {
		if !strings.Contains(string(gotArgs), want) {
			t.Fatalf("missing arg %q in %q", want, gotArgs)
		}
	}
}

func TestExecAdapterRequiresQuery(t *testing.T) {
	a := NewExecAdapter("/usr/bin/false")
	_, err := a.Search(context.Background(), Query{})
	if err == nil || !strings.Contains(err.Error(), "query text is required") {
		t.Fatalf("expected query-required error, got %v", err)
	}
}

func TestExecAdapterSurfacesBinaryFailure(t *testing.T) {
	bin := writeFakeBinary(t, `#!/bin/sh
echo "boom" >&2
exit 3
`)
	a := NewExecAdapter(bin)
	_, err := a.Search(context.Background(), Query{Text: "x"})
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("expected stderr capture, got %v", err)
	}
}

func TestStubSearcherCapturesQuery(t *testing.T) {
	stub := &StubSearcher{Hits: []Hit{{Path: "a.md"}}}
	hits, err := stub.Search(context.Background(), Query{Text: "row level security", Limit: 3, Rerank: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Path != "a.md" {
		t.Fatalf("unexpected: %+v", hits)
	}
	if stub.Last.Text != "row level security" || stub.Last.Limit != 3 || !stub.Last.Rerank {
		t.Fatalf("query not captured: %+v", stub.Last)
	}
}

func writeFakeBinary(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "fake-docs-puller")
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}
