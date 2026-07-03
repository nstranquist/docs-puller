package main

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"testing"
)

func TestExtractGatsbySlugsDedupesAndSkipsEmpty(t *testing.T) {
	body := []byte(`{"data":{"allMdx":{"nodes":[
		{"slug":"docs/a"},
		{"slug":"docs/b"},
		{"slug":"docs/a"},
		{"slug":"  "},
		{"slug":""}
	]}}}`)
	got := extractGatsbySlugs(body)
	want := []string{"docs/a", "docs/b"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestExtractGatsbySlugsDropsPrivateSegments(t *testing.T) {
	body := []byte(`{"data":{"allMdx":{"nodes":[
		{"slug":"docs/foo"},
		{"slug":"docs/_snippets/bar"},
		{"slug":"_drafts/never-published"},
		{"slug":"handbook/values"}
	]}}}`)
	got := extractGatsbySlugs(body)
	sort.Strings(got)
	want := []string{"docs/foo", "handbook/values"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestExtractGatsbySlugsReturnsNilWhenNoAllMdx(t *testing.T) {
	cases := [][]byte{
		[]byte(`{"data":{"allCloudinaryImage":{"nodes":[{"id":"x"}]}}}`),
		[]byte(`{"data":{"allMdx":{"nodes":[]}}}`),
		[]byte(`not json`),
	}
	for i, body := range cases {
		if got := extractGatsbySlugs(body); got != nil {
			t.Errorf("case %d: expected nil, got %v", i, got)
		}
	}
}

// TestLoadGatsbyPageDataPicksLargestQuery models PostHog's reality: the page-data.json
// references many static-query hashes, only some of which contain allMdx. We must
// pick the one with the most slugs (PostHog ships duplicates with identical sets).
func TestLoadGatsbyPageDataPicksLargestQuery(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/page-data/docs/page-data.json", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"staticQueryHashes":["small","big","unrelated"]}`))
	})
	mux.HandleFunc("/page-data/sq/d/small.json", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"data":{"allMdx":{"nodes":[{"slug":"docs/only"}]}}}`))
	})
	mux.HandleFunc("/page-data/sq/d/big.json", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"data":{"allMdx":{"nodes":[
			{"slug":"docs/a"},{"slug":"docs/b"},{"slug":"handbook/c"}
		]}}}`))
	})
	mux.HandleFunc("/page-data/sq/d/unrelated.json", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"data":{"allCloudinaryImage":{"nodes":[]}}}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	got, err := loadGatsbyPageData(srv.URL + "/page-data/docs/page-data.json")
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(got)
	want := []string{"docs/a", "docs/b", "handbook/c"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestLoadGatsbyPageDataErrorsWhenNoAllMdx(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/page-data/docs/page-data.json", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"staticQueryHashes":["a"]}`))
	})
	mux.HandleFunc("/page-data/sq/d/a.json", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"data":{"allCloudinaryImage":{"nodes":[]}}}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	got, err := loadGatsbyPageData(srv.URL + "/page-data/docs/page-data.json")
	if err == nil {
		t.Fatal("expected error when no static query has allMdx")
	}
	if len(got) != 0 {
		t.Fatalf("slugs = %v, want none on error", got)
	}
	if want := "no allMdx.nodes[].slug found across 1 static queries"; err.Error() != want {
		t.Fatalf("error = %q, want %q", err.Error(), want)
	}
}

func TestLoadGatsbyPageDataErrorsWhenNoStaticQueryHashes(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/page-data/docs/page-data.json", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	rawURL := srv.URL + "/page-data/docs/page-data.json"
	got, err := loadGatsbyPageData(rawURL)
	if err == nil {
		t.Fatal("expected error when page-data has no staticQueryHashes")
	}
	if len(got) != 0 {
		t.Fatalf("slugs = %v, want none on error", got)
	}
	if want := "no staticQueryHashes in " + rawURL; err.Error() != want {
		t.Fatalf("error = %q, want %q", err.Error(), want)
	}
}

func TestGatsbySlugsToURLs(t *testing.T) {
	got, err := gatsbySlugsToURLs(
		"https://posthog.com/page-data/docs/page-data.json",
		[]string{"docs/a", "/handbook/b"},
	)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"https://posthog.com/docs/a", "https://posthog.com/handbook/b"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestHasPrivateSegment(t *testing.T) {
	cases := map[string]bool{
		"docs/foo":               false,
		"docs/_snippets/foo":     true,
		"_drafts/foo":            true,
		"docs/foo/_partial/bar":  true,
		"":                       false,
		"docs/under_score-is-ok": false, // underscore mid-segment is fine
	}
	for slug, want := range cases {
		if got := hasPrivateSegment(slug); got != want {
			t.Errorf("hasPrivateSegment(%q) = %v, want %v", slug, got, want)
		}
	}
}
