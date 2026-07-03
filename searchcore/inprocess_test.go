package searchcore

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestInProcessSearcherDispatchesQuery(t *testing.T) {
	want := []Hit{{Path: "hit.md", Title: "Hit", Score: 9}}
	var got Query
	searcher := NewInProcessSearcher(func(_ context.Context, q Query) ([]Hit, error) {
		got = q
		return want, nil
	})

	hits, err := searcher.Search(context.Background(), Query{Text: "row level security", Limit: 5, Source: "supabase", Rerank: true})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(hits, want) {
		t.Fatalf("got %+v want %+v", hits, want)
	}
	if got.Text != "row level security" || got.Limit != 5 || got.Source != "supabase" || !got.Rerank {
		t.Fatalf("query not dispatched intact: %+v", got)
	}
}

func TestInProcessSearcherRequiresQuery(t *testing.T) {
	searcher := NewInProcessSearcher(func(_ context.Context, _ Query) ([]Hit, error) {
		t.Fatal("dispatch should not be called")
		return nil, nil
	})
	_, err := searcher.Search(context.Background(), Query{Text: "   "})
	if err == nil || !strings.Contains(err.Error(), "query text is required") {
		t.Fatalf("expected query-required error, got %v", err)
	}
}

func TestInProcessSearcherRequiresDispatch(t *testing.T) {
	searcher := NewInProcessSearcher(nil)
	_, err := searcher.Search(context.Background(), Query{Text: "needle"})
	if err == nil || !strings.Contains(err.Error(), "dispatcher is required") {
		t.Fatalf("expected dispatcher-required error, got %v", err)
	}
}

func TestInProcessSearcherSurfacesDispatchError(t *testing.T) {
	wantErr := errors.New("boom")
	searcher := NewInProcessSearcher(func(_ context.Context, _ Query) ([]Hit, error) {
		return nil, wantErr
	})
	_, err := searcher.Search(context.Background(), Query{Text: "needle"})
	if !errors.Is(err, wantErr) {
		t.Fatalf("expected %v, got %v", wantErr, err)
	}
}

func TestInProcessSearcherRespectsCanceledContext(t *testing.T) {
	searcher := NewInProcessSearcher(func(_ context.Context, _ Query) ([]Hit, error) {
		t.Fatal("dispatch should not be called")
		return nil, nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := searcher.Search(ctx, Query{Text: "needle"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context canceled, got %v", err)
	}
}
