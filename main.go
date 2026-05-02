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
	"time"
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
	reAutoIncOption  = regexp.MustCompile(`(?i)\s+AUTO_INCREMENT=\d+\b`)
	reDecimal        = regexp.MustCompile(`(?i)\bdecimal\s*\(\d+\s*,\s*\d+\)`)
	// MySQL's "ON UPDATE CURRENT_TIMESTAMP" column attribute (and other ON
	// UPDATE expressions on column defs) — SQLite has no inline equivalent.
	// Stripped; if needed, callers must add a trigger.
	reOnUpdate       = regexp.MustCompile(`(?i)\s+ON\s+UPDATE\s+\w+(\s*\([^)]*\))?`)
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
	sql = reAutoIncOption.ReplaceAllString(sql, "")
	sql = reDecimal.ReplaceAllString(sql, "NUMERIC")
	sql = reOnUpdate.ReplaceAllString(sql, "")
	sql = extractInlineKeys(sql)
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

// extractInlineKeys rewrites MySQL inline `KEY name (cols)` and
// `UNIQUE KEY name (cols)` clauses inside CREATE TABLE bodies into separate
// CREATE [UNIQUE] INDEX statements emitted after the table.
//
// SQLite/doltlite reject these inline; without this pass any CREATE TABLE
// containing an inline KEY halts the chunk and (worse) the table is never
// created, so every later commit referring to it fails with "no such table".
func extractInlineKeys(sql string) string {
	var out strings.Builder
	out.Grow(len(sql) + 64)
	i := 0
	for i < len(sql) {
		// Find next CREATE TABLE
		j := strings.Index(strings.ToUpper(sql[i:]), "CREATE TABLE")
		if j < 0 {
			out.WriteString(sql[i:])
			break
		}
		j += i
		out.WriteString(sql[i:j])

		// Locate the opening '(' of the column-definition body
		open := strings.IndexByte(sql[j:], '(')
		if open < 0 {
			out.WriteString(sql[j:])
			break
		}
		open += j

		// Walk balanced parens to find the matching ')'
		depth := 0
		end := -1
		for k := open; k < len(sql); k++ {
			switch sql[k] {
			case '(':
				depth++
			case ')':
				depth--
				if depth == 0 {
					end = k
				}
			}
			if end >= 0 {
				break
			}
		}
		if end < 0 {
			out.WriteString(sql[j:])
			break
		}

		header := sql[j : open+1]
		body := sql[open+1 : end]
		tail := sql[end:] // ");" plus rest

		// Pull table name from the header for the index DDL.
		tableName := ""
		if m := regexp.MustCompile("(?i)CREATE TABLE\\s+(IF NOT EXISTS\\s+)?[`\"]?(\\w+)[`\"]?").FindStringSubmatch(header); m != nil {
			tableName = m[2]
		}

		keptParts := []string{}
		var indexes []string
		for _, raw := range splitTopLevel(body, ',') {
			t := strings.TrimSpace(raw)
			low := strings.ToLower(t)
			if strings.HasPrefix(low, "unique key") || strings.HasPrefix(low, "key ") || low == "key" {
				if idx := makeIndexFromKey(t, tableName); idx != "" {
					indexes = append(indexes, idx)
					continue
				}
			}
			keptParts = append(keptParts, raw)
		}

		out.WriteString(header)
		out.WriteString(strings.Join(keptParts, ","))
		// Find end of statement (the ';' after `tail`'s leading ')')
		semi := strings.IndexByte(tail, ';')
		if semi < 0 {
			out.WriteString(tail)
			i = len(sql)
			continue
		}
		out.WriteString(tail[:semi+1])
		for _, idx := range indexes {
			out.WriteByte('\n')
			out.WriteString(idx)
		}
		i = end + semi + 1
	}
	return out.String()
}

// splitTopLevel splits `s` on `sep` ignoring separators inside parens or
// inside single/double-quoted strings.
func splitTopLevel(s string, sep byte) []string {
	var parts []string
	depth := 0
	inSQ, inDQ, inBT := false, false, false
	start := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\\' && (inSQ || inDQ):
			i++ // skip escaped char
			continue
		case c == '\'' && !inDQ && !inBT:
			inSQ = !inSQ
		case c == '"' && !inSQ && !inBT:
			inDQ = !inDQ
		case c == '`' && !inSQ && !inDQ:
			inBT = !inBT
		case !inSQ && !inDQ && !inBT && c == '(':
			depth++
		case !inSQ && !inDQ && !inBT && c == ')':
			depth--
		case !inSQ && !inDQ && !inBT && depth == 0 && c == sep:
			parts = append(parts, s[start:i])
			start = i + 1
		}
	}
	parts = append(parts, s[start:])
	return parts
}

var reKeyClause = regexp.MustCompile("(?is)^(UNIQUE\\s+)?KEY\\s+[`\"]?(\\w+)[`\"]?\\s*\\((.+)\\)\\s*$")

func makeIndexFromKey(clause, table string) string {
	m := reKeyClause.FindStringSubmatch(strings.TrimSpace(clause))
	if m == nil || table == "" {
		return ""
	}
	uniq := ""
	if strings.TrimSpace(m[1]) != "" {
		uniq = "UNIQUE "
	}
	return fmt.Sprintf("CREATE %sINDEX IF NOT EXISTS `%s_%s` ON `%s` (%s);",
		uniq, table, m[2], table, m[3])
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

// repairCtx carries source-side context that lets applyToDoltlite recover
// from "no such table" errors by fetching the missing schema from the source
// dolt repo at the failing commit and applying it before retrying the chunk.
// Empty values disable repair.
//
// The schema-bootstrap path (no such table) is on by default; the
// table-rebuild path (DROP PK column → rebuild table) is gated behind
// rebuild and off by default because the current implementation has
// known bugs that cascade into duplicate-column / quote errors on
// follow-up commits.
type repairCtx struct {
	srcKind, src, sourceCommit string
	rebuild                    bool
}

var (
	reNoSuchTable = regexp.MustCompile(`(?i)no such table:\s*"?(\w+)"?`)
	// SQLite refuses ALTER TABLE … DROP COLUMN when the column is part of the
	// primary key. The error doesn't name the table, so we recover the (table,
	// column) pair from the chunk's own ALTER statement instead.
	reDropPKColErr  = regexp.MustCompile(`(?i)cannot drop PRIMARY KEY column:?\s*"?(\w+)"?`)
	// MySQL accepts both `ALTER TABLE x DROP COLUMN y` and the shorthand
	// `ALTER TABLE x DROP y`; dolt diff emits the shorthand.
	reAlterDropCol  = regexp.MustCompile("(?i)ALTER TABLE [`\"]?(\\w+)[`\"]? DROP (?:COLUMN )?[`\"]?(\\w+)[`\"]?")
)

func applyToDoltlite(db, sql, msg, author, email, date string, rc repairCtx) error {
	stmts := splitStatements(sql)
	fmt.Fprintf(os.Stderr, "    [%d statements → %d chunks of <=%d, BEGIN/COMMIT wrapped]\n",
		len(stmts), (len(stmts)+chunkSize-1)/chunkSize, chunkSize)
	for i := 0; i < len(stmts); i += chunkSize {
		end := i + chunkSize
		if end > len(stmts) {
			end = len(stmts)
		}
		const maxRepair = 5
		var lastErr error
		for attempt := 0; attempt <= maxRepair; attempt++ {
			f, err := os.CreateTemp("", "replay-*.sql")
			if err != nil {
				return err
			}
			io.WriteString(f, "BEGIN;\n")
			for _, s := range stmts[i:end] {
				io.WriteString(f, s+";\n")
			}
			io.WriteString(f, "COMMIT;\n")
			f.Close()
			t0 := time.Now()
			out, errs, err := run("", "", "doltlite", "-bail", db, "-cmd", ".read "+f.Name())
			dt := time.Since(t0)
			os.Remove(f.Name())
			if dt > 500*time.Millisecond {
				fmt.Fprintf(os.Stderr, "      chunk[%d-%d] apply %.2fs\n", i, end, dt.Seconds())
			}
			if err == nil {
				lastErr = nil
				break
			}
			lastErr = fmt.Errorf("doltlite chunk %d-%d: %v\n%s\n%s", i, end, err, errs, truncate(out, 300))

			if rc.srcKind != "dolt" || rc.src == "" || rc.sourceCommit == "" {
				return lastErr
			}
			combined := errs + out
			if m := reNoSuchTable.FindStringSubmatch(combined); m != nil {
				missing := m[1]
				fmt.Fprintf(os.Stderr, "    [repair] missing table %q — fetching schema from source @ %s\n",
					missing, short10(rc.sourceCommit))
				if err := bootstrapTableFromDolt(db, rc.src, rc.sourceCommit, missing); err != nil {
					return fmt.Errorf("schema bootstrap for %q failed: %v (original: %v)", missing, err, lastErr)
				}
				continue
			}
			if rc.rebuild && reDropPKColErr.MatchString(combined) {
				// Find the offending ALTER in this chunk to learn which table/col,
				// then rebuild the table from source's post-commit schema and
				// strip the ALTER from the chunk so the retry doesn't re-trigger.
				var tbl, col string
				for k := i; k < end; k++ {
					if m := reAlterDropCol.FindStringSubmatch(stmts[k]); m != nil {
						tbl, col = m[1], m[2]
						stmts[k] = fmt.Sprintf("-- rebuilt: ALTER TABLE %s DROP COLUMN %s", tbl, col)
						break
					}
				}
				if tbl == "" {
					return lastErr
				}
				fmt.Fprintf(os.Stderr, "    [repair] DROP PK column %s.%s — rebuilding table from source @ %s\n",
					tbl, col, short10(rc.sourceCommit))
				if err := rebuildTableFromDolt(db, rc.src, rc.sourceCommit, tbl); err != nil {
					return fmt.Errorf("table rebuild for %q failed: %v (original: %v)", tbl, err, lastErr)
				}
				continue
			}
			return lastErr
		}
		if lastErr != nil {
			return lastErr
		}
	}
	commitSQL := fmt.Sprintf("SELECT dolt_commit('-A', '-m', '%s', '--author', '%s <%s>', '--date', '%s');",
		strings.ReplaceAll(msg, "'", "''"), author, email, date)
	tc0 := time.Now()
	out, errs, err := run("", "", "doltlite", db, commitSQL)
	dtc := time.Since(tc0)
	if dtc > 500*time.Millisecond {
		fmt.Fprintf(os.Stderr, "      dolt_commit %.2fs\n", dtc.Seconds())
	}
	if err != nil {
		// All of the diff translated away to "-- skipped" comments; no working
		// set delta to commit. Treat as benign no-op so the walk can continue.
		if strings.Contains(errs+out, "nothing to commit") {
			return nil
		}
		return fmt.Errorf("doltlite commit: %v\n%s", err, errs)
	}
	return nil
}

// bootstrapTableFromDolt asks the source dolt repo for `SHOW CREATE TABLE
// <table> AS OF '<commit>'`, translates it for SQLite, and applies on the
// target. Used when an apply chunk hits "no such table" because an earlier
// schema-changing commit was filtered to no-ops.
func bootstrapTableFromDolt(db, repo, commit, table string) error {
	q := fmt.Sprintf("SHOW CREATE TABLE `%s` AS OF '%s'", table, commit)
	out, errs, err := run(repo, "", "dolt", "sql", "-r", "csv", "-q", q)
	if err != nil {
		return fmt.Errorf("dolt sql: %v\n%s", err, errs)
	}
	r := csv.NewReader(strings.NewReader(out))
	r.FieldsPerRecord = -1
	rows, err := r.ReadAll()
	if err != nil || len(rows) < 2 || len(rows[1]) < 2 {
		return fmt.Errorf("unexpected SHOW CREATE TABLE output: %q", out)
	}
	createSQL := rows[1][1] + ";\n"
	createSQL = translateForSQLite(createSQL)
	if _, errs, err := run("", "", "doltlite", db, createSQL); err != nil {
		return fmt.Errorf("create on target: %v\n%s\nSQL: %s", err, errs, truncate(createSQL, 400))
	}
	return nil
}

// rebuildTableFromDolt rebuilds an existing target table to match the source's
// post-commit schema, copying rows from the existing target table for columns
// that survive in the new schema. Used to emulate ALTERs that SQLite refuses
// (DROP PK column, MODIFY COLUMN, change PK set).
//
// Procedure: CREATE <tbl>__rebuild with new schema, INSERT INTO <tbl>__rebuild
// (cols∩) SELECT cols∩ FROM <tbl>, DROP <tbl>, ALTER RENAME __rebuild → <tbl>.
// Wrapped in a transaction so a failure leaves the original table intact.
func rebuildTableFromDolt(db, repo, commit, table string) error {
	q := fmt.Sprintf("SHOW CREATE TABLE `%s` AS OF '%s'", table, commit)
	out, errs, err := run(repo, "", "dolt", "sql", "-r", "csv", "-q", q)
	if err != nil {
		return fmt.Errorf("dolt sql: %v\n%s", err, errs)
	}
	r := csv.NewReader(strings.NewReader(out))
	r.FieldsPerRecord = -1
	rows, err := r.ReadAll()
	if err != nil || len(rows) < 2 || len(rows[1]) < 2 {
		return fmt.Errorf("unexpected SHOW CREATE TABLE output: %q", out)
	}
	rebuildName := table + "__rebuild"
	createSQL := translateForSQLite(rows[1][1] + ";\n")
	// Rename the new table inside the CREATE so it doesn't collide.
	createSQL = strings.Replace(createSQL,
		fmt.Sprintf("CREATE TABLE IF NOT EXISTS \"%s\"", table),
		fmt.Sprintf("CREATE TABLE \"%s\"", rebuildName), 1)

	newCols, err := tableColumns(db, rebuildName, createSQL) // peek from SQL, table doesn't exist yet
	if err != nil {
		return err
	}
	oldCols, err := tableColumns(db, table, "")
	if err != nil {
		return err
	}
	common := intersect(newCols, oldCols)
	if len(common) == 0 {
		return fmt.Errorf("no overlapping columns between old %v and new %v", oldCols, newCols)
	}
	colList := strings.Join(quoteAll(common, "\""), ",")
	migration := fmt.Sprintf(`BEGIN;
%s
INSERT INTO "%s" (%s) SELECT %s FROM "%s";
DROP TABLE "%s";
ALTER TABLE "%s" RENAME TO "%s";
COMMIT;`, createSQL, rebuildName, colList, colList, table, table, rebuildName, table)
	if _, errs, err := run("", "", "doltlite", db, migration); err != nil {
		return fmt.Errorf("apply rebuild: %v\n%s\nSQL: %s", err, errs, truncate(migration, 600))
	}
	return nil
}

// tableColumns returns the column names of `table` in `db`. If sqlSnippet is
// non-empty, parse columns from the CREATE TABLE in the snippet instead of
// querying the live table (used when the table doesn't exist on target yet).
func tableColumns(db, table, sqlSnippet string) ([]string, error) {
	if sqlSnippet != "" {
		// Find balanced () after CREATE TABLE … and split top-level commas;
		// take the leading word of each part as the column name (ignoring
		// CONSTRAINT/PRIMARY KEY/etc lines).
		open := strings.IndexByte(sqlSnippet, '(')
		if open < 0 {
			return nil, fmt.Errorf("no body in CREATE TABLE")
		}
		depth := 0
		end := -1
		for i := open; i < len(sqlSnippet); i++ {
			switch sqlSnippet[i] {
			case '(':
				depth++
			case ')':
				depth--
				if depth == 0 {
					end = i
				}
			}
			if end >= 0 {
				break
			}
		}
		if end < 0 {
			return nil, fmt.Errorf("unbalanced () in CREATE TABLE")
		}
		var cols []string
		for _, p := range splitTopLevel(sqlSnippet[open+1:end], ',') {
			t := strings.TrimSpace(p)
			low := strings.ToLower(t)
			if strings.HasPrefix(low, "primary key") || strings.HasPrefix(low, "unique") ||
				strings.HasPrefix(low, "constraint") || strings.HasPrefix(low, "foreign key") ||
				strings.HasPrefix(low, "key ") || strings.HasPrefix(low, "check") {
				continue
			}
			name := t
			if i := strings.IndexAny(t, " \t"); i > 0 {
				name = t[:i]
			}
			name = strings.Trim(name, "`\"")
			if name != "" {
				cols = append(cols, name)
			}
		}
		return cols, nil
	}
	out, errs, err := run("", "", "doltlite", "-csv", "-header", db,
		fmt.Sprintf("SELECT name FROM pragma_table_info('%s')", table))
	if err != nil {
		return nil, fmt.Errorf("pragma_table_info: %v\n%s", err, errs)
	}
	cr := csv.NewReader(strings.NewReader(out))
	rows, err := cr.ReadAll()
	if err != nil {
		return nil, err
	}
	if len(rows) < 2 {
		return nil, fmt.Errorf("table %q has no columns (or doesn't exist)", table)
	}
	cols := make([]string, 0, len(rows)-1)
	for _, r := range rows[1:] {
		cols = append(cols, r[0])
	}
	return cols, nil
}

func intersect(a, b []string) []string {
	bs := map[string]bool{}
	for _, x := range b {
		bs[x] = true
	}
	var out []string
	for _, x := range a {
		if bs[x] {
			out = append(out, x)
		}
	}
	return out
}

func quoteAll(ss []string, q string) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = q + s + q
	}
	return out
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
		rebuild = flag.Bool("rebuild-on-pk-drop", false, "EXPERIMENTAL: emulate ALTER DROP PK-column via full table rebuild (currently has bugs)")
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
				err = applyToDoltlite(*dst, sql, c.Message, c.Author, c.Email, c.Date,
					repairCtx{srcKind: *srcKind, src: *src, sourceCommit: c.Hash, rebuild: *rebuild})
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
