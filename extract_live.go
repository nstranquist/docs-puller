package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Live-DB introspection extractors. Both shell out to the vendor CLI
// (pg_dump, clickhouse-client), capture the schema DDL on stdout, write
// it to a single synthetic .sql file, then hand the file off to the
// existing SQL-schema pipeline. That gives us per-migration + per-table
// docs with no new rendering code paths.
//
// Why a synthetic .sql file rather than parsing the DDL inline: the
// existing scanner already handles CREATE/ALTER TABLE, ENABLE RLS,
// CREATE POLICY, CREATE INDEX deterministically across regex tests,
// and the pipeline already wires manifest + FTS5 update under the
// write lock. Reusing it means new failure modes don't appear here.
//
// SAFETY NOTE: these tools NEVER auto-discover DSNs or read environment
// variables for credentials. Pass --conn / --dsn explicitly. They also
// don't open network connections themselves; the vendor CLI does, and
// fails fast (5s connect timeout) if credentials are wrong or the host
// is unreachable.

func cmdExtract(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "extract: missing kind (try: sql-schema, pg-introspect, clickhouse-schema)")
		os.Exit(2)
	}
	kind := args[0]
	rest := args[1:]
	switch kind {
	case "sql-schema":
		cmdExtractSQLSchema(rest)
	case "pg-introspect":
		cmdExtractPGIntrospect(rest)
	case "clickhouse-schema":
		cmdExtractClickHouseSchema(rest)
	default:
		fmt.Fprintf(os.Stderr, "extract: unknown kind %q (supported: sql-schema, pg-introspect, clickhouse-schema)\n", kind)
		os.Exit(2)
	}
}

func cmdExtractPGIntrospect(args []string) {
	o := defaultOpts()
	fs := flag.NewFlagSet("extract pg-introspect", flag.ExitOnError)
	conn := fs.String("conn", "", "Postgres connection string (REQUIRED — no env-var auto-discovery)")
	name := fs.String("name", "", "source name to write into (REQUIRED)")
	schema := fs.String("schema", "public", "schema to dump (default 'public'; pass empty to dump all schemas)")
	bindOpts(fs, &o)
	fs.Parse(args)

	if *conn == "" {
		die(fmt.Errorf("extract pg-introspect: --conn URL is required"))
	}
	if *name == "" {
		die(fmt.Errorf("extract pg-introspect: --name SOURCE is required"))
	}
	if _, err := exec.LookPath("pg_dump"); err != nil {
		die(fmt.Errorf("extract pg-introspect: pg_dump not in PATH — install with `brew install postgresql` or equivalent"))
	}

	source := sanitizeSourceName(*name)

	// pg_dump flags chosen to produce something the SQL scanner handles
	// well: schema-only (no data, fast), no-owner/no-privileges (less
	// clutter), connect-timeout=5s (fail-fast), --no-comments (also less
	// clutter).
	dumpArgs := []string{
		"--schema-only",
		"--no-owner",
		"--no-privileges",
		"--no-comments",
		"--no-publications",
		"--no-subscriptions",
	}
	if *schema != "" {
		dumpArgs = append(dumpArgs, "--schema", *schema)
	}
	dumpArgs = append(dumpArgs, *conn)

	fmt.Fprintf(os.Stderr, "→ pg_dump --schema-only %s ...\n", redactConn(*conn))
	cmd := exec.Command("pg_dump", dumpArgs...)
	// Connect timeout via libpq env (pg_dump respects it).
	cmd.Env = append(os.Environ(), "PGCONNECT_TIMEOUT=5")
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		// Surface stderr but redact the connection string if it leaks
		// into error messages (pg_dump usually doesn't print it, but be
		// defensive — the caller may be piping our stderr to chat).
		stderr := redactConn(errb.String())
		die(fmt.Errorf("pg_dump failed: %v\n%s", err, stderr))
	}

	if err := stageAndExtract(out.Bytes(), source, "pg_introspect_"+nowStamp()+".sql", o, []string{"extract", "pg-introspect"}); err != nil {
		die(err)
	}
}

func cmdExtractClickHouseSchema(args []string) {
	o := defaultOpts()
	fs := flag.NewFlagSet("extract clickhouse-schema", flag.ExitOnError)
	dsn := fs.String("dsn", "", "ClickHouse DSN, e.g. 'tcp://user:pass@host:9000/db' (REQUIRED)")
	name := fs.String("name", "", "source name to write into (REQUIRED)")
	database := fs.String("database", "", "database to dump (default: from DSN; pass empty to use connection default)")
	bindOpts(fs, &o)
	fs.Parse(args)

	if *dsn == "" {
		die(fmt.Errorf("extract clickhouse-schema: --dsn URL is required"))
	}
	if *name == "" {
		die(fmt.Errorf("extract clickhouse-schema: --name SOURCE is required"))
	}
	if _, err := exec.LookPath("clickhouse-client"); err != nil {
		die(fmt.Errorf("extract clickhouse-schema: clickhouse-client not in PATH — install with `brew install --cask clickhouse` or equivalent"))
	}

	source := sanitizeSourceName(*name)

	// We pull SHOW CREATE TABLE for every table in the database. This is
	// the canonical way to get clickhouse DDL — `clickhouse-client --dsn
	// <dsn> --query "SHOW TABLES FROM db"`, then per-table SHOW CREATE.
	showTablesQ := "SHOW TABLES"
	if *database != "" {
		showTablesQ += " FROM " + *database
	}
	fmt.Fprintf(os.Stderr, "→ clickhouse-client SHOW TABLES %s ...\n", redactConn(*dsn))
	tablesOut, err := runCmd(exec.Command("clickhouse-client", "--connect_timeout=5", "--dsn", *dsn, "--query", showTablesQ))
	if err != nil {
		die(fmt.Errorf("clickhouse SHOW TABLES failed: %v", err))
	}
	tables := strings.Fields(strings.TrimSpace(string(tablesOut)))

	var combined bytes.Buffer
	for _, t := range tables {
		q := "SHOW CREATE TABLE "
		if *database != "" {
			q += *database + "."
		}
		q += t
		ddl, err := runCmd(exec.Command("clickhouse-client", "--connect_timeout=5", "--dsn", *dsn, "--query", q))
		if err != nil {
			fmt.Fprintf(os.Stderr, "  WARN: SHOW CREATE TABLE %s failed: %v\n", t, err)
			continue
		}
		// SHOW CREATE TABLE returns the DDL with embedded \n — clickhouse
		// renders \n literally in --format=TabSeparated; replace them
		// back to actual newlines so the scanner sees normal SQL.
		s := strings.ReplaceAll(string(ddl), `\n`, "\n")
		combined.WriteString(s)
		combined.WriteString(";\n\n")
	}

	if combined.Len() == 0 {
		die(fmt.Errorf("clickhouse: no tables introspected (database=%q empty?)", *database))
	}

	if err := stageAndExtract(combined.Bytes(), source, "clickhouse_introspect_"+nowStamp()+".sql", o, []string{"extract", "clickhouse-schema"}); err != nil {
		die(err)
	}
}

// stageAndExtract writes raw DDL into a temp dir and routes it through
// extractSQLSchemaInto with `_live` as the per-source subtree. Output
// lands at <source>/_live/migrations/ and <source>/_live/tables/ so a
// single source can carry both migration history (in _schema/) and a
// live snapshot (in _live/) without one clobbering the other.
//
// Earlier versions wrote to _schema/ then renamed the directory; that
// silently moved any pre-existing migration docs into _live/ on first
// run. Direct write is the right primitive.
func stageAndExtract(ddl []byte, source, filename string, o pullOpts, cmdArgs []string) error {
	tmp, err := os.MkdirTemp("", "extract-live-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	if err := os.WriteFile(filepath.Join(tmp, filename), ddl, 0o644); err != nil {
		return err
	}
	return extractSQLSchemaInto(tmp, source, o, nil, cmdArgs, "_live")
}

// runCmd captures stdout from a command and returns it. stderr is sent to
// our stderr so the user sees the real error from pg_dump / clickhouse-client.
func runCmd(c *exec.Cmd) ([]byte, error) {
	var out bytes.Buffer
	c.Stdout = &out
	c.Stderr = os.Stderr
	if err := c.Run(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// redactConn replaces a Postgres-style or ClickHouse-style connection
// string with its host/db scaffold so we can log progress without leaking
// passwords. Conservative: anything between "://user:" and "@" gets
// replaced with "***". If the string isn't a recognizable URL form, we
// return "***" rather than echoing it.
func redactConn(s string) string {
	at := strings.LastIndex(s, "@")
	colonSep := strings.Index(s, "://")
	if at < 0 || colonSep < 0 || at <= colonSep {
		return "***"
	}
	userPass := s[colonSep+3 : at]
	if i := strings.Index(userPass, ":"); i >= 0 {
		s = s[:colonSep+3] + userPass[:i] + ":***" + s[at:]
	}
	return s
}

// nowStamp returns a UTC timestamp suitable for inclusion in a filename.
func nowStamp() string {
	return time.Now().UTC().Format("20060102T150405Z")
}
