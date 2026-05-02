// dolt-replay — replay commit history between Dolt and Doltlite databases.
//
// Supports four source/target combos: dolt↔dolt, dolt→doltlite, doltlite→dolt,
// doltlite↔doltlite. The last is useful for migrating across incompatible
// doltlite version upgrades — replay history through a fresh DB built by the
// current binary.
//
// Per-commit, the tool extracts diff SQL via `dolt diff -r sql` (Dolt) or
// reconstructs from `dolt_diff_<table>` (Doltlite), translates dialect
// quirks for the target, applies the SQL, then creates a new commit using
// the original message + author + date.
//
// Usage:
//   go run . --src-kind dolt    --src ~/bahaiwritings \
//            --dst-kind doltlite --dst /tmp/out.dl \
//            --table writings --limit 5
//
// POC limitations: one table at a time; schema translation is best-effort
// regex (drops ENGINE/CHARSET/COLLATE, rewrites smallint/int/bigint→INTEGER,
// varchar/text→TEXT, datetime/timestamp→TEXT).
package main

import (
	"bytes"
	"encoding/csv"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
)

// ---------- shell helpers ----------

func run(dir string, stdin string, prog string, args ...string) (string, string, error) {
	cmd := exec.Command(prog, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	err := cmd.Run()
	return out.String(), errb.String(), err
}

// ---------- types ----------

type Commit struct {
	Hash, Author, Email, Date, Message string
}

// ---------- source readers ----------

func parseCommitCSV(s string) ([]Commit, error) {
	r := csv.NewReader(strings.NewReader(s))
	rows, err := r.ReadAll()
	if err != nil {
		return nil, err
	}
	if len(rows) < 2 {
		return nil, nil
	}
	hdr := rows[0]
	col := func(name string) int {
		for i, h := range hdr {
			if h == name {
				return i
			}
		}
		return -1
	}
	hH, aH, eH, dH, mH := col("commit_hash"), col("committer"), col("email"), col("date"), col("message")
	out := make([]Commit, 0, len(rows)-1)
	for _, r := range rows[1:] {
		out = append(out, Commit{r[hH], r[aH], r[eH], r[dH], r[mH]})
	}
	return out, nil
}

func doltLog(repo string, limit int) ([]Commit, error) {
	sql := fmt.Sprintf("SELECT commit_hash, committer, email, date, message FROM dolt_log ORDER BY date DESC LIMIT %d", limit+1)
	out, errs, err := run(repo, "", "dolt", "sql", "-r", "csv", "-q", sql)
	if err != nil {
		return nil, fmt.Errorf("dolt log: %v\n%s", err, errs)
	}
	cs, err := parseCommitCSV(out)
	if err != nil {
		return nil, err
	}
	// reverse → oldest first
	for i, j := 0, len(cs)-1; i < j; i, j = i+1, j-1 {
		cs[i], cs[j] = cs[j], cs[i]
	}
	return cs, nil
}

func doltDiffSQL(repo, parent, child, table string) (string, error) {
	out, errs, err := run(repo, "", "dolt", "diff", "-r", "sql", parent, child, "--", table)
	if err != nil {
		if strings.Contains(errs, "does not exist") {
			return "", nil
		}
		return "", fmt.Errorf("dolt diff: %v\n%s", err, errs)
	}
	return out, nil
}

func doltliteLog(db string, limit int) ([]Commit, error) {
	sql := fmt.Sprintf("SELECT commit_hash, committer, email, date, message FROM dolt_log ORDER BY date DESC LIMIT %d", limit+1)
	out, errs, err := run("", "", "doltlite", "-csv", "-header", db, sql)
	if err != nil {
		return nil, fmt.Errorf("doltlite log: %v\n%s", err, errs)
	}
	cs, err := parseCommitCSV(out)
	if err != nil {
		return nil, err
	}
	for i, j := 0, len(cs)-1; i < j; i, j = i+1, j-1 {
		cs[i], cs[j] = cs[j], cs[i]
	}
	return cs, nil
}

func doltliteDiffSQL(db, parent, child, table string) (string, error) {
	colsOut, _, err := run("", "", "doltlite", "-csv", "-header", db,
		fmt.Sprintf("SELECT name FROM pragma_table_info('%s')", table))
	if err != nil {
		return "", err
	}
	cr := csv.NewReader(strings.NewReader(colsOut))
	rows, err := cr.ReadAll()
	if err != nil || len(rows) < 2 {
		return "", err
	}
	cols := make([]string, 0, len(rows)-1)
	for _, r := range rows[1:] {
		cols = append(cols, r[0])
	}

	dq := fmt.Sprintf("SELECT * FROM dolt_diff_%s WHERE to_commit='%s' AND from_commit='%s'", table, child, parent)
	dOut, _, err := run("", "", "doltlite", "-csv", "-header", db, dq)
	if err != nil {
		return "", nil
	}
	dr := csv.NewReader(strings.NewReader(dOut))
	dr.FieldsPerRecord = -1
	drows, _ := dr.ReadAll()
	if len(drows) < 2 {
		return "", nil
	}
	hdr := drows[0]
	idx := func(n string) int {
		for i, h := range hdr {
			if h == n {
				return i
			}
		}
		return -1
	}
	dtIdx := idx("diff_type")
	q := func(s string) string {
		if s == "" {
			return "NULL"
		}
		return "'" + strings.ReplaceAll(s, "'", "''") + "'"
	}

	var sb strings.Builder
	for _, r := range drows[1:] {
		op := r[dtIdx]
		switch op {
		case "added":
			vs := make([]string, len(cols))
			cs := make([]string, len(cols))
			for i, c := range cols {
				vs[i] = q(r[idx("to_"+c)])
				cs[i] = `"` + c + `"`
			}
			fmt.Fprintf(&sb, "INSERT INTO \"%s\" (%s) VALUES (%s);\n",
				table, strings.Join(cs, ","), strings.Join(vs, ","))
		case "removed":
			parts := []string{}
			for _, c := range cols {
				if v := r[idx("from_"+c)]; v != "" {
					parts = append(parts, fmt.Sprintf(`"%s"=%s`, c, q(v)))
				}
			}
			fmt.Fprintf(&sb, "DELETE FROM \"%s\" WHERE %s;\n", table, strings.Join(parts, " AND "))
		case "modified":
			sets := []string{}
			where := []string{}
			for _, c := range cols {
				sets = append(sets, fmt.Sprintf(`"%s"=%s`, c, q(r[idx("to_"+c)])))
				if v := r[idx("from_"+c)]; v != "" {
					where = append(where, fmt.Sprintf(`"%s"=%s`, c, q(v)))
				}
			}
			fmt.Fprintf(&sb, "UPDATE \"%s\" SET %s WHERE %s;\n",
				table, strings.Join(sets, ","), strings.Join(where, " AND "))
		}
	}
	return sb.String(), nil
}

// ---------- SQL dialect translator ----------

var (
	reEngine    = regexp.MustCompile(`(?s)\)\s*ENGINE=\w+[^;]*;`)
	reCollate   = regexp.MustCompile(`(?i)\bCOLLATE\s+\w+`)
	reCharset   = regexp.MustCompile(`(?i)\bCHARACTER SET\s+\w+`)
	reCharsetD  = regexp.MustCompile(`(?i)\bDEFAULT CHARSET=\w+`)
	reIntTypes  = regexp.MustCompile(`(?i)\b(tinyint|smallint|mediumint|int|bigint)(\(\d+\))?\b`)
	reVarchar   = regexp.MustCompile(`(?i)\bvarchar\s*\(\d+\)`)
	reLongtext  = regexp.MustCompile(`(?i)\b(longtext|mediumtext|tinytext)\b`)
	reDatetime  = regexp.MustCompile(`(?i)\bdatetime\b`)
	reTimestamp = regexp.MustCompile(`(?i)\btimestamp\b`)
)

func translateForSQLite(sql string) string {
	sql = reEngine.ReplaceAllString(sql, ");")
	sql = reCollate.ReplaceAllString(sql, "")
	sql = reCharset.ReplaceAllString(sql, "")
	sql = reCharsetD.ReplaceAllString(sql, "")
	sql = reIntTypes.ReplaceAllString(sql, "INTEGER")
	sql = reVarchar.ReplaceAllString(sql, "TEXT")
	sql = reLongtext.ReplaceAllString(sql, "TEXT")
	sql = reDatetime.ReplaceAllString(sql, "TEXT")
	sql = reTimestamp.ReplaceAllString(sql, "TEXT")
	return strings.ReplaceAll(sql, "`", `"`)
}

func translateForDolt(sql string) string {
	// SQLite → Dolt (limited): swap double-quote idents → backticks
	return strings.ReplaceAll(sql, `"`, "`")
}

// ---------- target writers ----------

func applyToDolt(repo, sql, msg, author, email, date string) error {
	full := "SET FOREIGN_KEY_CHECKS=0;\n" + sql + "\nSET FOREIGN_KEY_CHECKS=1;\n"
	if _, errs, err := run(repo, full, "dolt", "sql"); err != nil {
		return fmt.Errorf("dolt sql: %v\n%s", err, errs)
	}
	if _, _, err := run(repo, "", "dolt", "add", "-A"); err != nil {
		return err
	}
	out, errs, err := run(repo, "", "dolt", "commit", "-m", msg,
		"--author", fmt.Sprintf("%s <%s>", author, email),
		"--date", date)
	if err != nil && !strings.Contains(out+errs, "nothing to commit") {
		return fmt.Errorf("dolt commit: %v\n%s", err, errs)
	}
	return nil
}

// chunkSize: split many INSERTs into batches. The historical reason was the
// v0.9.0 row-loss bug (fixed in v0.9.1, see KNOWN_ISSUES.md); we still chunk +
// wrap each chunk in BEGIN/COMMIT to keep failures localized and memory bounded.
const chunkSize = 1500

func applyToDoltlite(db, sql, msg, author, email, date string) error {
	stmts := splitStatements(sql)
	fmt.Fprintf(os.Stderr, "    [%d statements → %d chunks of <=%d, BEGIN/COMMIT wrapped]\n",
		len(stmts), (len(stmts)+chunkSize-1)/chunkSize, chunkSize)
	for i := 0; i < len(stmts); i += chunkSize {
		end := i + chunkSize
		if end > len(stmts) {
			end = len(stmts)
		}
		f, err := os.CreateTemp("", "replay-*.sql")
		if err != nil {
			return err
		}
		// CREATE TABLE can't run inside SQLite transactions if other tx is
		// open — but a fresh DB has no open tx, so safe. We always wrap.
		io.WriteString(f, "BEGIN;\n")
		for _, s := range stmts[i:end] {
			io.WriteString(f, s+";\n")
		}
		io.WriteString(f, "COMMIT;\n")
		f.Close()
		out, errs, err := run("", "", "doltlite", "-bail", db, "-cmd", ".read "+f.Name())
		os.Remove(f.Name())
		if err != nil {
			return fmt.Errorf("doltlite chunk %d-%d: %v\n%s\n%s", i, end, err, errs, truncate(out, 300))
		}
	}
	commitSQL := fmt.Sprintf("SELECT dolt_commit('-A', '-m', '%s', '--author', '%s <%s>', '--date', '%s');",
		strings.ReplaceAll(msg, "'", "''"), author, email, date)
	if _, errs, err := run("", "", "doltlite", db, commitSQL); err != nil {
		return fmt.Errorf("doltlite commit: %v\n%s", err, errs)
	}
	return nil
}

// splitStatements naively splits on `;` followed by newline. Good enough for
// dolt-diff output (one statement per line) but not for embedded `;\n` inside
// quoted strings (rare in practice for our schema/data).
func splitStatements(sql string) []string {
	parts := strings.Split(sql, ";\n")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "...[truncated]"
}

// ---------- main ----------

func main() {
	var (
		srcKind = flag.String("src-kind", "", "Source: dolt | doltlite")
		src     = flag.String("src", "", "Source path: dolt repo dir or doltlite db file")
		dstKind = flag.String("dst-kind", "", "Target: dolt | doltlite")
		dst     = flag.String("dst", "", "Target path")
		table   = flag.String("table", "", "Table to replay (POC: one at a time)")
		limit   = flag.Int("limit", 5, "Number of recent commits to walk (replays last N)")
		dryRun  = flag.Bool("dry-run", false, "Print SQL, do not apply")
	)
	flag.Parse()
	for _, p := range []struct{ name, val string }{
		{"--src-kind", *srcKind}, {"--src", *src},
		{"--dst-kind", *dstKind}, {"--dst", *dst},
		{"--table", *table},
	} {
		if p.val == "" {
			fmt.Fprintf(os.Stderr, "missing required flag %s\n", p.name)
			os.Exit(2)
		}
	}

	var (
		commits []Commit
		err     error
	)
	if *srcKind == "dolt" {
		commits, err = doltLog(*src, *limit)
	} else {
		commits, err = doltliteLog(*src, *limit)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "[%s] %d commits to walk\n", *srcKind, len(commits))
	if len(commits) < 2 {
		fmt.Fprintln(os.Stderr, "need at least 2 commits (a parent + a child)")
		os.Exit(1)
	}

	parent := commits[0].Hash
	replayed := 0
	for _, c := range commits[1:] {
		var sql string
		if *srcKind == "dolt" {
			sql, err = doltDiffSQL(*src, parent, c.Hash, *table)
		} else {
			sql, err = doltliteDiffSQL(*src, parent, c.Hash, *table)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}

		if *dstKind == "doltlite" {
			sql = translateForSQLite(sql)
		} else {
			sql = translateForDolt(sql)
		}

		short := c.Hash
		if len(short) > 10 {
			short = short[:10]
		}
		msg := strings.SplitN(c.Message, "\n", 2)[0]
		if len(msg) > 60 {
			msg = msg[:60]
		}
		fmt.Fprintf(os.Stderr, "\n=== %s | %s | %s | %s ===\n", short, c.Date, c.Author, msg)
		fmt.Fprintf(os.Stderr, "    SQL: %d bytes\n", len(sql))

		if strings.TrimSpace(sql) == "" {
			fmt.Fprintln(os.Stderr, "    (no changes to this table — skip)")
			parent = c.Hash
			continue
		}

		if *dryRun {
			fmt.Println(truncate(sql, 800))
		} else {
			if *dstKind == "dolt" {
				err = applyToDolt(*dst, sql, c.Message, c.Author, c.Email, c.Date)
			} else {
				err = applyToDoltlite(*dst, sql, c.Message, c.Author, c.Email, c.Date)
			}
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			replayed++
		}
		parent = c.Hash
	}
	fmt.Fprintf(os.Stderr, "\n[done] replayed %d commits\n", replayed)
}
