package main

// Benchmarks for doltlite per-row UPDATE workloads — the dominant cost when
// replaying `dolt diff -r sql` output (one UPDATE statement per changed PK,
// each in its own implicit savepoint => mutmap flushes with M=1).
//
// Run only when DOLTLITE_BENCH=1 is set (otherwise b.Skip), so normal
// `go test ./...` stays fast and offline-friendly.
//
// Override the binary under test with DLITE=/path/to/doltlite. Override row
// count with DOLTLITE_BENCH_ROWS=N (default 2000).
//
// Typical use:
//   DOLTLITE_BENCH=1 DLITE=/usr/bin/doltlite go test -run=^$ -bench=. -benchtime=1x ./...
//   DOLTLITE_BENCH=1 DLITE=/tmp/dl_patched/doltlite go test -run=^$ -bench=. -benchtime=1x ./...
// then `benchstat` the two outputs.

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func benchSetup(b *testing.B) (dlite string, rows int) {
	b.Helper()
	if os.Getenv("DOLTLITE_BENCH") != "1" {
		b.Skip("set DOLTLITE_BENCH=1 to run perf benchmarks")
	}
	dlite = os.Getenv("DLITE")
	if dlite == "" {
		dlite = "doltlite"
	}
	if _, err := exec.LookPath(dlite); err != nil {
		b.Skipf("doltlite binary %q not found", dlite)
	}
	rows = 2000
	if v := os.Getenv("DOLTLITE_BENCH_ROWS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			rows = n
		}
	}
	return dlite, rows
}

// runScript pipes a SQL script into doltlite via stdin (.read $-) so we
// don't pay file-open cost per statement.
func runScript(b *testing.B, dlite, db, script string) time.Duration {
	b.Helper()
	scriptPath := filepath.Join(b.TempDir(), "script.sql")
	if err := os.WriteFile(scriptPath, []byte(script), 0o644); err != nil {
		b.Fatalf("write script: %v", err)
	}
	cmd := exec.Command(dlite, db, fmt.Sprintf(".read %s", scriptPath))
	t0 := time.Now()
	out, err := cmd.CombinedOutput()
	d := time.Since(t0)
	if err != nil {
		b.Fatalf("doltlite run failed: %v\n%s", err, out)
	}
	return d
}

func seedAndUpdate(b *testing.B, pkType, pkExpr string) {
	dlite, rows := benchSetup(b)
	for i := 0; i < b.N; i++ {
		dir := b.TempDir()
		db := filepath.Join(dir, "bench.dl")

		var seed strings.Builder
		fmt.Fprintf(&seed, "CREATE TABLE t (pk %s PRIMARY KEY, payload TEXT);\nBEGIN;\n", pkType)
		for k := 1; k <= rows; k++ {
			fmt.Fprintf(&seed, "INSERT INTO t VALUES (%s, 'x');\n", fmt.Sprintf(pkExpr, k))
		}
		seed.WriteString("COMMIT;\n")
		runScript(b, dlite, db, seed.String())

		var upd strings.Builder
		upd.WriteString("BEGIN;\n")
		for k := 1; k <= rows; k++ {
			fmt.Fprintf(&upd, "UPDATE t SET payload='y' WHERE pk=%s;\n", fmt.Sprintf(pkExpr, k))
		}
		upd.WriteString("COMMIT;\n")

		b.StartTimer()
		d := runScript(b, dlite, db, upd.String())
		b.StopTimer()

		usPerUpdate := float64(d.Microseconds()) / float64(rows)
		b.ReportMetric(usPerUpdate, "us/update")
	}
}

func BenchmarkDoltliteIntPKUpdate(b *testing.B) {
	b.StopTimer()
	seedAndUpdate(b, "INTEGER", "%d")
}

func BenchmarkDoltliteTextPKUpdate(b *testing.B) {
	b.StopTimer()
	// 36-char UUID-shaped key — exercises the TEXT-PK slow path that #718 reports.
	seedAndUpdate(b, "TEXT", "'uuid-%08x-aaaa-bbbb-cccc-000000000000'")
}

// Compares the cost of N separate INSERTs vs one INSERT … VALUES (…),(…),(…)
// of N rows, both inside one BEGIN/COMMIT. Confirms whether coalesceInserts
// actually moves the needle on doltlite's bulk-insert path.
func benchInsertShape(b *testing.B, multiRow bool) {
	dlite, rows := benchSetup(b)
	for i := 0; i < b.N; i++ {
		dir := b.TempDir()
		db := filepath.Join(dir, "bench.dl")

		var script strings.Builder
		script.WriteString("CREATE TABLE t (pk TEXT PRIMARY KEY, payload TEXT);\nBEGIN;\n")
		if multiRow {
			script.WriteString("INSERT INTO t VALUES ")
			for k := 1; k <= rows; k++ {
				if k > 1 {
					script.WriteString(", ")
				}
				fmt.Fprintf(&script, "('uuid-%08x-aaaa-bbbb-cccc-000000000000', 'x')", k)
			}
			script.WriteString(";\n")
		} else {
			for k := 1; k <= rows; k++ {
				fmt.Fprintf(&script, "INSERT INTO t VALUES ('uuid-%08x-aaaa-bbbb-cccc-000000000000', 'x');\n", k)
			}
		}
		script.WriteString("COMMIT;\n")

		b.StartTimer()
		d := runScript(b, dlite, db, script.String())
		b.StopTimer()
		b.ReportMetric(float64(d.Microseconds())/float64(rows), "us/row")
	}
}

func BenchmarkDoltliteTextPKInsertSeparate(b *testing.B) {
	b.StopTimer()
	benchInsertShape(b, false)
}

func BenchmarkDoltliteTextPKInsertMultiRow(b *testing.B) {
	b.StopTimer()
	benchInsertShape(b, true)
}

// Compares N per-row UPDATE-by-PK statements vs a single CASE-batched UPDATE
// touching the same N rows, both inside one BEGIN/COMMIT. Validates whether
// coalesceUpdates can collapse the bulk-remap chunks.
func benchUpdateShape(b *testing.B, batched bool) {
	dlite, rows := benchSetup(b)
	for i := 0; i < b.N; i++ {
		dir := b.TempDir()
		db := filepath.Join(dir, "bench.dl")

		var seed strings.Builder
		seed.WriteString("CREATE TABLE t (pk TEXT PRIMARY KEY, payload TEXT);\nBEGIN;\n")
		for k := 1; k <= rows; k++ {
			fmt.Fprintf(&seed, "INSERT INTO t VALUES ('uuid-%08x-aaaa-bbbb-cccc-000000000000', 'old-%d');\n", k, k)
		}
		seed.WriteString("COMMIT;\n")
		runScript(b, dlite, db, seed.String())

		var upd strings.Builder
		upd.WriteString("BEGIN;\n")
		if batched {
			upd.WriteString("UPDATE t SET payload = CASE pk ")
			for k := 1; k <= rows; k++ {
				fmt.Fprintf(&upd, "WHEN 'uuid-%08x-aaaa-bbbb-cccc-000000000000' THEN 'new-%d' ", k, k)
			}
			upd.WriteString("ELSE payload END WHERE pk IN (")
			for k := 1; k <= rows; k++ {
				if k > 1 {
					upd.WriteString(",")
				}
				fmt.Fprintf(&upd, "'uuid-%08x-aaaa-bbbb-cccc-000000000000'", k)
			}
			upd.WriteString(");\n")
		} else {
			for k := 1; k <= rows; k++ {
				fmt.Fprintf(&upd, "UPDATE t SET payload='new-%d' WHERE pk='uuid-%08x-aaaa-bbbb-cccc-000000000000';\n", k, k)
			}
		}
		upd.WriteString("COMMIT;\n")

		b.StartTimer()
		d := runScript(b, dlite, db, upd.String())
		b.StopTimer()
		b.ReportMetric(float64(d.Microseconds())/float64(rows), "us/update")
	}
}

func BenchmarkDoltliteTextPKUpdateSeparate(b *testing.B) {
	b.StopTimer()
	benchUpdateShape(b, false)
}

func BenchmarkDoltliteTextPKUpdateCASE(b *testing.B) {
	b.StopTimer()
	benchUpdateShape(b, true)
}
