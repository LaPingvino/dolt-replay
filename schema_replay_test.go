package main

// Tests for replaying combined schema-and-data commits — the failure
// mode dolthub/dolt#10988 surfaced. Each test sets up a doltlite source
// db with a specific commit pattern, replays into BOTH a fresh doltlite
// target AND a fresh dolt target, and compares per-row state across
// both directions.
//
// The test cases are organized by the failure mode they exercise:
//
//   simple             - schema-unchanged commits replay correctly
//   add_nullable       - ADD COLUMN with no default (bahaiwritings shape)
//   add_with_default   - ADD COLUMN with DEFAULT value (nicktobey case 1,
//                        currently a known gap — schema change populates
//                        existing rows but dolt_diff_<table> doesn't
//                        surface that)
//   drop_then_add      - DROP COLUMN + ADD COLUMN where the new column's
//                        post-update value happens to match the old one
//                        (nicktobey case 2, also a known gap — positional
//                        row-record aliasing makes it look like no change)
//   drop_only          - DROP COLUMN with no add
//
// Each non-skipped case runs both doltlite -> doltlite (intra-format,
// fast) and doltlite -> dolt (cross-format, requires real dolt). The
// known-gap tests (case 1, case 2) are t.Skip until the implementation
// handles them.

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// requireDoltliteOnly skips when the doltlite binary is missing.
// Tests that also need dolt as a target use requireDoltliteAndDolt.
func requireDoltliteOnly(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("doltlite"); err != nil {
		t.Skip("doltlite not installed")
	}
	return buildReplay(t)
}

func requireDoltliteAndDolt(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("doltlite"); err != nil {
		t.Skip("doltlite not installed")
	}
	if _, err := exec.LookPath("dolt"); err != nil {
		t.Skip("dolt not installed")
	}
	return buildReplay(t)
}

func buildReplay(t *testing.T) string {
	t.Helper()
	replayBin := filepath.Join(t.TempDir(), "dolt-replay")
	cmd := exec.Command("go", "build", "-o", replayBin, ".")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build dolt-replay: %v\n%s", err, out)
	}
	return replayBin
}

// dliteSQLcheck runs a SQL script against a doltlite db. Fails the
// test on non-zero exit; returns combined output.
func dliteSQLcheck(t *testing.T, db, sql string) string {
	t.Helper()
	cmd := exec.Command("doltlite", db, sql)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("doltlite %s\nSQL: %s\nERR: %v\nOUT: %s", db, sql, err, out)
	}
	return string(out)
}

// dliteCommitSep waits past the end of the current second before the
// next commit. doltlite's commit log walker sorts by date with
// second-resolution timestamps, so commits inside the same second can
// shuffle and break parent inference. Spread tests across seconds
// until that's fixed in main.go's doltliteLog.
func dliteCommitSep(t *testing.T) {
	t.Helper()
	time.Sleep(1100 * time.Millisecond)
}

// dliteRows returns rows of (table) at HEAD as pipe-separated strings.
func dliteRows(t *testing.T, db, table, orderByCol string) []string {
	t.Helper()
	q := fmt.Sprintf("SELECT * FROM \"%s\" ORDER BY \"%s\"", table, orderByCol)
	cmd := exec.Command("doltlite", db, q)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("doltlite read %s: %v\n%s", table, err, out)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return nil
	}
	return lines
}

// doltRows returns rows of (table) at HEAD from a dolt repo at dir,
// formatted as pipe-separated strings.
func doltRows(t *testing.T, dir, table, orderByCol string) []string {
	t.Helper()
	q := fmt.Sprintf("SELECT * FROM `%s` ORDER BY `%s`", table, orderByCol)
	cmd := exec.Command("dolt", "sql", "-r", "csv", "-q", q)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("dolt read %s: %v\n%s", table, err, out)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	// drop CSV header
	if len(lines) >= 1 {
		lines = lines[1:]
	}
	// Convert CSV commas to pipes for comparison parity with doltlite
	// output. Quotes around values are stripped on the simple cases
	// these tests cover (no embedded commas/quotes/newlines).
	for i, l := range lines {
		lines[i] = strings.ReplaceAll(l, ",", "|")
	}
	if len(lines) == 1 && lines[0] == "" {
		return nil
	}
	return lines
}

func runReplayDoltliteToDoltlite(t *testing.T, replayBin, src, dst, table string) (string, error) {
	t.Helper()
	cmd := exec.Command(replayBin,
		"--src-kind", "doltlite", "--src", src,
		"--dst-kind", "doltlite", "--dst", dst,
		"--table", table, "--limit", "1000")
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func runReplayDoltliteToDolt(t *testing.T, replayBin, src, dstDir, table string) (string, error) {
	t.Helper()
	cmd := exec.Command(replayBin,
		"--src-kind", "doltlite", "--src", src,
		"--dst-kind", "dolt", "--dst", dstDir,
		"--table", table, "--limit", "1000")
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func runReplayDoltToDoltlite(t *testing.T, replayBin, srcDir, dst, table string) (string, error) {
	t.Helper()
	cmd := exec.Command(replayBin,
		"--src-kind", "dolt", "--src", srcDir,
		"--dst-kind", "doltlite", "--dst", dst,
		"--table", table, "--limit", "1000")
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func runReplayDoltToDolt(t *testing.T, replayBin, srcDir, dstDir, table string) (string, error) {
	t.Helper()
	cmd := exec.Command(replayBin,
		"--src-kind", "dolt", "--src", srcDir,
		"--dst-kind", "dolt", "--dst", dstDir,
		"--table", table, "--limit", "1000")
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// doltSQLcheck runs SQL against a dolt repo. The SQL may contain
// dolt_commit calls.
func doltSQLcheck(t *testing.T, dir, sql string) string {
	t.Helper()
	cmd := exec.Command("dolt", "sql", "-q", sql)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("dolt sql in %s\nSQL: %s\nERR: %v\nOUT: %s", dir, sql, err, out)
	}
	return string(out)
}

func freshDoltliteSrc(t *testing.T, name string) string {
	t.Helper()
	dir := t.TempDir()
	db := filepath.Join(dir, name)
	dliteSQLcheck(t, db, "SELECT 1;")
	dliteCommitSep(t)
	return db
}

func freshDoltDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	cmd := exec.Command("dolt", "init", "--name", "test", "--email", "t@t.t")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("dolt init: %v\n%s", err, out)
	}
	return dir
}

// runBothDirections is the test body shared across cases. It builds
// the source via setupSrc, replays into both a fresh doltlite target
// and a fresh dolt target, and asserts each target's rows match `want`.
//
// orderByCol is used in the SELECT * FROM ... ORDER BY query against
// both targets so the row order is deterministic.
func runBothDirections(t *testing.T, table, orderByCol string, want []string,
	setupSrc func(t *testing.T, src string)) {
	t.Helper()

	// doltlite -> doltlite leg first; doesn't depend on dolt.
	t.Run("dlite_to_dlite", func(t *testing.T) {
		replayBin := requireDoltliteOnly(t)
		src := freshDoltliteSrc(t, "src.dl")
		setupSrc(t, src)

		dst := filepath.Join(filepath.Dir(src), "dst.dl")
		out, err := runReplayDoltliteToDoltlite(t, replayBin, src, dst, table)
		if err != nil {
			t.Fatalf("replay failed: %v\n%s", err, out)
		}
		got := dliteRows(t, dst, table, orderByCol)
		if !equalLines(want, got) {
			t.Errorf("doltlite target rows = %v\nwant = %v\nreplay output:\n%s",
				got, want, out)
		}
	})

	// doltlite -> dolt leg requires dolt installed too.
	t.Run("dlite_to_dolt", func(t *testing.T) {
		replayBin := requireDoltliteAndDolt(t)
		src := freshDoltliteSrc(t, "src.dl")
		setupSrc(t, src)

		dstDir := freshDoltDir(t)
		out, err := runReplayDoltliteToDolt(t, replayBin, src, dstDir, table)
		if err != nil {
			t.Fatalf("replay failed: %v\n%s", err, out)
		}
		got := doltRows(t, dstDir, table, orderByCol)
		if !equalLines(want, got) {
			t.Errorf("dolt target rows = %v\nwant = %v\nreplay output:\n%s",
				got, want, out)
		}
	})
}

// runBothDirectionsFromDolt mirrors runBothDirections for dolt-source
// tests. setupSrc is called with a freshly-initialized dolt repo and
// should run `dolt sql` / `dolt commit` to build history.
func runBothDirectionsFromDolt(t *testing.T, table, orderByCol string, want []string,
	setupSrc func(t *testing.T, srcDir string)) {
	t.Helper()

	t.Run("dolt_to_dlite", func(t *testing.T) {
		replayBin := requireDoltliteAndDolt(t)
		srcDir := freshDoltDir(t)
		setupSrc(t, srcDir)

		dst := filepath.Join(t.TempDir(), "dst.dl")
		out, err := runReplayDoltToDoltlite(t, replayBin, srcDir, dst, table)
		if err != nil {
			t.Fatalf("replay failed: %v\n%s", err, out)
		}
		got := dliteRows(t, dst, table, orderByCol)
		if !equalLines(want, got) {
			t.Errorf("doltlite target rows = %v\nwant = %v\nreplay output:\n%s",
				got, want, out)
		}
	})

	t.Run("dolt_to_dolt", func(t *testing.T) {
		replayBin := requireDoltliteAndDolt(t)
		srcDir := freshDoltDir(t)
		setupSrc(t, srcDir)

		dstDir := freshDoltDir(t)
		out, err := runReplayDoltToDolt(t, replayBin, srcDir, dstDir, table)
		if err != nil {
			t.Fatalf("replay failed: %v\n%s", err, out)
		}
		got := doltRows(t, dstDir, table, orderByCol)
		if !equalLines(want, got) {
			t.Errorf("dolt target rows = %v\nwant = %v\nreplay output:\n%s",
				got, want, out)
		}
	})
}

// ---------------- tests ----------------

// TestReplaySchema_Simple: schema-unchanged history replays cleanly in
// both directions. Baseline that confirms the doltlite-source path
// still works for the no-schema-change case after the schema-replay
// changes landed.
func TestReplaySchema_Simple(t *testing.T) {
	runBothDirections(t, "t", "id", []string{"2|b", "3|c", "4|d"},
		func(t *testing.T, src string) {
			dliteSQLcheck(t, src, `CREATE TABLE t(id INTEGER PRIMARY KEY, name TEXT);
INSERT INTO t VALUES (1,'a'),(2,'b'),(3,'c');
SELECT dolt_commit('-Am','seed');`)
			dliteCommitSep(t)
			dliteSQLcheck(t, src, `INSERT INTO t VALUES (4,'d');
DELETE FROM t WHERE id=1;
SELECT dolt_commit('-Am','add+remove');`)
		})
}

// TestReplaySchema_DoltSrc_Simple: dolt-source baseline with no schema
// changes. Confirms the dolt-source path handles the no-schema-change
// case before we tackle the upstream #10988 silent-skip on schema-change
// commits from the dolt side.
func TestReplaySchema_DoltSrc_Simple(t *testing.T) {
	runBothDirectionsFromDolt(t, "t", "id", []string{"2|b", "3|c", "4|d"},
		func(t *testing.T, srcDir string) {
			doltSQLcheck(t, srcDir, `CREATE TABLE t(id INTEGER PRIMARY KEY, name TEXT);
INSERT INTO t VALUES (1,'a'),(2,'b'),(3,'c');
CALL DOLT_COMMIT('-Am','seed');`)
			doltSQLcheck(t, srcDir, `INSERT INTO t VALUES (4,'d');
DELETE FROM t WHERE id=1;
CALL DOLT_COMMIT('-Am','add+remove');`)
		})
}

// TestReplaySchema_DoltSrc_AddNullable: dolt-source ADD COLUMN (no
// default) + data changes in the same commit. From the dolt side this
// hits the upstream #10988 bug — `dolt diff -r sql` emits the ALTER
// but silently drops the data DML, so the target ends up with the new
// schema but missing the row updates. Documents the source-side gap.
func TestReplaySchema_DoltSrc_AddNullable(t *testing.T) {
	runBothDirectionsFromDolt(t, "t", "id", []string{"1|a|", "2|b|", "6|f|", "7|g|"},
		func(t *testing.T, srcDir string) {
			doltSQLcheck(t, srcDir, `CREATE TABLE t(id INTEGER PRIMARY KEY, name TEXT);
INSERT INTO t VALUES (1,'a'),(2,'b'),(3,'c'),(4,'d'),(5,'e');
CALL DOLT_COMMIT('-Am','seed');`)
			doltSQLcheck(t, srcDir, `ALTER TABLE t ADD COLUMN extra TEXT;
DELETE FROM t WHERE id IN (3,4,5);
INSERT INTO t VALUES (6,'f',NULL),(7,'g',NULL);
CALL DOLT_COMMIT('-Am','combined');`)
		})
}

// TestReplaySchema_AddNullable: ADD COLUMN with no default plus data
// changes in the same commit — the bahaiwritings shape, the case the
// prototype was specifically built for.
func TestReplaySchema_AddNullable(t *testing.T) {
	runBothDirections(t, "t", "id", []string{"1|a|", "2|b|", "6|f|", "7|g|"},
		func(t *testing.T, src string) {
			dliteSQLcheck(t, src, `CREATE TABLE t(id INTEGER PRIMARY KEY, name TEXT);
INSERT INTO t VALUES (1,'a'),(2,'b'),(3,'c'),(4,'d'),(5,'e');
SELECT dolt_commit('-Am','seed');`)
			dliteCommitSep(t)
			dliteSQLcheck(t, src, `ALTER TABLE t ADD COLUMN extra TEXT;
DELETE FROM t WHERE id IN (3,4,5);
INSERT INTO t VALUES (6,'f',NULL),(7,'g',NULL);
SELECT dolt_commit('-Am','combined');`)
		})
}

// TestReplaySchema_AddWithDefault: ADD COLUMN with DEFAULT that should
// populate existing rows. dolt_diff_<table> doesn't surface a row for
// the schema-change commit's effect (nicktobey case 1) — the ALTER's
// default-population isn't visible at the data-diff layer.
func TestReplaySchema_AddWithDefault(t *testing.T) {
	runBothDirections(t, "t", "pk", []string{"0|6", "1|6", "2|6"},
		func(t *testing.T, src string) {
			dliteSQLcheck(t, src, `CREATE TABLE t(pk INTEGER PRIMARY KEY);
INSERT INTO t VALUES (0),(1),(2);
SELECT dolt_commit('-Am','seed');`)
			dliteCommitSep(t)
			dliteSQLcheck(t, src, `ALTER TABLE t ADD COLUMN c INTEGER DEFAULT 6;
SELECT dolt_commit('-Am','add column with default');`)
		})
}

// TestReplaySchema_DropThenAdd: DROP COLUMN + ADD COLUMN where the
// post-update value happens to match what the dropped column had.
// Positional row-record aliasing hides the change from dolt_diff_<table>
// (nicktobey case 2). The data emitter has no way to recover the
// semantic UPDATE that should have replayed.
func TestReplaySchema_DropThenAdd(t *testing.T) {
	runBothDirections(t, "t", "pk", []string{"0|10"},
		func(t *testing.T, src string) {
			dliteSQLcheck(t, src, `CREATE TABLE t(pk INTEGER PRIMARY KEY, a INTEGER);
INSERT INTO t VALUES (0, 10);
SELECT dolt_commit('-Am','seed');`)
			dliteCommitSep(t)
			dliteSQLcheck(t, src, `ALTER TABLE t DROP COLUMN a;
ALTER TABLE t ADD COLUMN b INTEGER;
UPDATE t SET b=10;
SELECT dolt_commit('-Am','drop and add');`)
		})
}

// TestReplaySchema_RenameColumn: ALTER TABLE RENAME COLUMN. Tests
// whether the rename surfaces correctly through the diff layer and
// applies on the target.
func TestReplaySchema_RenameColumn(t *testing.T) {
	runBothDirections(t, "t", "id", []string{"1|alpha", "2|beta"},
		func(t *testing.T, src string) {
			dliteSQLcheck(t, src, `CREATE TABLE t(id INTEGER PRIMARY KEY, name TEXT);
INSERT INTO t VALUES (1,'alpha'),(2,'beta');
SELECT dolt_commit('-Am','seed');`)
			dliteCommitSep(t)
			dliteSQLcheck(t, src, `ALTER TABLE t RENAME COLUMN name TO label;
SELECT dolt_commit('-Am','rename');`)
		})
}

// TestReplaySchema_RowOrderingPreserved: insert rows in non-PK-sorted
// order across multiple commits, verify the target's PK-ordered read
// matches the source's. Catches any sneaky reordering or row aliasing.
func TestReplaySchema_RowOrderingPreserved(t *testing.T) {
	runBothDirections(t, "t", "id",
		[]string{"1|alpha", "2|beta", "3|gamma", "4|delta", "5|epsilon"},
		func(t *testing.T, src string) {
			dliteSQLcheck(t, src, `CREATE TABLE t(id INTEGER PRIMARY KEY, name TEXT);
INSERT INTO t VALUES (3,'gamma'),(1,'alpha'),(5,'epsilon');
SELECT dolt_commit('-Am','seed out-of-order');`)
			dliteCommitSep(t)
			dliteSQLcheck(t, src, `INSERT INTO t VALUES (4,'delta'),(2,'beta');
SELECT dolt_commit('-Am','add interior rows');`)
		})
}

// TestReplaySchema_CreateTableMidHistory: a table introduced not in the
// initial seed but in a later commit. Tests that the schema-and-data
// emitter handles tables whose first appearance is partway through.
func TestReplaySchema_CreateTableMidHistory(t *testing.T) {
	setup := func(t *testing.T, src string) {
		dliteSQLcheck(t, src, `CREATE TABLE a(id INTEGER PRIMARY KEY, name TEXT);
INSERT INTO a VALUES (1,'x');
SELECT dolt_commit('-Am','seed a');`)
		dliteCommitSep(t)
		dliteSQLcheck(t, src, `CREATE TABLE b(id INTEGER PRIMARY KEY, val INTEGER);
INSERT INTO b VALUES (10, 100);
SELECT dolt_commit('-Am','add b');`)
		dliteCommitSep(t)
		dliteSQLcheck(t, src, `INSERT INTO a VALUES (2,'y');
INSERT INTO b VALUES (20, 200);
SELECT dolt_commit('-Am','data both');`)
	}

	check := func(t *testing.T, readA, readB func() []string) {
		gotA := readA()
		wantA := []string{"1|x", "2|y"}
		if !equalLines(wantA, gotA) {
			t.Errorf("table a rows = %v, want %v", gotA, wantA)
		}
		gotB := readB()
		wantB := []string{"10|100", "20|200"}
		if !equalLines(wantB, gotB) {
			t.Errorf("table b rows = %v, want %v", gotB, wantB)
		}
	}

	t.Run("dlite_to_dlite", func(t *testing.T) {
		replayBin := requireDoltliteOnly(t)
		src := freshDoltliteSrc(t, "src.dl")
		setup(t, src)

		dst := filepath.Join(filepath.Dir(src), "dst.dl")
		out, err := runReplayDoltliteToDoltlite(t, replayBin, src, dst, "")
		if err != nil {
			t.Fatalf("replay failed: %v\n%s", err, out)
		}
		check(t,
			func() []string { return dliteRows(t, dst, "a", "id") },
			func() []string { return dliteRows(t, dst, "b", "id") },
		)
	})

	t.Run("dlite_to_dolt", func(t *testing.T) {
		replayBin := requireDoltliteAndDolt(t)
		src := freshDoltliteSrc(t, "src.dl")
		setup(t, src)

		dstDir := freshDoltDir(t)
		out, err := runReplayDoltliteToDolt(t, replayBin, src, dstDir, "")
		if err != nil {
			t.Fatalf("replay failed: %v\n%s", err, out)
		}
		check(t,
			func() []string { return doltRows(t, dstDir, "a", "id") },
			func() []string { return doltRows(t, dstDir, "b", "id") },
		)
	})
}

// TestReplaySchema_MultiTable: two tables change in the same history.
// Replay with --table="" (all tables) and verify both end states.
func TestReplaySchema_MultiTable(t *testing.T) {
	setup := func(t *testing.T, src string) {
		dliteSQLcheck(t, src, `CREATE TABLE a(id INTEGER PRIMARY KEY, name TEXT);
CREATE TABLE b(id INTEGER PRIMARY KEY, val INTEGER);
INSERT INTO a VALUES (1,'x'),(2,'y');
INSERT INTO b VALUES (10, 100),(20, 200);
SELECT dolt_commit('-Am','seed both');`)
		dliteCommitSep(t)
		dliteSQLcheck(t, src, `INSERT INTO a VALUES (3,'z');
UPDATE b SET val=999 WHERE id=10;
SELECT dolt_commit('-Am','update both');`)
	}

	check := func(t *testing.T, readA, readB func() []string) {
		gotA := readA()
		wantA := []string{"1|x", "2|y", "3|z"}
		if !equalLines(wantA, gotA) {
			t.Errorf("table a rows = %v, want %v", gotA, wantA)
		}
		gotB := readB()
		wantB := []string{"10|999", "20|200"}
		if !equalLines(wantB, gotB) {
			t.Errorf("table b rows = %v, want %v", gotB, wantB)
		}
	}

	t.Run("dlite_to_dlite", func(t *testing.T) {
		replayBin := requireDoltliteOnly(t)
		src := freshDoltliteSrc(t, "src.dl")
		setup(t, src)

		dst := filepath.Join(filepath.Dir(src), "dst.dl")
		out, err := runReplayDoltliteToDoltlite(t, replayBin, src, dst, "")
		if err != nil {
			t.Fatalf("replay failed: %v\n%s", err, out)
		}
		check(t,
			func() []string { return dliteRows(t, dst, "a", "id") },
			func() []string { return dliteRows(t, dst, "b", "id") },
		)
	})

	t.Run("dlite_to_dolt", func(t *testing.T) {
		replayBin := requireDoltliteAndDolt(t)
		src := freshDoltliteSrc(t, "src.dl")
		setup(t, src)

		dstDir := freshDoltDir(t)
		out, err := runReplayDoltliteToDolt(t, replayBin, src, dstDir, "")
		if err != nil {
			t.Fatalf("replay failed: %v\n%s", err, out)
		}
		check(t,
			func() []string { return doltRows(t, dstDir, "a", "id") },
			func() []string { return doltRows(t, dstDir, "b", "id") },
		)
	})
}

// TestReplaySchema_DropTable: DROP TABLE on a table that previously
// had data. Target should end up without the table (or with it gone
// after the second commit applies). Single-table replay (--table t)
// means the replay is scoped — what does dropping the watched table
// actually do?
func TestReplaySchema_DropTable(t *testing.T) {
	setup := func(t *testing.T, src string) {
		dliteSQLcheck(t, src, `CREATE TABLE t(id INTEGER PRIMARY KEY, name TEXT);
INSERT INTO t VALUES (1,'a'),(2,'b');
SELECT dolt_commit('-Am','seed');`)
		dliteCommitSep(t)
		dliteSQLcheck(t, src, `DROP TABLE t;
SELECT dolt_commit('-Am','drop table');`)
	}

	t.Run("dlite_to_dlite", func(t *testing.T) {
		replayBin := requireDoltliteOnly(t)
		src := freshDoltliteSrc(t, "src.dl")
		setup(t, src)

		dst := filepath.Join(filepath.Dir(src), "dst.dl")
		out, err := runReplayDoltliteToDoltlite(t, replayBin, src, dst, "t")
		if err != nil {
			t.Fatalf("replay failed: %v\n%s", err, out)
		}
		cmd := exec.Command("doltlite", "-csv", "-header", dst,
			"SELECT name FROM sqlite_master WHERE type='table' AND name='t'")
		chk, _ := cmd.CombinedOutput()
		gotLines := strings.Split(strings.TrimSpace(string(chk)), "\n")
		if len(gotLines) > 1 {
			t.Errorf("table 't' still exists on doltlite target\nintrospect: %s\nreplay output:\n%s",
				chk, out)
		}
	})

	t.Run("dlite_to_dolt", func(t *testing.T) {
		replayBin := requireDoltliteAndDolt(t)
		src := freshDoltliteSrc(t, "src.dl")
		setup(t, src)

		dstDir := freshDoltDir(t)
		out, err := runReplayDoltliteToDolt(t, replayBin, src, dstDir, "t")
		if err != nil {
			t.Fatalf("replay failed: %v\n%s", err, out)
		}
		cmd := exec.Command("dolt", "sql", "-r", "csv", "-q",
			"SELECT table_name FROM information_schema.tables WHERE table_name='t' AND table_schema=database()")
		cmd.Dir = dstDir
		chk, _ := cmd.CombinedOutput()
		gotLines := strings.Split(strings.TrimSpace(string(chk)), "\n")
		if len(gotLines) > 1 {
			t.Errorf("table 't' still exists on dolt target\nintrospect: %s\nreplay output:\n%s",
				chk, out)
		}
	})
}

// TestReplaySchema_TypeWidening: ALTER COLUMN to widen a type
// (e.g. INTEGER → BIGINT, VARCHAR(10) → VARCHAR(50)). Pure type change,
// no value loss expected. Tests whether deriveAlterFromCreate detects
// type changes and emits something usable.
func TestReplaySchema_TypeWidening(t *testing.T) {
	// SQLite doesn't support MODIFY COLUMN; emulate the type change via
	// the standard table-rebuild pattern (new table + INSERT SELECT +
	// DROP + RENAME). From the replay's view, this is a same-name same-
	// position type change — deriveAlterFromCreate emits a schema-comment
	// rather than an ALTER, but data values still flow through.
	runBothDirections(t, "t", "id", []string{"1|hello", "2|world"},
		func(t *testing.T, src string) {
			dliteSQLcheck(t, src, `CREATE TABLE t(id INTEGER PRIMARY KEY, name VARCHAR(10));
INSERT INTO t VALUES (1,'hello'),(2,'world');
SELECT dolt_commit('-Am','seed');`)
			dliteCommitSep(t)
			dliteSQLcheck(t, src, `CREATE TABLE t_new(id INTEGER PRIMARY KEY, name VARCHAR(50));
INSERT INTO t_new SELECT * FROM t;
DROP TABLE t;
ALTER TABLE t_new RENAME TO t;
SELECT dolt_commit('-Am','widen');`)
		})
}

// TestReplaySchema_TypeNarrowing: dlite-source narrow. Same SQLite
// parser limitation as TypeWidening — MODIFY COLUMN isn't accepted.
func TestReplaySchema_TypeNarrowing(t *testing.T) {
	runBothDirections(t, "t", "id", []string{"1|hi", "2|world"},
		func(t *testing.T, src string) {
			dliteSQLcheck(t, src, `CREATE TABLE t(id INTEGER PRIMARY KEY, name VARCHAR(50));
INSERT INTO t VALUES (1,'hi'),(2,'world');
SELECT dolt_commit('-Am','seed');`)
			dliteCommitSep(t)
			dliteSQLcheck(t, src, `CREATE TABLE t_new(id INTEGER PRIMARY KEY, name VARCHAR(10));
INSERT INTO t_new SELECT * FROM t;
DROP TABLE t;
ALTER TABLE t_new RENAME TO t;
SELECT dolt_commit('-Am','narrow');`)
		})
}

// TestReplaySchema_DropOnly: DROP COLUMN with no add. Existing rows
// keep their PK + remaining columns; the dropped column simply goes
// away on the target.
func TestReplaySchema_DropOnly(t *testing.T) {
	runBothDirections(t, "t", "id", []string{"1|a", "2|b"},
		func(t *testing.T, src string) {
			dliteSQLcheck(t, src, `CREATE TABLE t(id INTEGER PRIMARY KEY, name TEXT, extra TEXT);
INSERT INTO t VALUES (1,'a','x'),(2,'b','y');
SELECT dolt_commit('-Am','seed');`)
			dliteCommitSep(t)
			dliteSQLcheck(t, src, `ALTER TABLE t DROP COLUMN extra;
SELECT dolt_commit('-Am','drop column');`)
		})
}

// TestReplaySchema_DoltSrc_AddWithDefault: dolt-source side of nicktobey
// case 1. Compounds two failure modes:
//   - upstream #10988 silent-skip on schema-change commits (source-side)
//   - dolt_diff_<table> not surfacing ALTER's default-population (algorithm)
// Skipped pending the schema-then-diff-against-rebased-baseline fix.
func TestReplaySchema_DoltSrc_AddWithDefault(t *testing.T) {
	runBothDirectionsFromDolt(t, "t", "pk", []string{"0|6", "1|6", "2|6"},
		func(t *testing.T, srcDir string) {
			doltSQLcheck(t, srcDir, `CREATE TABLE t(pk INTEGER PRIMARY KEY);
INSERT INTO t VALUES (0),(1),(2);
CALL DOLT_COMMIT('-Am','seed');`)
			doltSQLcheck(t, srcDir, `ALTER TABLE t ADD COLUMN c INTEGER DEFAULT 6;
CALL DOLT_COMMIT('-Am','add column with default');`)
		})
}

// TestReplaySchema_DoltSrc_DropThenAdd: dolt-source side of nicktobey
// case 2. Same cascade — upstream silent-skip plus positional row-record
// aliasing. Skipped pending the algorithm fix.
func TestReplaySchema_DoltSrc_DropThenAdd(t *testing.T) {
	runBothDirectionsFromDolt(t, "t", "pk", []string{"0|10"},
		func(t *testing.T, srcDir string) {
			doltSQLcheck(t, srcDir, `CREATE TABLE t(pk INTEGER PRIMARY KEY, a INTEGER);
INSERT INTO t VALUES (0, 10);
CALL DOLT_COMMIT('-Am','seed');`)
			doltSQLcheck(t, srcDir, `ALTER TABLE t DROP COLUMN a;
ALTER TABLE t ADD COLUMN b INTEGER;
UPDATE t SET b=10;
CALL DOLT_COMMIT('-Am','drop and add');`)
		})
}

// TestReplaySchema_DoltSrc_TypeNarrowing: dolt-source MODIFY COLUMN to
// narrow a type that nonetheless fits all current values (no overflow).
// Should preserve values; the narrower type is just less permissive.
func TestReplaySchema_DoltSrc_TypeNarrowing(t *testing.T) {
	setup := func(t *testing.T, srcDir string) {
		doltSQLcheck(t, srcDir, `CREATE TABLE t(id INTEGER PRIMARY KEY, name VARCHAR(50));
INSERT INTO t VALUES (1,'hi'),(2,'world');
CALL DOLT_COMMIT('-Am','seed');`)
		doltSQLcheck(t, srcDir, `ALTER TABLE t MODIFY COLUMN name VARCHAR(10);
CALL DOLT_COMMIT('-Am','narrow');`)
	}

	t.Run("dolt_to_dlite", func(t *testing.T) {
		replayBin := requireDoltliteAndDolt(t)
		srcDir := freshDoltDir(t)
		setup(t, srcDir)

		dst := filepath.Join(t.TempDir(), "dst.dl")
		out, err := runReplayDoltToDoltlite(t, replayBin, srcDir, dst, "t")
		if err != nil {
			t.Fatalf("replay failed: %v\n%s", err, out)
		}
		got := dliteRows(t, dst, "t", "id")
		if !equalLines([]string{"1|hi", "2|world"}, got) {
			t.Errorf("rows = %v\nreplay:\n%s", got, out)
		}
	})

	t.Run("dolt_to_dolt", func(t *testing.T) {
		replayBin := requireDoltliteAndDolt(t)
		srcDir := freshDoltDir(t)
		setup(t, srcDir)

		dstDir := freshDoltDir(t)
		out, err := runReplayDoltToDolt(t, replayBin, srcDir, dstDir, "t")
		if err != nil {
			t.Fatalf("replay failed: %v\n%s", err, out)
		}
		got := doltRows(t, dstDir, "t", "id")
		if !equalLines([]string{"1|hi", "2|world"}, got) {
			t.Errorf("rows = %v\nreplay:\n%s", got, out)
		}
	})
}

// TestReplaySchema_DoltSrc_RenameColumn: dolt-source ALTER RENAME
// COLUMN. `dolt diff -r sql` natively emits RENAME COLUMN, so the
// dolt→dolt direction should work; dolt→dlite hinges on whether the
// dialect translation passes the rename through and doltlite/SQLite
// accepts it.
func TestReplaySchema_DoltSrc_RenameColumn(t *testing.T) {
	setup := func(t *testing.T, srcDir string) {
		doltSQLcheck(t, srcDir, `CREATE TABLE t(id INTEGER PRIMARY KEY, name TEXT);
INSERT INTO t VALUES (1,'alpha'),(2,'beta');
CALL DOLT_COMMIT('-Am','seed');`)
		doltSQLcheck(t, srcDir, `ALTER TABLE t RENAME COLUMN name TO label;
CALL DOLT_COMMIT('-Am','rename');`)
	}

	t.Run("dolt_to_dlite", func(t *testing.T) {
		replayBin := requireDoltliteAndDolt(t)
		srcDir := freshDoltDir(t)
		setup(t, srcDir)

		dst := filepath.Join(t.TempDir(), "dst.dl")
		out, err := runReplayDoltToDoltlite(t, replayBin, srcDir, dst, "t")
		if err != nil {
			t.Fatalf("replay failed: %v\n%s", err, out)
		}
		got := dliteRows(t, dst, "t", "id")
		if !equalLines([]string{"1|alpha", "2|beta"}, got) {
			t.Errorf("rows = %v\nreplay:\n%s", got, out)
		}
	})

	t.Run("dolt_to_dolt", func(t *testing.T) {
		replayBin := requireDoltliteAndDolt(t)
		srcDir := freshDoltDir(t)
		setup(t, srcDir)

		dstDir := freshDoltDir(t)
		out, err := runReplayDoltToDolt(t, replayBin, srcDir, dstDir, "t")
		if err != nil {
			t.Fatalf("replay failed: %v\n%s", err, out)
		}
		got := doltRows(t, dstDir, "t", "id")
		if !equalLines([]string{"1|alpha", "2|beta"}, got) {
			t.Errorf("rows = %v\nreplay:\n%s", got, out)
		}
	})
}

// TestReplaySchema_DoltSrc_TypeWidening: dolt-source MODIFY COLUMN to
// widen the type. Dolt supports MODIFY COLUMN natively, so the source
// commit can be set up. The replay then either preserves the widened
// type, or — if dialect translation strips type info — the doltlite
// target ends up with the values intact (TEXT-coerced) and the dolt
// target with the widened type.
func TestReplaySchema_DoltSrc_TypeWidening(t *testing.T) {
	setup := func(t *testing.T, srcDir string) {
		doltSQLcheck(t, srcDir, `CREATE TABLE t(id INTEGER PRIMARY KEY, name VARCHAR(10));
INSERT INTO t VALUES (1,'hello'),(2,'world');
CALL DOLT_COMMIT('-Am','seed');`)
		doltSQLcheck(t, srcDir, `ALTER TABLE t MODIFY COLUMN name VARCHAR(50);
CALL DOLT_COMMIT('-Am','widen');`)
	}

	t.Run("dolt_to_dlite", func(t *testing.T) {
		replayBin := requireDoltliteAndDolt(t)
		srcDir := freshDoltDir(t)
		setup(t, srcDir)

		dst := filepath.Join(t.TempDir(), "dst.dl")
		out, err := runReplayDoltToDoltlite(t, replayBin, srcDir, dst, "t")
		if err != nil {
			t.Fatalf("replay failed: %v\n%s", err, out)
		}
		got := dliteRows(t, dst, "t", "id")
		if !equalLines([]string{"1|hello", "2|world"}, got) {
			t.Errorf("rows = %v\nreplay:\n%s", got, out)
		}
	})

	t.Run("dolt_to_dolt", func(t *testing.T) {
		replayBin := requireDoltliteAndDolt(t)
		srcDir := freshDoltDir(t)
		setup(t, srcDir)

		dstDir := freshDoltDir(t)
		out, err := runReplayDoltToDolt(t, replayBin, srcDir, dstDir, "t")
		if err != nil {
			t.Fatalf("replay failed: %v\n%s", err, out)
		}
		got := doltRows(t, dstDir, "t", "id")
		if !equalLines([]string{"1|hello", "2|world"}, got) {
			t.Errorf("rows = %v\nreplay:\n%s", got, out)
		}
	})
}

// TestReplaySchema_DoltSrc_DropTable: dolt-source DROP TABLE on a
// previously-populated table. Pure-schema in the drop commit; no
// upstream silent-skip hit.
func TestReplaySchema_DoltSrc_DropTable(t *testing.T) {
	setup := func(t *testing.T, srcDir string) {
		doltSQLcheck(t, srcDir, `CREATE TABLE t(id INTEGER PRIMARY KEY, name TEXT);
INSERT INTO t VALUES (1,'a'),(2,'b');
CALL DOLT_COMMIT('-Am','seed');`)
		doltSQLcheck(t, srcDir, `DROP TABLE t;
CALL DOLT_COMMIT('-Am','drop table');`)
	}

	t.Run("dolt_to_dlite", func(t *testing.T) {
		replayBin := requireDoltliteAndDolt(t)
		srcDir := freshDoltDir(t)
		setup(t, srcDir)

		dst := filepath.Join(t.TempDir(), "dst.dl")
		out, err := runReplayDoltToDoltlite(t, replayBin, srcDir, dst, "t")
		if err != nil {
			t.Fatalf("replay failed: %v\n%s", err, out)
		}
		cmd := exec.Command("doltlite", "-csv", "-header", dst,
			"SELECT name FROM sqlite_master WHERE type='table' AND name='t'")
		chk, _ := cmd.CombinedOutput()
		gotLines := strings.Split(strings.TrimSpace(string(chk)), "\n")
		if len(gotLines) > 1 {
			t.Errorf("table 't' still exists on doltlite target\nintrospect: %s\nreplay output:\n%s",
				chk, out)
		}
	})

	t.Run("dolt_to_dolt", func(t *testing.T) {
		replayBin := requireDoltliteAndDolt(t)
		srcDir := freshDoltDir(t)
		setup(t, srcDir)

		dstDir := freshDoltDir(t)
		out, err := runReplayDoltToDolt(t, replayBin, srcDir, dstDir, "t")
		if err != nil {
			t.Fatalf("replay failed: %v\n%s", err, out)
		}
		cmd := exec.Command("dolt", "sql", "-r", "csv", "-q",
			"SELECT table_name FROM information_schema.tables WHERE table_name='t' AND table_schema=database()")
		cmd.Dir = dstDir
		chk, _ := cmd.CombinedOutput()
		gotLines := strings.Split(strings.TrimSpace(string(chk)), "\n")
		if len(gotLines) > 1 {
			t.Errorf("table 't' still exists on dolt target\nintrospect: %s\nreplay output:\n%s",
				chk, out)
		}
	})
}

// TestReplaySchema_DoltSrc_MultiTable: dolt-source two tables changing
// in the same history.
func TestReplaySchema_DoltSrc_MultiTable(t *testing.T) {
	setup := func(t *testing.T, srcDir string) {
		doltSQLcheck(t, srcDir, `CREATE TABLE a(id INTEGER PRIMARY KEY, name TEXT);
CREATE TABLE b(id INTEGER PRIMARY KEY, val INTEGER);
INSERT INTO a VALUES (1,'x'),(2,'y');
INSERT INTO b VALUES (10, 100),(20, 200);
CALL DOLT_COMMIT('-Am','seed both');`)
		doltSQLcheck(t, srcDir, `INSERT INTO a VALUES (3,'z');
UPDATE b SET val=999 WHERE id=10;
CALL DOLT_COMMIT('-Am','update both');`)
	}

	t.Run("dolt_to_dlite", func(t *testing.T) {
		replayBin := requireDoltliteAndDolt(t)
		srcDir := freshDoltDir(t)
		setup(t, srcDir)

		dst := filepath.Join(t.TempDir(), "dst.dl")
		out, err := runReplayDoltToDoltlite(t, replayBin, srcDir, dst, "")
		if err != nil {
			t.Fatalf("replay failed: %v\n%s", err, out)
		}
		gotA := dliteRows(t, dst, "a", "id")
		if !equalLines([]string{"1|x", "2|y", "3|z"}, gotA) {
			t.Errorf("a = %v", gotA)
		}
		gotB := dliteRows(t, dst, "b", "id")
		if !equalLines([]string{"10|999", "20|200"}, gotB) {
			t.Errorf("b = %v\nreplay:\n%s", gotB, out)
		}
	})

	t.Run("dolt_to_dolt", func(t *testing.T) {
		replayBin := requireDoltliteAndDolt(t)
		srcDir := freshDoltDir(t)
		setup(t, srcDir)

		dstDir := freshDoltDir(t)
		out, err := runReplayDoltToDolt(t, replayBin, srcDir, dstDir, "")
		if err != nil {
			t.Fatalf("replay failed: %v\n%s", err, out)
		}
		gotA := doltRows(t, dstDir, "a", "id")
		if !equalLines([]string{"1|x", "2|y", "3|z"}, gotA) {
			t.Errorf("a = %v", gotA)
		}
		gotB := doltRows(t, dstDir, "b", "id")
		if !equalLines([]string{"10|999", "20|200"}, gotB) {
			t.Errorf("b = %v\nreplay:\n%s", gotB, out)
		}
	})
}

// TestReplaySchema_DoltSrc_CreateTableMidHistory: dolt-source second
// table introduced partway through history.
func TestReplaySchema_DoltSrc_CreateTableMidHistory(t *testing.T) {
	setup := func(t *testing.T, srcDir string) {
		doltSQLcheck(t, srcDir, `CREATE TABLE a(id INTEGER PRIMARY KEY, name TEXT);
INSERT INTO a VALUES (1,'x');
CALL DOLT_COMMIT('-Am','seed a');`)
		doltSQLcheck(t, srcDir, `CREATE TABLE b(id INTEGER PRIMARY KEY, val INTEGER);
INSERT INTO b VALUES (10, 100);
CALL DOLT_COMMIT('-Am','add b');`)
		doltSQLcheck(t, srcDir, `INSERT INTO a VALUES (2,'y');
INSERT INTO b VALUES (20, 200);
CALL DOLT_COMMIT('-Am','data both');`)
	}

	t.Run("dolt_to_dlite", func(t *testing.T) {
		replayBin := requireDoltliteAndDolt(t)
		srcDir := freshDoltDir(t)
		setup(t, srcDir)

		dst := filepath.Join(t.TempDir(), "dst.dl")
		out, err := runReplayDoltToDoltlite(t, replayBin, srcDir, dst, "")
		if err != nil {
			t.Fatalf("replay failed: %v\n%s", err, out)
		}
		gotA := dliteRows(t, dst, "a", "id")
		if !equalLines([]string{"1|x", "2|y"}, gotA) {
			t.Errorf("a = %v\nreplay:\n%s", gotA, out)
		}
		gotB := dliteRows(t, dst, "b", "id")
		if !equalLines([]string{"10|100", "20|200"}, gotB) {
			t.Errorf("b = %v\nreplay:\n%s", gotB, out)
		}
	})

	t.Run("dolt_to_dolt", func(t *testing.T) {
		replayBin := requireDoltliteAndDolt(t)
		srcDir := freshDoltDir(t)
		setup(t, srcDir)

		dstDir := freshDoltDir(t)
		out, err := runReplayDoltToDolt(t, replayBin, srcDir, dstDir, "")
		if err != nil {
			t.Fatalf("replay failed: %v\n%s", err, out)
		}
		gotA := doltRows(t, dstDir, "a", "id")
		if !equalLines([]string{"1|x", "2|y"}, gotA) {
			t.Errorf("a = %v", gotA)
		}
		gotB := doltRows(t, dstDir, "b", "id")
		if !equalLines([]string{"10|100", "20|200"}, gotB) {
			t.Errorf("b = %v", gotB)
		}
	})
}

// TestReplaySchema_DoltSrc_RowOrderingPreserved: dolt-source non-PK-
// sorted insertion order across commits.
func TestReplaySchema_DoltSrc_RowOrderingPreserved(t *testing.T) {
	runBothDirectionsFromDolt(t, "t", "id",
		[]string{"1|alpha", "2|beta", "3|gamma", "4|delta", "5|epsilon"},
		func(t *testing.T, srcDir string) {
			doltSQLcheck(t, srcDir, `CREATE TABLE t(id INTEGER PRIMARY KEY, name TEXT);
INSERT INTO t VALUES (3,'gamma'),(1,'alpha'),(5,'epsilon');
CALL DOLT_COMMIT('-Am','seed out-of-order');`)
			doltSQLcheck(t, srcDir, `INSERT INTO t VALUES (4,'delta'),(2,'beta');
CALL DOLT_COMMIT('-Am','add interior rows');`)
		})
}

// TestReplaySchema_DoltSrc_DropOnly: dolt-source DROP COLUMN with no
// accompanying data changes. Pure-schema commits should be safe even
// under the upstream silent-skip bug since there's no data DML to drop.
func TestReplaySchema_DoltSrc_DropOnly(t *testing.T) {
	runBothDirectionsFromDolt(t, "t", "id", []string{"1|a", "2|b"},
		func(t *testing.T, srcDir string) {
			doltSQLcheck(t, srcDir, `CREATE TABLE t(id INTEGER PRIMARY KEY, name TEXT, extra TEXT);
INSERT INTO t VALUES (1,'a','x'),(2,'b','y');
CALL DOLT_COMMIT('-Am','seed');`)
			doltSQLcheck(t, srcDir, `ALTER TABLE t DROP COLUMN extra;
CALL DOLT_COMMIT('-Am','drop column');`)
		})
}

func equalLines(a, b []string) bool {
	if len(a) == 0 && len(b) == 0 {
		return true
	}
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
