package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestScanSQLObjects(t *testing.T) {
	const sql = `
-- 20251013_initial.sql
create table "public"."hits" (
    "id" bigint not null,
    "path" character varying not null
);

alter table "public"."hits" enable row level security;

create table public.organizations (
    id uuid not null,
    name text not null
);

create unique index hits_pkey on public.hits using btree (id);
create unique index "organizations_slug_key" on "public"."organizations" using btree (slug);

CREATE POLICY "select_own_hits" ON "public"."hits"
  FOR SELECT TO authenticated
  USING (true);

alter table only public.organizations add constraint organizations_pkey primary key (id);

drop table if exists public.legacy_table;
`
	tables, policies, indexes := scanSQLObjects([]byte(sql))

	wantTables := map[string][]string{
		"public.hits":          {"CREATE", "ENABLE RLS"},
		"public.organizations": {"CREATE", "ALTER"},
		"public.legacy_table":  {"DROP"},
	}
	if len(tables) != len(wantTables) {
		t.Fatalf("got %d tables, want %d: %+v", len(tables), len(wantTables), tables)
	}
	for _, tt := range tables {
		want, ok := wantTables[tt.qualified]
		if !ok {
			t.Errorf("unexpected table %q", tt.qualified)
			continue
		}
		gotJoined := strings.Join(tt.actions, ",")
		wantJoined := strings.Join(want, ",")
		if gotJoined != wantJoined {
			t.Errorf("table %q actions = %v, want %v", tt.qualified, tt.actions, want)
		}
	}

	if len(policies) != 1 || policies[0].name != "select_own_hits" || policies[0].table != "public.hits" {
		t.Errorf("policies: got %+v", policies)
	}

	wantIdx := map[string]string{
		"hits_pkey":              "public.hits",
		"organizations_slug_key": "public.organizations",
	}
	if len(indexes) != len(wantIdx) {
		t.Fatalf("got %d indexes, want %d: %+v", len(indexes), len(wantIdx), indexes)
	}
	for _, idx := range indexes {
		if wantIdx[idx.name] != idx.table {
			t.Errorf("index %q: table=%q want=%q", idx.name, idx.table, wantIdx[idx.name])
		}
	}
}

func TestScanSQLObjects_IgnoresCommentedDDL(t *testing.T) {
	const sql = `
-- create table public.fake_a (id int);
/* create table public.fake_b (id int); */
create table public.real (id int);
`
	tables, _, _ := scanSQLObjects([]byte(sql))
	if len(tables) != 1 || tables[0].qualified != "public.real" {
		t.Errorf("expected only public.real, got %+v", tables)
	}
}

func TestRenderSQLMigrationMarkdown_HasTableAndPolicyHeaders(t *testing.T) {
	sql := []byte(`create table public.users (id int);
alter table public.users enable row level security;
create policy "users_self_read" on public.users for select using (true);
create unique index users_pkey on public.users (id);
`)
	got := string(renderSQLMigrationMarkdown("20260101000000_users.sql", sql))
	for _, want := range []string{
		"# Migration: 20260101000000_users",
		"Tables touched",
		"public.users — CREATE, ENABLE RLS",
		"RLS policies",
		"`users_self_read` on public.users",
		"Indexes",
		"`users_pkey` on public.users",
		"```sql",
		"create table public.users",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("rendered markdown missing %q\n--- markdown ---\n%s", want, got)
		}
	}
}

func TestCollectTableStates_AggregatesAcrossMigrations(t *testing.T) {
	dir := t.TempDir()
	mkFile := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Two migrations, the second one alters the table from the first
	// and adds a policy. Filenames are out-of-order on disk; chronological
	// sort by timestamp prefix should fix that.
	mkFile("20260201000000_alter.sql", `
alter table public.users add column display_name text;
create policy "users_self_read" on public.users for select using (true);
`)
	mkFile("20260101000000_init.sql", `
create table public.users (id int);
alter table public.users enable row level security;
`)
	mkFile("99_random.sql", `
create unique index users_pkey on public.users (id);
`)

	states, err := collectTableStates(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	st, ok := states["public.users"]
	if !ok {
		t.Fatalf("expected public.users in states: %+v", states)
	}
	// Only migrations that touch the table itself (CREATE / ALTER / DROP /
	// ENABLE RLS) count as a migration touch. "99_random.sql" only creates
	// an index, which is recorded under st.indexes but not migrations.
	wantOrder := []string{"20260101000000_init", "20260201000000_alter"}
	if len(st.migrations) != len(wantOrder) {
		t.Fatalf("got %d migration touches, want %d: %+v", len(st.migrations), len(wantOrder), st.migrations)
	}
	for i, m := range st.migrations {
		if m.base != wantOrder[i] {
			t.Errorf("migrations[%d] = %q, want %q", i, m.base, wantOrder[i])
		}
	}
	if st.indexes[0].migration != "99_random" {
		t.Errorf("index migration should be 99_random (chronologically last), got %q", st.indexes[0].migration)
	}
	if len(st.policies) != 1 || st.policies[0].name != "users_self_read" {
		t.Errorf("policies = %+v", st.policies)
	}
	if st.policies[0].migration != "20260201000000_alter" {
		t.Errorf("policy migration = %q, want 20260201000000_alter", st.policies[0].migration)
	}
	if len(st.indexes) != 1 || st.indexes[0].name != "users_pkey" {
		t.Errorf("indexes = %+v", st.indexes)
	}
}

func TestRenderTableMarkdown_HasAllSections(t *testing.T) {
	st := &tableState{
		qualified: "public.users",
		migrations: []migrationTouch{
			{base: "20260101000000_init", actions: []string{"CREATE", "ENABLE RLS"}},
			{base: "20260201000000_alter", actions: []string{"ALTER"}},
		},
		policies: []tablePolicyEntry{{name: "users_self_read", migration: "20260201000000_alter"}},
		indexes:  []tableIndexEntry{{name: "users_pkey", migration: "20260101000000_init"}},
	}
	got := string(renderTableMarkdown(st))
	for _, want := range []string{
		"# users — schema for table public.users",
		"history for the `users` table (`public.users`)",
		"Introduced in: `20260101000000_init`",
		"Last touched in: `20260201000000_alter`",
		"## Migrations (2)",
		"`20260101000000_init` — CREATE, ENABLE RLS",
		"## RLS policies (1)",
		"`users_self_read` (added in `20260201000000_alter`)",
		"## Indexes (1)",
		"`users_pkey` (added in `20260101000000_init`)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q\n--- got ---\n%s", want, got)
		}
	}
}

func TestApplyMigrationToTables_AlterColumnEvents(t *testing.T) {
	states := map[string]*tableState{}

	applyMigrationToTables(states, stripSQLComments(`
create table "public"."earnings" (
    "type" text default 'regular',
    "user_id" bigint
);
`), "20260101_init")

	applyMigrationToTables(states, stripSQLComments(`
alter table "public"."earnings" alter column "type" drop default;
alter table "public"."earnings" alter column "type" type "earnings_type" using type::text::earnings_type;
alter table "public"."earnings" alter column "type" set default 'regular'::earnings_type;
alter table "public"."earnings" alter column "user_id" drop not null;
alter table public.earnings alter column user_id set not null;
`), "20260201_alter")

	st := states["public.earnings"]
	if st == nil {
		t.Fatal("public.earnings missing")
	}
	var typ, userID *columnDef
	for i := range st.columns {
		switch st.columns[i].name {
		case "type":
			typ = &st.columns[i]
		case "user_id":
			userID = &st.columns[i]
		}
	}
	if typ == nil || userID == nil {
		t.Fatalf("columns missing: %+v", st.columns)
	}
	wantType := []string{
		"20260201_alter: DROP DEFAULT",
		"20260201_alter: TYPE change",
		"20260201_alter: SET DEFAULT",
	}
	if !equalStr(typ.alterations, wantType) {
		t.Errorf("type alterations = %v, want %v", typ.alterations, wantType)
	}
	wantUserID := []string{
		"20260201_alter: DROP NOT NULL",
		"20260201_alter: SET NOT NULL",
	}
	if !equalStr(userID.alterations, wantUserID) {
		t.Errorf("user_id alterations = %v, want %v", userID.alterations, wantUserID)
	}

	// Render must include alteration entries as bullet sub-items.
	rendered := string(renderTableMarkdown(st))
	for _, want := range []string{
		"altered: `20260201_alter: TYPE change`",
		"altered: `20260201_alter: SET NOT NULL`",
	} {
		if !strings.Contains(rendered, want) {
			t.Errorf("rendered missing %q in:\n%s", want, rendered)
		}
	}
}

func TestSplitColumnList_HandlesNestedParensAndQuotes(t *testing.T) {
	input := `"id" bigint not null, "name" character varying not null default 'a, b', "qty" numeric(10,2) check (qty > 0), constraint pk primary key ("id")`
	got := splitColumnList(input)
	want := []string{
		`"id" bigint not null`,
		`"name" character varying not null default 'a, b'`,
		`"qty" numeric(10,2) check (qty > 0)`,
		`constraint pk primary key ("id")`,
	}
	if len(got) != len(want) {
		t.Fatalf("split count = %d, want %d: %#v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("entry %d: got %q, want %q", i, got[i], want[i])
		}
	}
}

func TestParseColumnEntry(t *testing.T) {
	cases := []struct {
		entry, name, typeText string
		ok                    bool
	}{
		{`"id" bigint not null`, "id", "bigint not null", true},
		{`"created_at" timestamp with time zone not null default now()`,
			"created_at", "timestamp with time zone not null default now()", true},
		{`name text`, "name", "text", true},
		{`primary key ("id")`, "", "", false},
		{`constraint pk primary key (id)`, "", "", false},
		{`unique (slug)`, "", "", false},
		{`check (qty > 0)`, "", "", false},
		{`foreign key (org_id) references orgs(id)`, "", "", false},
	}
	for _, c := range cases {
		name, typeText, ok := parseColumnEntry(c.entry)
		if name != c.name || typeText != c.typeText || ok != c.ok {
			t.Errorf("parseColumnEntry(%q) = (%q,%q,%v), want (%q,%q,%v)",
				c.entry, name, typeText, ok, c.name, c.typeText, c.ok)
		}
	}
}

func TestApplyMigrationToTables_Replay(t *testing.T) {
	states := map[string]*tableState{}

	// Migration 1: create users with two columns.
	applyMigrationToTables(states, stripSQLComments(`
create table "public"."users" (
    "id" bigint not null,
    "email" text not null
);
`), "20260101_init")

	st := states["public.users"]
	if st == nil || len(st.columns) != 2 {
		t.Fatalf("after init: cols = %+v", st)
	}
	if st.columns[0].name != "id" || st.columns[1].name != "email" {
		t.Errorf("init cols: %+v", st.columns)
	}

	// Migration 2: add display_name and remove email.
	applyMigrationToTables(states, stripSQLComments(`
alter table "public"."users" add column "display_name" text;
alter table "public"."users" drop column "email";
`), "20260201_alter")
	if got := colNames(states["public.users"].columns); !equalStr(got, []string{"id", "display_name"}) {
		t.Errorf("after add+drop: cols = %v", got)
	}

	// Migration 3: rename id to user_id, then add a multi-clause ALTER.
	applyMigrationToTables(states, stripSQLComments(`
alter table "public"."users" rename column "id" to "user_id";
alter table public.users
  add column if not exists "active" boolean not null default true,
  add column if not exists "metadata" jsonb;
`), "20260301_alter")
	if got := colNames(states["public.users"].columns); !equalStr(got, []string{"user_id", "display_name", "active", "metadata"}) {
		t.Errorf("after rename+multi-add: cols = %v", got)
	}

	// Provenance check: display_name was added in 20260201_alter.
	for _, c := range states["public.users"].columns {
		if c.name == "display_name" && c.addedIn != "20260201_alter" {
			t.Errorf("display_name addedIn = %q, want 20260201_alter", c.addedIn)
		}
		if c.name == "active" && c.addedIn != "20260301_alter" {
			t.Errorf("active addedIn = %q, want 20260301_alter", c.addedIn)
		}
	}
}

func colNames(cols []columnDef) []string {
	out := make([]string, len(cols))
	for i, c := range cols {
		out[i] = c.name
	}
	return out
}
func equalStr(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestSanitizeTableFilename(t *testing.T) {
	cases := map[string]string{
		"public.users":       "public.users",
		"public.foo-bar":     "public.foo-bar",
		"My Schema.tbl":      "my_schema.tbl",
		"":                   "unnamed",
		"public.with spaces": "public.with_spaces",
	}
	for in, want := range cases {
		// sanitizeTableFilename lowercases via the qualify step, but we
		// pass already-lowercased input here. Test names assume lowercase.
		if got := sanitizeTableFilename(strings.ToLower(in)); got != strings.ToLower(want) {
			t.Errorf("sanitizeTableFilename(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestExtractSQLSchemaInto_EndToEnd(t *testing.T) {
	dir := t.TempDir()
	out := t.TempDir()
	src := filepath.Join(dir, "20260101120000_init.sql")
	body := []byte(`create table public.foo (id int);
create policy "foo_read" on public.foo for select using (true);
`)
	if err := os.WriteFile(src, body, 0o644); err != nil {
		t.Fatal(err)
	}
	o := pullOpts{out: out}
	if err := extractSQLSchemaInto(dir, "test", o, nil, []string{"extract", "sql-schema"}, "_schema"); err != nil {
		t.Fatal(err)
	}
	outFile := filepath.Join(out, "test", "_schema", "migrations", "20260101120000_init.md")
	data, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatalf("expected output at %s: %v", outFile, err)
	}
	got := string(data)
	if !strings.Contains(got, "public.foo") {
		t.Errorf("output missing table reference: %s", got)
	}
	if !strings.Contains(got, "foo_read") {
		t.Errorf("output missing policy reference: %s", got)
	}

	// Manifest should now have BOTH the per-migration doc AND a per-table
	// pivot doc (public.foo) — added by the per-table aggregation pass.
	mf, err := loadOrMigrateManifest(filepath.Join(out, "test"))
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	if len(mf.Entries) != 2 {
		t.Fatalf("manifest entries = %d, want 2 (migration doc + table pivot): %+v", len(mf.Entries), mf.Entries)
	}
	var sawMigrationDoc, sawTableDoc bool
	for _, r := range mf.Entries {
		if r.Mode != "extract:sql-schema" {
			t.Errorf("manifest mode = %q, want extract:sql-schema", r.Mode)
		}
		switch {
		case strings.HasSuffix(r.Path, "_schema/migrations/20260101120000_init.md"):
			sawMigrationDoc = true
		case strings.HasSuffix(r.Path, "_schema/tables/public.foo.md"):
			sawTableDoc = true
		}
	}
	if !sawMigrationDoc {
		t.Errorf("missing per-migration doc in manifest: %+v", mf.Entries)
	}
	if !sawTableDoc {
		t.Errorf("missing per-table pivot doc in manifest: %+v", mf.Entries)
	}

	// And the per-table doc should mention the migration that introduced
	// the table — that's the load-bearing aggregation behavior.
	tableDoc, err := os.ReadFile(filepath.Join(out, "test", "_schema", "tables", "public.foo.md"))
	if err != nil {
		t.Fatalf("expected per-table doc: %v", err)
	}
	if !strings.Contains(string(tableDoc), "20260101120000_init") {
		t.Errorf("per-table doc missing migration provenance: %s", tableDoc)
	}
}
