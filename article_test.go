package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

func TestFetchRedditThreadRejectsEmptyListings(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/r/nicos/comments/abc/thread.json" {
			t.Errorf("path = %q, want reddit json path", r.URL.Path)
		}
		if r.URL.RawQuery != "raw_json=1" {
			t.Errorf("query = %q, want raw_json=1", r.URL.RawQuery)
		}
		n, err := w.Write([]byte(`[{"data":{"children":[]}},{"data":{"children":[]}}]`))
		if err != nil || n == 0 {
			t.Errorf("write reddit fixture bytes=%d err=%v", n, err)
		}
	}))
	defer server.Close()

	target, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	origClient := httpClient
	httpClient = &http.Client{Transport: &rewriteTransport{target: target, inner: http.DefaultTransport}}
	defer func() { httpClient = origClient }()

	threadURL, err := url.Parse("https://reddit.com/r/nicos/comments/abc/thread")
	if err != nil {
		t.Fatal(err)
	}
	got, err := fetchRedditThread(threadURL)
	if err == nil {
		t.Fatal("expected empty Reddit listing error")
	}
	if len(got) != 0 {
		t.Fatalf("markdown = %q, want empty on error", string(got))
	}
	if want := "reddit json: empty listings"; err.Error() != want {
		t.Fatalf("error = %q, want %q", err.Error(), want)
	}
}
