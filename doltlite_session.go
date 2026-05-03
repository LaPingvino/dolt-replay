package main

// doltliteSession holds one long-lived doltlite REPL subprocess against a
// single database file. Each chunk apply is sent as `.read <tmpfile>` plus a
// per-apply EOC sentinel that we wait for on stdout, so we keep the same
// strict ordering semantics as the one-shot path while paying the doltlite
// process startup + DB open cost only once per clone.
//
// Errors are detected by checking stderr for known doltlite error markers
// during the apply window. On any error the session is closed; callers can
// fall back to the one-shot path or restart the session.

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"
)

type doltliteSession struct {
	bin, db string
	cmd     *exec.Cmd
	stdin   io.WriteCloser

	// stdoutCh is fed by a single long-running goroutine reading the subprocess
	// stdout line-by-line. Apply consumes from it until the per-call EOC marker
	// is seen. One pump avoids the concurrent-bufio.Reader hazard of spawning a
	// reader per Apply.
	stdoutCh  chan stdoutLine
	stderrMu  sync.Mutex
	stderrBuf bytes.Buffer

	seq    int
	closed bool
}

type stdoutLine struct {
	line string
	err  error
}

func newDoltliteSession(bin, db string) (*doltliteSession, error) {
	cmd := exec.Command(bin, db)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	s := &doltliteSession{
		bin:      bin,
		db:       db,
		cmd:      cmd,
		stdin:    stdin,
		stdoutCh: make(chan stdoutLine, 64),
	}
	go func() {
		r := bufio.NewReader(stdoutPipe)
		for {
			line, err := r.ReadString('\n')
			if line != "" {
				s.stdoutCh <- stdoutLine{line, nil}
			}
			if err != nil {
				s.stdoutCh <- stdoutLine{"", err}
				return
			}
		}
	}()
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := stderrPipe.Read(buf)
			if n > 0 {
				s.stderrMu.Lock()
				s.stderrBuf.Write(buf[:n])
				s.stderrMu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()
	return s, nil
}

// Apply runs the SQL in scriptPath inside the session and returns
// (stdout-since-last-apply, stderr-since-last-apply, error). An error is
// returned only if the subprocess died or the EOC sentinel didn't arrive
// within the timeout — callers must inspect stderr for SQL-level errors,
// the same way the one-shot path inspected the doltlite command's stderr.
func (s *doltliteSession) Apply(scriptPath string, timeout time.Duration) (string, string, error) {
	if s.closed {
		return "", "", fmt.Errorf("session closed")
	}
	s.seq++
	marker := fmt.Sprintf("__DOLT_REPLAY_EOC_%d__", s.seq)
	// Snapshot stderr offset so we only return what arrived during this apply.
	s.stderrMu.Lock()
	preStderrLen := s.stderrBuf.Len()
	s.stderrMu.Unlock()

	cmds := fmt.Sprintf(".bail on\n.read %s\n.bail off\n.print %s\n", scriptPath, marker)
	if _, err := io.WriteString(s.stdin, cmds); err != nil {
		s.closeQuietly()
		return "", "", fmt.Errorf("write to doltlite stdin: %w", err)
	}

	var out bytes.Buffer
	deadline := time.After(timeout)
	for {
		select {
		case r := <-s.stdoutCh:
			if r.err != nil {
				s.closeQuietly()
				return out.String(), s.drainStderrSince(preStderrLen), fmt.Errorf("doltlite stdout EOF: %w", r.err)
			}
			out.WriteString(r.line)
			if strings.Contains(r.line, marker) {
				return out.String(), s.drainStderrSince(preStderrLen), nil
			}
		case <-deadline:
			s.closeQuietly()
			return out.String(), s.drainStderrSince(preStderrLen), fmt.Errorf("doltlite apply timeout after %s", timeout)
		}
	}
}

func (s *doltliteSession) drainStderrSince(off int) string {
	s.stderrMu.Lock()
	defer s.stderrMu.Unlock()
	if off >= s.stderrBuf.Len() {
		return ""
	}
	return s.stderrBuf.String()[off:]
}

func (s *doltliteSession) Close() error {
	if s.closed {
		return nil
	}
	s.closed = true
	io.WriteString(s.stdin, ".exit\n")
	s.stdin.Close()
	done := make(chan error, 1)
	go func() { done <- s.cmd.Wait() }()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		s.cmd.Process.Kill()
		return fmt.Errorf("doltlite did not exit cleanly within 5s")
	}
}

func (s *doltliteSession) closeQuietly() { _ = s.Close() }

// looksLikeDoltliteError returns true if the stderr block contains markers
// doltlite emits when a SQL statement fails inside `.read`. Conservative:
// false negatives mean we miss an error and continue (caller may then catch
// it via row-count mismatch); false positives mean we abort a successful
// apply (worse), so we keep the patterns specific.
func looksLikeDoltliteError(stderr string) bool {
	if stderr == "" {
		return false
	}
	for _, m := range []string{
		"Error:",
		"Runtime error",
		"Parse error",
		"near line",
	} {
		if strings.Contains(stderr, m) {
			return true
		}
	}
	return false
}
