package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Reproduces what dolt-replay does end-to-end on the multiline-string scenario:
// two sequential applies on one session, second insert is multi-line.
func TestDoltliteSession_MultilineSequential(t *testing.T) {
	bin := requireDoltlite(t)
	dir := t.TempDir()
	db := filepath.Join(dir, "x.dl")

	// Pre-create the DB the way the E2E test does, then open a session.
	if out, err := execOutput(bin, db, "SELECT 1"); err != nil {
		t.Fatalf("pre-create db: %v\n%s", err, out)
	}

	s, err := newDoltliteSession(bin, db)
	if err != nil {
		t.Fatal(err)
	}

	s1 := writeScript(t, dir, "BEGIN;\nCREATE TABLE IF NOT EXISTS \"notes\" (\n  \"id\" INTEGER NOT NULL,\n  \"body\" text,\n  PRIMARY KEY (\"id\")\n);\nCOMMIT;\n")
	if _, errs, err := s.Apply(s1, 30*time.Second); err != nil || looksLikeDoltliteError(errs) {
		t.Fatalf("apply1: err=%v stderr=%q", err, errs)
	}

	multiline := "BEGIN;\nINSERT OR REPLACE INTO \"notes\" (\"id\",\"body\") VALUES (1,'## Tablet\nLine2 ; here\nLine3');\nCOMMIT;\n"
	s2 := filepath.Join(dir, "s2.sql")
	if err := os.WriteFile(s2, []byte(multiline), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, errs, err := s.Apply(s2, 30*time.Second); err != nil || looksLikeDoltliteError(errs) {
		t.Fatalf("apply2: err=%v stderr=%q", err, errs)
	}

	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	t.Logf("verifying via fresh doltlite invocation")
	if got := dliteCountStandalone(t, bin, db, "notes"); got != 1 {
		t.Fatalf("got %d, want 1", got)
	}
}

func dliteCountStandalone(t *testing.T, bin, db, table string) int {
	t.Helper()
	out, err := execOutput(bin, db, "SELECT COUNT(*) FROM "+table)
	if err != nil {
		t.Fatalf("count: %v\n%s", err, out)
	}
	var n int
	if _, err := tryAtoi(out, &n); err != nil {
		t.Fatalf("parse %q: %v", out, err)
	}
	return n
}

func execOutput(prog string, args ...string) (string, error) {
	out, _, err := run("", "", prog, args...)
	return out, err
}

func tryAtoi(s string, n *int) (int, error) {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			s = s[:i]
			break
		}
	}
	if s == "" {
		return 0, os.ErrInvalid
	}
	v := 0
	for i := 0; i < len(s); i++ {
		v = v*10 + int(s[i]-'0')
	}
	*n = v
	return v, nil
}
