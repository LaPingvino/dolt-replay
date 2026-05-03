package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func requireDoltlite(t *testing.T) string {
	t.Helper()
	bin, err := exec.LookPath("doltlite")
	if err != nil {
		t.Skip("doltlite not installed")
	}
	return bin
}

func writeScript(t *testing.T, dir, content string) string {
	t.Helper()
	p := filepath.Join(dir, "script.sql")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestDoltliteSession_BasicApply(t *testing.T) {
	bin := requireDoltlite(t)
	dir := t.TempDir()
	db := filepath.Join(dir, "x.dl")

	s, err := newDoltliteSession(bin, db)
	if err != nil {
		t.Fatalf("session start: %v", err)
	}
	defer s.Close()

	script := writeScript(t, dir, "CREATE TABLE t (a INT PRIMARY KEY);\nINSERT INTO t VALUES (1), (2), (3);\n")
	_, stderr, err := s.Apply(script, 30*time.Second)
	if err != nil {
		t.Fatalf("apply: %v\nstderr: %s", err, stderr)
	}
	if looksLikeDoltliteError(stderr) {
		t.Fatalf("unexpected error in stderr: %s", stderr)
	}

	script2 := writeScript(t, dir, "INSERT INTO t VALUES (4);\n")
	_, stderr, err = s.Apply(script2, 30*time.Second)
	if err != nil {
		t.Fatalf("apply 2: %v\nstderr: %s", err, stderr)
	}
	if looksLikeDoltliteError(stderr) {
		t.Fatalf("unexpected error in stderr 2: %s", stderr)
	}

	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// verify rows landed by counting via a fresh one-shot doltlite invocation
	cmd := exec.Command(bin, db, "SELECT COUNT(*) FROM t")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("count: %v\n%s", err, out)
	}
	got := strings.TrimSpace(string(out))
	if got != "4" {
		t.Fatalf("expected 4 rows, got %q", got)
	}
}

func TestDoltliteSession_ErrorDetection(t *testing.T) {
	bin := requireDoltlite(t)
	dir := t.TempDir()
	db := filepath.Join(dir, "x.dl")

	s, err := newDoltliteSession(bin, db)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// Reference a table that doesn't exist — should leave a marker on stderr
	script := writeScript(t, dir, "INSERT INTO does_not_exist VALUES (1);\n")
	_, stderr, err := s.Apply(script, 30*time.Second)
	if err != nil {
		// process may have stayed alive (.bail aborts the .read but not the REPL)
		// — that's fine, we only care that we got the error signal back.
		t.Logf("apply returned err (may be ok): %v", err)
	}
	if !looksLikeDoltliteError(stderr) {
		t.Fatalf("expected error marker in stderr, got: %q", stderr)
	}
}
