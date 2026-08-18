package main

import (
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
)

// FuzzFtsBuildQuery drives the FTS5 query-construction surface with arbitrary
// user search input. ftsBuildQuery and its title/path/source-scoped siblings
// turn an untrusted query string into an FTS5 MATCH expression; a builder that
// leaked an unescaped operator or an unbalanced quote would turn a search box
// into an FTS5 syntax error at best and altered match semantics at worst.
//
// Invariants: no panic for any input, and every non-empty expression the
// builders emit is a syntactically valid FTS5 MATCH — validated by running it
// against a real in-memory FTS5 table (modernc.org/sqlite has FTS5 compiled in,
// no cgo). A single connection is pinned so the virtual table created on the
// first statement stays visible to every MATCH probe.
func FuzzFtsBuildQuery(f *testing.F) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		f.Fatalf("open sqlite: %v", err)
	}
	db.SetMaxOpenConns(1)
	f.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(`CREATE VIRTUAL TABLE docs USING fts5(body, title, path_tokens, tokenize='porter unicode61')`); err != nil {
		f.Fatalf("create fts5 table: %v", err)
	}

	seeds := []string{
		"row level security",
		"how do I upload files to Supabase storage",
		`"phrase" NEAR(x y)`,
		"foo*",
		"--my-flag",
		"supabase rls",
		"a AND b OR c",
		`unbalanced " quote`,
		"col: ( )",
		"NEAR(",
		"",
		"   ",
		"日本語 tokenizer",
	}
	for _, s := range seeds {
		f.Add(s, false, "supabase")
		f.Add(s, true, `source" OR *`)
	}

	validate := func(t *testing.T, label, expr string) {
		if expr == "" {
			return
		}
		// A syntactically invalid FTS5 expression fails at MATCH-compile time
		// ("fts5: syntax error near ..."); a valid one returns a count of 0.
		var n int
		if err := db.QueryRow(`SELECT count(*) FROM docs WHERE docs MATCH ?`, expr).Scan(&n); err != nil {
			t.Fatalf("%s emitted an invalid FTS5 MATCH expr %q: %v", label, expr, err)
		}
	}

	f.Fuzz(func(t *testing.T, q string, exact bool, source string) {
		validate(t, "ftsBuildQuery", ftsBuildQuery(q, exact))
		validate(t, "ftsBuildSourceScopedQuery", ftsBuildSourceScopedQuery(q, exact, source))
		titleQ, _ := ftsBuildTitleQuery(q, exact)
		validate(t, "ftsBuildTitleQuery", titleQ)
		pathQ, _ := ftsBuildPathQuery(q, exact)
		validate(t, "ftsBuildPathQuery", pathQ)
	})
}
