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
	Parent                             string // empty for root commits
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
	pH := col("parent_hash") // -1 if absent
	out := make([]Commit, 0, len(rows)-1)
	for _, r := range rows[1:] {
		c := Commit{Hash: r[hH], Author: r[aH], Email: r[eH], Date: r[dH], Message: r[mH]}
		if pH >= 0 {
			c.Parent = r[pH]
		}
		out = append(out, c)
	}
	return out, nil
}

// doltLog returns commits in chronological order (oldest first), each with its
// first-parent hash (or "" for the root commit). limit==0 means unlimited.
func doltLog(repo string, limit int) ([]Commit, error) {
	limitClause := ""
	if limit > 0 {
		// +1 because for "last N" callers want N child diffs, which need N+1
		// commits (parent + N children). The chronological walk handles parent
		// via the join below, so the +1 just ensures we don't strand a child.
		limitClause = fmt.Sprintf(" LIMIT %d", limit+1)
	}
	// LEFT JOIN: root commit has no row in dolt_commit_ancestors, so parent_hash → NULL → "".
	sql := "SELECT l.commit_hash, l.committer, l.email, l.date, l.message, " +
		"COALESCE(a.parent_hash, '') AS parent_hash " +
		"FROM dolt_log l LEFT JOIN dolt_commit_ancestors a " +
		"ON a.commit_hash = l.commit_hash AND a.parent_index = 0 " +
		"ORDER BY l.date ASC" + limitClause
	out, errs, err := run(repo, "", "dolt", "sql", "-r", "csv", "-q", sql)
	if err != nil {
		return nil, fmt.Errorf("dolt log: %v\n%s", err, errs)
	}
	return parseCommitCSV(out)
}

// doltDiffSQL emits diff SQL between parent..child. If table=="" the diff
// covers every changed table (CREATE TABLE included for first appearance).
func doltDiffSQL(repo, parent, child, table string) (string, error) {
	args := []string{"diff", "-r", "sql", parent, child}
	if table != "" {
		args = append(args, "--", table)
	}
	out, errs, err := run(repo, "", "dolt", args...)
	if err != nil {
		if strings.Contains(errs, "does not exist") {
			return "", nil
		}
		return "", fmt.Errorf("dolt diff: %v\n%s", err, errs)
	}
	out = stripDoltDiffNoise(out)
	out = rewriteColumnSwaps(out)
	return out, nil
}

// stripDoltDiffNoise removes diagnostic lines that `dolt diff -r sql` emits
// inline with SQL ("Incompatible schema change…", "Primary key sets differ…",
// "warnings during diff…"). Without filtering these become syntax errors.
func stripDoltDiffNoise(s string) string {
	noisePrefixes := []string{
		"Incompatible schema change",
		"Primary key sets differ",
		"warnings during diff",
		"Warning: ",
	}
	var b strings.Builder
	b.Grow(len(s))
nextLine:
	for _, line := range strings.Split(s, "\n") {
		t := strings.TrimSpace(line)
		for _, p := range noisePrefixes {
			if strings.HasPrefix(t, p) {
				continue nextLine
			}
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}

var reRenameCol = regexp.MustCompile("(?i)^ALTER TABLE [`\"]?(\\w+)[`\"]? RENAME COLUMN [`\"]?(\\w+)[`\"]? TO [`\"]?(\\w+)[`\"]?;\\s*$")

// rewriteColumnSwaps detects the pattern
//
//	ALTER TABLE T RENAME COLUMN A TO B;
//	ALTER TABLE T RENAME COLUMN B TO A;
//
// (which dolt happily emits but every SQL engine — including doltlite —
// rejects because step 1 produces two columns named B) and rewrites it as a
// three-step swap through a generated temporary name.
func rewriteColumnSwaps(s string) string {
	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	i := 0
	for i < len(lines) {
		m1 := reRenameCol.FindStringSubmatch(lines[i])
		if m1 != nil && i+1 < len(lines) {
			m2 := reRenameCol.FindStringSubmatch(lines[i+1])
			if m2 != nil && m1[1] == m2[1] && m1[2] == m2[3] && m1[3] == m2[2] {
				t, a, b := m1[1], m1[2], m1[3]
				tmp := fmt.Sprintf("__swap_%s_%s", a, b)
				out = append(out,
					fmt.Sprintf("ALTER TABLE `%s` RENAME COLUMN `%s` TO `%s`;", t, a, tmp),
					fmt.Sprintf("ALTER TABLE `%s` RENAME COLUMN `%s` TO `%s`;", t, b, a),
					fmt.Sprintf("ALTER TABLE `%s` RENAME COLUMN `%s` TO `%s`;", t, tmp, b),
				)
				i += 2
				continue
			}
		}
		out = append(out, lines[i])
		i++
	}
	return strings.Join(out, "\n")
}

func doltliteLog(db string, limit int) ([]Commit, error) {
	limitClause := ""
	if limit > 0 {
		limitClause = fmt.Sprintf(" LIMIT %d", limit+1)
	}
	// doltlite doesn't expose dolt_commit_ancestors yet — fetch in date order
	// and chain parents in Go from the result rows. Root parent stays "".
	sql := "SELECT commit_hash, committer, email, date, message FROM dolt_log " +
		"ORDER BY date ASC" + limitClause
	out, errs, err := run("", "", "doltlite", "-csv", "-header", db, sql)
	if err != nil {
		return nil, fmt.Errorf("doltlite log: %v\n%s", err, errs)
	}
	cs, err := parseCommitCSV(out)
	if err != nil {
		return nil, err
	}
	for i := range cs {
		if i == 0 {
			continue
		}
		cs[i].Parent = cs[i-1].Hash
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
	reEnum      = regexp.MustCompile(`(?is)\benum\s*\([^)]*\)`)
	reRenameTbl = regexp.MustCompile("(?i)\\bRENAME TABLE ([`\"]?\\w+[`\"]?) TO ([`\"]?\\w+[`\"]?)")

	// MySQL-only ALTERs that SQLite/doltlite reject. We translate what we can
	// and strip the rest with a comment so a future pass can audit them.
	reAddIndex       = regexp.MustCompile("(?is)ALTER TABLE ([`\"]?\\w+[`\"]?) ADD (UNIQUE )?INDEX ([`\"]?\\w+[`\"]?)\\s*\\(([^)]+)\\);")
	reAddFKConstr    = regexp.MustCompile("(?is)ALTER TABLE [`\"]?\\w+[`\"]? ADD CONSTRAINT [^;]*FOREIGN KEY[^;]*;")
	reModifyColumn   = regexp.MustCompile("(?is)ALTER TABLE [`\"]?\\w+[`\"]? MODIFY COLUMN [^;]*;")
	reDropPK         = regexp.MustCompile("(?is)ALTER TABLE [`\"]?\\w+[`\"]? DROP PRIMARY KEY;")
	reAddPK          = regexp.MustCompile("(?is)ALTER TABLE [`\"]?\\w+[`\"]? ADD PRIMARY KEY[^;]*;")
	reDropIndex      = regexp.MustCompile("(?is)ALTER TABLE ([`\"]?\\w+[`\"]?) DROP INDEX ([`\"]?\\w+[`\"]?);")
	reAfterClause    = regexp.MustCompile(`(?i)\s+AFTER\s+\x60\w+\x60`)
	reAutoIncrement  = regexp.MustCompile(`(?i)\s+AUTO_INCREMENT\b`)
	reCreateTbl      = regexp.MustCompile("(?i)\\bCREATE TABLE ([`\"])")
	reInsertInto     = regexp.MustCompile("(?i)\\bINSERT INTO ([`\"])")
	reSet       = regexp.MustCompile(`(?is)\bset\s*\([^)]*\)`)
	reVarchar   = regexp.MustCompile(`(?i)\bvarchar\s*\(\d+\)`)
	reChar      = regexp.MustCompile(`(?i)\bchar\s*\(\d+\)`)
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
	sql = reEnum.ReplaceAllString(sql, "TEXT")
	sql = reSet.ReplaceAllString(sql, "TEXT")
	sql = reVarchar.ReplaceAllString(sql, "TEXT")
	sql = reChar.ReplaceAllString(sql, "TEXT")
	sql = reLongtext.ReplaceAllString(sql, "TEXT")
	sql = reDatetime.ReplaceAllString(sql, "TEXT")
	sql = reTimestamp.ReplaceAllString(sql, "TEXT")
	sql = reRenameTbl.ReplaceAllString(sql, "ALTER TABLE $1 RENAME TO $2")
	// AFTER `col` is a MySQL ordering hint with no SQLite analogue; strip it.
	sql = reAfterClause.ReplaceAllString(sql, "")
	// ADD INDEX / ADD UNIQUE INDEX → CREATE [UNIQUE] INDEX
	sql = reAddIndex.ReplaceAllStringFunc(sql, func(m string) string {
		parts := reAddIndex.FindStringSubmatch(m)
		uniq := strings.TrimSpace(parts[2])
		if uniq != "" {
			uniq = "UNIQUE "
		}
		return fmt.Sprintf("CREATE %sINDEX IF NOT EXISTS %s ON %s (%s);",
			uniq, parts[3], parts[1], parts[4])
	})
	// SQLite can't add FK / change PK / modify column on an existing table
	// without a full table rebuild. Skip with a SQL comment so the file is
	// still valid — KNOWN_ISSUES.md tracks the schema-rebuild gap.
	sql = reAddFKConstr.ReplaceAllString(sql, "-- skipped: ADD FOREIGN KEY (sqlite limitation)")
	sql = reModifyColumn.ReplaceAllString(sql, "-- skipped: MODIFY COLUMN (sqlite limitation)")
	sql = reDropPK.ReplaceAllString(sql, "-- skipped: DROP PRIMARY KEY (sqlite limitation)")
	sql = reAddPK.ReplaceAllString(sql, "-- skipped: ADD PRIMARY KEY (sqlite limitation)")
	sql = reDropIndex.ReplaceAllString(sql, "DROP INDEX IF EXISTS $2;")
	sql = reAutoIncrement.ReplaceAllString(sql, "")
	// Replay tolerance: dolt history may DROP+CREATE the same table later, or
	// re-emit a row that's already present after we no-op'd a schema change.
	// Make CREATE/INSERT idempotent so transient state mismatches don't halt
	// the walk. Loses fidelity for true conflict-on-INSERT cases — acceptable
	// for migration use where the final state is what matters.
	sql = reCreateTbl.ReplaceAllString(sql, "CREATE TABLE IF NOT EXISTS $1")
	sql = reInsertInto.ReplaceAllString(sql, "INSERT OR REPLACE INTO $1")
	sql = strings.ReplaceAll(sql, "`", `"`)
	return mysqlEscapesToSQLite(sql)
}

// mysqlEscapesToSQLite converts MySQL backslash escapes inside single-quoted
// string literals (\\ \' \" \n \t \r \0 \b \Z) to SQLite's literal-or-doubled
// form. Outside string literals the input is passed through unchanged.
//
// Dolt emits MySQL-flavoured INSERTs which use `\'` for an embedded quote;
// SQLite treats backslash as literal and only recognizes `''` for the same
// purpose, so without this translation any prayer text containing an apostrophe
// (e.g. "Bahá'u'lláh") halts the .read at the first such row.
func mysqlEscapesToSQLite(sql string) string {
	var b strings.Builder
	b.Grow(len(sql))
	inStr := false
	for i := 0; i < len(sql); i++ {
		c := sql[i]
		if !inStr {
			b.WriteByte(c)
			if c == '\'' {
				inStr = true
			}
			continue
		}
		// inside string
		if c == '\\' && i+1 < len(sql) {
			n := sql[i+1]
			switch n {
			case '\'':
				b.WriteString("''")
			case '"', '\\':
				b.WriteByte(n)
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			case 'r':
				b.WriteByte('\r')
			case '0':
				// SQLite tolerates NUL in TEXT but it's a footgun; drop.
			case 'b':
				b.WriteByte('\b')
			case 'Z':
				b.WriteByte(0x1A)
			default:
				// unknown escape: keep both bytes verbatim
				b.WriteByte(c)
				b.WriteByte(n)
			}
			i++
			continue
		}
		if c == '\'' {
			// MySQL also accepts '' as an embedded quote; SQLite uses the same.
			if i+1 < len(sql) && sql[i+1] == '\'' {
				b.WriteString("''")
				i++
				continue
			}
			b.WriteByte(c)
			inStr = false
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
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
	if out, errs, err := run("", "", "doltlite", db, commitSQL); err != nil {
		// All of the diff translated away to "-- skipped" comments; no working
		// set delta to commit. Treat as benign no-op so the walk can continue.
		if strings.Contains(errs+out, "nothing to commit") {
			return nil
		}
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
		table   = flag.String("table", "", "Table to replay (default: all tables in each commit)")
		limit   = flag.Int("limit", 0, "Cap commits to walk in source order (0 = all, oldest→newest)")
		dryRun  = flag.Bool("dry-run", false, "Print SQL, do not apply")
		keepGo  = flag.Bool("continue-on-error", false, "Log apply failures and continue instead of aborting")
	)
	flag.Parse()
	for _, p := range []struct{ name, val string }{
		{"--src-kind", *srcKind}, {"--src", *src},
		{"--dst-kind", *dstKind}, {"--dst", *dst},
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
	if len(commits) == 0 {
		fmt.Fprintln(os.Stderr, "no commits found")
		os.Exit(1)
	}

	replayed := 0
	skipped := 0
	failed := 0
	for _, c := range commits {
		if c.Parent == "" {
			fmt.Fprintf(os.Stderr, "(skip root commit %s — no parent diff)\n", short10(c.Hash))
			skipped++
			continue
		}
		var sql string
		if *srcKind == "dolt" {
			sql, err = doltDiffSQL(*src, c.Parent, c.Hash, *table)
		} else {
			if *table == "" {
				fmt.Fprintln(os.Stderr, "doltlite source requires --table (whole-commit diff not implemented for doltlite)")
				os.Exit(2)
			}
			sql, err = doltliteDiffSQL(*src, c.Parent, c.Hash, *table)
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

		short := short10(c.Hash)
		msg := strings.SplitN(c.Message, "\n", 2)[0]
		if len(msg) > 60 {
			msg = msg[:60]
		}
		fmt.Fprintf(os.Stderr, "\n=== %s | %s | %s | %s ===\n", short, c.Date, c.Author, msg)
		fmt.Fprintf(os.Stderr, "    SQL: %d bytes\n", len(sql))

		if strings.TrimSpace(sql) == "" {
			fmt.Fprintln(os.Stderr, "    (no changes — skip)")
			skipped++
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
				if *keepGo {
					fmt.Fprintf(os.Stderr, "    FAIL: %v\n", err)
					failed++
					continue
				}
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			replayed++
		}
	}
	fmt.Fprintf(os.Stderr, "\n[done] replayed %d commits, skipped %d, failed %d\n", replayed, skipped, failed)
	if failed > 0 {
		os.Exit(1)
	}
}

func short10(s string) string {
	if len(s) > 10 {
		return s[:10]
	}
	return s
}
