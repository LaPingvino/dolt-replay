package main

import (
	"os/exec"
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

// ---------- end-to-end smoke (skipped if binaries missing) ----------

// Round-trips a tiny dolt repo through doltlite and back, asserting the
// commit count, author, and row count survive. Skips automatically when
// either tool isn't installed so the unit suite stays hermetic.
func TestSmokeRoundtrip(t *testing.T) {
	if _, err := exec.LookPath("dolt"); err != nil {
		t.Skip("dolt not installed")
	}
	if _, err := exec.LookPath("doltlite"); err != nil {
		t.Skip("doltlite not installed")
	}
	t.Skip("end-to-end harness not wired yet; placeholder for future round-trip test")
}
