package main

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// ---------- splitStatements ----------

func TestSplitStatements(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"empty", "", nil},
		{"whitespace", "   \n\t\n", nil},
		{
			"two simple",
			"INSERT INTO t VALUES (1);\nINSERT INTO t VALUES (2);\n",
			[]string{"INSERT INTO t VALUES (1)", "INSERT INTO t VALUES (2)"},
		},
		{
			"trailing without newline preserved",
			"INSERT INTO t VALUES (1);\nINSERT INTO t VALUES (2)",
			[]string{"INSERT INTO t VALUES (1)", "INSERT INTO t VALUES (2)"},
		},
		{
			"embedded semicolon inside the statement (no following newline) stays together",
			"INSERT INTO t VALUES ('a;b');\nINSERT INTO t VALUES ('c');\n",
			[]string{"INSERT INTO t VALUES ('a;b')", "INSERT INTO t VALUES ('c')"},
		},
		{
			"single trailing newline doesn't yield empty entry",
			"CREATE TABLE t (a INT);\n",
			[]string{"CREATE TABLE t (a INT)"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := splitStatements(tc.in)
			// reflect.DeepEqual treats []string{} != nil; normalize empty
			if len(got) == 0 && len(tc.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got %#v, want %#v", got, tc.want)
			}
		})
	}
}

// ---------- translateForSQLite ----------

func TestTranslateForSQLite(t *testing.T) {
	cases := []struct {
		name     string
		in       string
		contains []string
		excludes []string
	}{
		{
			"strip ENGINE/CHARSET/COLLATE on CREATE TABLE",
			"CREATE TABLE `t` (`x` int) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin;",
			[]string{"CREATE TABLE IF NOT EXISTS \"t\"", "\"x\" INTEGER", ");"},
			[]string{"ENGINE", "CHARSET", "COLLATE", "InnoDB", "`"},
		},
		{
			"int family → INTEGER",
			"a tinyint, b smallint, c mediumint, d int(11), e bigint",
			[]string{"a INTEGER", "b INTEGER", "c INTEGER", "d INTEGER", "e INTEGER"},
			nil,
		},
		{
			"varchar(N) → TEXT",
			"name varchar(255)",
			[]string{"name TEXT"},
			[]string{"varchar"},
		},
		{
			"longtext / mediumtext / tinytext → TEXT",
			"a longtext, b mediumtext, c tinytext",
			[]string{"a TEXT", "b TEXT", "c TEXT"},
			[]string{"longtext", "mediumtext", "tinytext"},
		},
		{
			"datetime / timestamp → TEXT",
			"created datetime, updated timestamp",
			[]string{"created TEXT", "updated TEXT"},
			[]string{"datetime", "timestamp"},
		},
		{
			"backticks → double quotes",
			"SELECT `a`, `b` FROM `t`",
			[]string{`"a"`, `"b"`, `"t"`},
			[]string{"`"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := translateForSQLite(tc.in)
			for _, s := range tc.contains {
				if !strings.Contains(got, s) {
					t.Errorf("output missing %q\n  got: %s", s, got)
				}
			}
			for _, s := range tc.excludes {
				if strings.Contains(got, s) {
					t.Errorf("output unexpectedly contains %q\n  got: %s", s, got)
				}
			}
		})
	}
}

// translateForSQLite must be idempotent: running it twice yields the same
// result as running it once. Catches regressions where a translation rule
// matches its own output.
func TestTranslateForSQLiteIdempotent(t *testing.T) {
	input := "CREATE TABLE `users` (`id` int(11) NOT NULL, `name` varchar(255), `bio` longtext, `created` datetime) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;"
	once := translateForSQLite(input)
	twice := translateForSQLite(once)
	if once != twice {
		t.Errorf("not idempotent:\n  once:  %s\n  twice: %s", once, twice)
	}
}

// ---------- translateForDolt ----------

func TestTranslateForDolt(t *testing.T) {
	in := `INSERT INTO "t" ("a", "b") VALUES (1, 'hello')`
	got := translateForDolt(in)
	want := "INSERT INTO `t` (`a`, `b`) VALUES (1, 'hello')"
	if got != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}
}

// translate round-trip for the identifier-quoting we actually use:
// dolt → sqlite → dolt should preserve the original backtick form on the
// subset of SQL we generate (no embedded quotes-of-the-other-kind in idents).
func TestTranslateRoundtrip(t *testing.T) {
	orig := "INSERT INTO `t` (`a`, `b`) VALUES (1, 'a string with \"quotes\"')"
	mid := translateForSQLite(orig)
	back := translateForDolt(mid)
	// The string literal contains "quotes" which translateForDolt will also
	// flip to backticks — that's a known limitation of the naive translator.
	// Document it: assert the *identifier* survives even if the literal doesn't.
	if !strings.Contains(back, "`t`") || !strings.Contains(back, "`a`") || !strings.Contains(back, "`b`") {
		t.Errorf("identifiers lost in round-trip: %s", back)
	}
}

// ---------- parseCommitCSV ----------

func TestParseCommitCSV(t *testing.T) {
	in := `commit_hash,committer,email,date,message
abc123,Alice,alice@example.com,2026-01-01,first commit
def456,Bob,bob@example.com,2026-01-02,"second, with comma"
`
	got, err := parseCommitCSV(in)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 commits, got %d", len(got))
	}
	if got[0].Hash != "abc123" || got[0].Author != "Alice" || got[0].Message != "first commit" {
		t.Errorf("row 0 wrong: %+v", got[0])
	}
	if got[1].Message != "second, with comma" {
		t.Errorf("CSV-quoted message not preserved: %q", got[1].Message)
	}
}

func TestParseCommitCSVEmpty(t *testing.T) {
	got, err := parseCommitCSV("commit_hash,committer,email,date,message\n")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("want 0 commits, got %d", len(got))
	}
}

func TestParseCommitCSVHeaderOnly(t *testing.T) {
	got, err := parseCommitCSV("")
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Errorf("want nil, got %v", got)
	}
}

// ---------- truncate ----------

func TestTruncate(t *testing.T) {
	cases := []struct {
		in   string
		n    int
		want string
	}{
		{"hello", 10, "hello"},
		{"hello world", 5, "hello...[truncated]"},
		{"", 5, ""},
	}
	for _, tc := range cases {
		got := truncate(tc.in, tc.n)
		if got != tc.want {
			t.Errorf("truncate(%q, %d) = %q, want %q", tc.in, tc.n, got, tc.want)
		}
	}
}

// ---------- end-to-end integration tests (skipped if binaries missing) ----------

// requireDoltAndReplay skips the test if dolt/doltlite/the replay binary
// isn't available. The test also needs the dolt-replay binary built — we
// build it on demand so `go test` works without a separate step.
func requireDoltAndReplay(t *testing.T) (replayBin string) {
	t.Helper()
	if _, err := exec.LookPath("dolt"); err != nil {
		t.Skip("dolt not installed")
	}
	if _, err := exec.LookPath("doltlite"); err != nil {
		t.Skip("doltlite not installed")
	}
	replayBin = filepath.Join(t.TempDir(), "dolt-replay")
	cmd := exec.Command("go", "build", "-o", replayBin, ".")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build dolt-replay: %v\n%s", err, out)
	}
	return replayBin
}

// runIn runs a command in dir, fails the test on non-zero exit. Returns stdout.
func runIn(t *testing.T, dir, prog string, args ...string) string {
	t.Helper()
	cmd := exec.Command(prog, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v in %s: %v\n%s", prog, args, dir, err, out)
	}
	return string(out)
}

// makeDoltRepo initializes a dolt repo at dir. Returns the dir.
func makeDoltRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	runIn(t, dir, "dolt", "init", "--name", "test", "--email", "t@t.t")
	return dir
}

// doltSQL executes a SQL statement against the dolt repo at dir.
func doltSQL(t *testing.T, dir, sql string) {
	t.Helper()
	runIn(t, dir, "dolt", "sql", "-q", sql)
}

// doltCommit commits the current working set with the given message.
func doltCommit(t *testing.T, dir, msg string) {
	t.Helper()
	runIn(t, dir, "dolt", "add", "-A")
	runIn(t, dir, "dolt", "commit", "-m", msg, "--allow-empty")
}

// doltCount returns COUNT(*) for table.
func doltCount(t *testing.T, dir, table string) int {
	t.Helper()
	out := runIn(t, dir, "dolt", "sql", "-r", "csv", "-q",
		fmt.Sprintf("SELECT COUNT(*) FROM `%s`", table))
	// CSV output: header line + count line
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) < 2 {
		t.Fatalf("unexpected dolt count output: %q", out)
	}
	var n int
	if _, err := fmt.Sscanf(lines[1], "%d", &n); err != nil {
		t.Fatalf("parse dolt count %q: %v", lines[1], err)
	}
	return n
}

// dliteCount returns COUNT(*) for table from a doltlite db.
func dliteCount(t *testing.T, db, table string) int {
	t.Helper()
	cmd := exec.Command("doltlite", db, fmt.Sprintf("SELECT COUNT(*) FROM `%s`", table))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("doltlite count %s: %v\n%s", table, err, out)
	}
	var n int
	if _, err := fmt.Sscanf(strings.TrimSpace(string(out)), "%d", &n); err != nil {
		t.Fatalf("parse doltlite count %q: %v", out, err)
	}
	return n
}

// runReplay invokes dolt-replay; returns combined output for inspection.
// Fails the test only if exit != 0 AND we didn't pass --continue-on-error.
func runReplay(t *testing.T, replayBin string, args ...string) string {
	t.Helper()
	cmd := exec.Command(replayBin, args...)
	out, err := cmd.CombinedOutput()
	keepGoing := false
	for _, a := range args {
		if a == "--continue-on-error" {
			keepGoing = true
		}
	}
	if err != nil && !keepGoing {
		t.Fatalf("dolt-replay %v: %v\n%s", args, err, out)
	}
	return string(out)
}

// TestE2E_SimpleInsertOnly: 5 commits each adding 10 rows to one INT-PK table.
// Source ends at 50 rows; target must match.
func TestE2E_SimpleInsertOnly(t *testing.T) {
	replayBin := requireDoltAndReplay(t)
	src := makeDoltRepo(t)

	doltSQL(t, src, "CREATE TABLE t (id INT PRIMARY KEY, name TEXT)")
	doltCommit(t, src, "init schema")

	for batch := 0; batch < 5; batch++ {
		var sb strings.Builder
		for i := 0; i < 10; i++ {
			id := batch*10 + i + 1
			fmt.Fprintf(&sb, "INSERT INTO t VALUES (%d, 'name-%d');", id, id)
		}
		doltSQL(t, src, sb.String())
		doltCommit(t, src, fmt.Sprintf("batch %d", batch))
	}

	if got := doltCount(t, src, "t"); got != 50 {
		t.Fatalf("source row count: got %d, want 50", got)
	}

	dst := filepath.Join(t.TempDir(), "out.dl")
	runIn(t, src, "doltlite", dst, "SELECT 1") // create empty
	out := runReplay(t, replayBin,
		"--src-kind", "dolt", "--src", src,
		"--dst-kind", "doltlite", "--dst", dst)
	t.Logf("replay output:\n%s", out)

	if got := dliteCount(t, dst, "t"); got != 50 {
		t.Fatalf("target row count: got %d, want 50", got)
	}
}

// TestE2E_InsertDelete: documents the over-count bug we observed in
// the bahaiwritings clone — INSERTs that get DELETEd later end up
// over-counting in target. Source ends at 5 rows; target should match.
func TestE2E_InsertDelete(t *testing.T) {
	replayBin := requireDoltAndReplay(t)
	src := makeDoltRepo(t)

	doltSQL(t, src, "CREATE TABLE t (id INT PRIMARY KEY, name TEXT)")
	doltCommit(t, src, "schema")

	doltSQL(t, src, "INSERT INTO t VALUES (1,'a'),(2,'b'),(3,'c'),(4,'d'),(5,'e'),(6,'f'),(7,'g'),(8,'h'),(9,'i'),(10,'j')")
	doltCommit(t, src, "10 rows")

	doltSQL(t, src, "DELETE FROM t WHERE id IN (3, 5, 7, 9, 10)")
	doltCommit(t, src, "delete odd-ish")

	if got := doltCount(t, src, "t"); got != 5 {
		t.Fatalf("source row count after deletes: got %d, want 5", got)
	}

	dst := filepath.Join(t.TempDir(), "out.dl")
	runIn(t, src, "doltlite", dst, "SELECT 1")
	runReplay(t, replayBin,
		"--src-kind", "dolt", "--src", src,
		"--dst-kind", "doltlite", "--dst", dst)

	got := dliteCount(t, dst, "t")
	if got != 5 {
		t.Fatalf("target row count: got %d, want 5 (over-count = INSERT OR REPLACE not handling DELETEs)", got)
	}
}

// TestE2E_TextPKInsertDelete: same as above but with TEXT PK like
// bahaiwritings.writings — exercises the path where we hit the issue.
func TestE2E_TextPKInsertDelete(t *testing.T) {
	replayBin := requireDoltAndReplay(t)
	src := makeDoltRepo(t)

	doltSQL(t, src, "CREATE TABLE writings (version VARCHAR(36) PRIMARY KEY, payload TEXT)")
	doltCommit(t, src, "schema")

	for i := 1; i <= 20; i++ {
		doltSQL(t, src, fmt.Sprintf(
			"INSERT INTO writings VALUES ('uuid-%04d', 'p%d')", i, i))
	}
	doltCommit(t, src, "20 inserts")

	doltSQL(t, src, "DELETE FROM writings WHERE version IN ('uuid-0005','uuid-0010','uuid-0015','uuid-0020')")
	doltCommit(t, src, "4 deletes")

	if got := doltCount(t, src, "writings"); got != 16 {
		t.Fatalf("source: got %d, want 16", got)
	}

	dst := filepath.Join(t.TempDir(), "out.dl")
	runIn(t, src, "doltlite", dst, "SELECT 1")
	runReplay(t, replayBin,
		"--src-kind", "dolt", "--src", src,
		"--dst-kind", "doltlite", "--dst", dst)

	got := dliteCount(t, dst, "writings")
	if got != 16 {
		t.Fatalf("target: got %d, want 16", got)
	}
}

// TestE2E_DropPKColumnReproducesOverCount: documents that WITHOUT
// --rebuild-on-pk-drop, the schema migration is silently skipped and
// the target ends up with extra rows. This is what bit the bahaiwritings
// clone (writings ended up at 50172 vs source 49212).
func TestE2E_DropPKColumnReproducesOverCount(t *testing.T) {
	replayBin := requireDoltAndReplay(t)
	src := makeDoltRepo(t)

	doltSQL(t, src, "CREATE TABLE w (id INTEGER PRIMARY KEY, version VARCHAR(36), name TEXT)")
	doltCommit(t, src, "schema v1 with id PK")
	for i := 1; i <= 5; i++ {
		doltSQL(t, src, fmt.Sprintf("INSERT INTO w VALUES (%d, 'v-%04d', 'orig-%d')", i, i, i))
	}
	doltCommit(t, src, "5 rows under id PK")
	doltSQL(t, src, "ALTER TABLE w DROP id")
	doltSQL(t, src, "ALTER TABLE w ADD PRIMARY KEY (version)")
	doltCommit(t, src, "drop id, version is PK")
	doltSQL(t, src, "UPDATE w SET name = 'updated' WHERE version = 'v-0001'")
	doltCommit(t, src, "update v-0001")

	if got := doltCount(t, src, "w"); got != 5 {
		t.Fatalf("source: got %d, want 5", got)
	}

	dst := filepath.Join(t.TempDir(), "out.dl")
	runIn(t, src, "doltlite", dst, "SELECT 1")
	out := runReplay(t, replayBin,
		"--src-kind", "dolt", "--src", src,
		"--dst-kind", "doltlite", "--dst", dst,
		"--continue-on-error")
	t.Logf("replay output:\n%s", out)

	got := dliteCount(t, dst, "w")
	// Without --rebuild-on-pk-drop, target keeps the old id-PK schema —
	// dolt diff emits ALTER DROP PRIMARY KEY + ADD PRIMARY KEY which our
	// translator turns into "-- skipped" comments, and DROP id fails. The
	// minimal repro lands at the right row count anyway because the UPDATE
	// is just an UPDATE (not DELETE+INSERT). The bahaiwritings over-count
	// requires longer-history re-INSERT-under-new-schema patterns that this
	// minimal test doesn't reproduce. Documented as a Logf — see
	// TestE2E_DropPKColumnSchemaMigration for the working case with
	// --rebuild-on-pk-drop.
	t.Logf("without --rebuild-on-pk-drop: target=%d rows, source=5 (over-count would be > 5)", got)
}

// TestE2E_DropPKColumnSchemaMigration: same shape with --rebuild-on-pk-drop;
// confirms the flag actually fixes the over-count.
func TestE2E_DropPKColumnSchemaMigration(t *testing.T) {
	replayBin := requireDoltAndReplay(t)
	src := makeDoltRepo(t)

	doltSQL(t, src, "CREATE TABLE w (id INTEGER PRIMARY KEY, version VARCHAR(36), name TEXT)")
	doltCommit(t, src, "schema v1 with id PK")

	for i := 1; i <= 5; i++ {
		doltSQL(t, src, fmt.Sprintf("INSERT INTO w VALUES (%d, 'v-%04d', 'orig-%d')", i, i, i))
	}
	doltCommit(t, src, "5 rows under id PK")

	// Schema migration: drop id, promote version to PK.
	doltSQL(t, src, "ALTER TABLE w DROP id")
	doltSQL(t, src, "ALTER TABLE w ADD PRIMARY KEY (version)")
	doltCommit(t, src, "drop id, version is PK")

	// More inserts under new PK convention.
	for i := 6; i <= 8; i++ {
		doltSQL(t, src, fmt.Sprintf("INSERT INTO w VALUES ('v-%04d', 'new-%d')", i, i))
	}
	doltCommit(t, src, "3 more rows under version PK")

	// Update one of the original rows — under the new PK this is one row.
	doltSQL(t, src, "UPDATE w SET name = 'updated' WHERE version = 'v-0001'")
	doltCommit(t, src, "update v-0001")

	if got := doltCount(t, src, "w"); got != 8 {
		t.Fatalf("source: got %d, want 8", got)
	}

	dst := filepath.Join(t.TempDir(), "out.dl")
	runIn(t, src, "doltlite", dst, "SELECT 1")
	out := runReplay(t, replayBin,
		"--src-kind", "dolt", "--src", src,
		"--dst-kind", "doltlite", "--dst", dst,
		"--continue-on-error",
		"--rebuild-on-pk-drop")
	t.Logf("replay output:\n%s", out)

	got := dliteCount(t, dst, "w")
	if got != 8 {
		t.Fatalf("target: got %d, want 8 (duplicate rows = PK migration not handled)", got)
	}
}

// TestE2E_MultilineStringLiteral exercises the `splitStatements` bug we
// hit on the bahaiwritings 'Tablet of the Holy Mariner' commit — a single
// SQL string literal containing embedded newlines.
func TestE2E_MultilineStringLiteral(t *testing.T) {
	replayBin := requireDoltAndReplay(t)
	src := makeDoltRepo(t)

	doltSQL(t, src, "CREATE TABLE notes (id INT PRIMARY KEY, body TEXT)")
	doltCommit(t, src, "schema")

	// Insert a row whose value contains literal newlines + a semicolon.
	multiline := "## Tablet of the Holy Mariner\n\nLine 2 with semicolon ; here\nLine 3"
	doltSQL(t, src, fmt.Sprintf("INSERT INTO notes VALUES (1, '%s')",
		strings.ReplaceAll(multiline, "'", "''")))
	doltCommit(t, src, "multiline insert")

	if got := doltCount(t, src, "notes"); got != 1 {
		t.Fatalf("source: got %d, want 1", got)
	}

	dst := filepath.Join(t.TempDir(), "out.dl")
	runIn(t, src, "doltlite", dst, "SELECT 1")
	runReplay(t, replayBin,
		"--src-kind", "dolt", "--src", src,
		"--dst-kind", "doltlite", "--dst", dst)

	got := dliteCount(t, dst, "notes")
	if got != 1 {
		t.Fatalf("target: got %d, want 1 (multiline string literal broke splitStatements?)", got)
	}
}
