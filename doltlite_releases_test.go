//go:build doltlite_releases

// Regression check across every published doltlite release on GitHub.
//
// Heavy: downloads each release's linux-x64 tools zip on first run and
// caches them under testdata/doltlite-bins/. Skipped by default; opt in
// with `go test -tags doltlite_releases -v -run TestDoltliteReleases`.
//
// Asserts the wide-row bulk-INSERT repro from KNOWN_ISSUES.md persists
// all 5000 rows. v0.9.0 is the one known-broken release (#710, fixed in
// v0.9.1) and is asserted to drop rows so a future doltlite republish
// would surface the regression instead of silently passing.
package main

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const (
	releasesAPI    = "https://api.github.com/repos/dolthub/doltlite/releases?per_page=100"
	assetTemplate  = "doltlite-tools-linux-x64-%s.zip"
	cacheDir       = "testdata/doltlite-bins"
	expectedRows   = 5000
	brokenInV090   = 1342 // observed row count on v0.9.0 with the wide-row repro
)

type ghRelease struct {
	TagName string `json:"tag_name"`
	Assets  []struct {
		Name        string `json:"name"`
		DownloadURL string `json:"browser_download_url"`
	} `json:"assets"`
}

func TestDoltliteReleases(t *testing.T) {
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		t.Skipf("release binaries are linux-x64 only; running on %s-%s", runtime.GOOS, runtime.GOARCH)
	}

	releases, err := fetchReleases()
	if err != nil {
		t.Fatalf("list releases: %v", err)
	}
	if len(releases) == 0 {
		t.Fatal("no releases returned")
	}

	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		t.Fatal(err)
	}

	for _, rel := range releases {
		rel := rel
		t.Run(rel.TagName, func(t *testing.T) {
			version := strings.TrimPrefix(rel.TagName, "v")
			assetName := fmt.Sprintf(assetTemplate, version)

			var url string
			for _, a := range rel.Assets {
				if a.Name == assetName {
					url = a.DownloadURL
					break
				}
			}
			if url == "" {
				t.Skipf("no %s asset on this release", assetName)
			}

			bin, err := ensureBinary(rel.TagName, url)
			if err != nil {
				t.Fatalf("fetch binary: %v", err)
			}

			rows, err := runRepro(t, bin)
			if err != nil {
				t.Fatalf("run repro: %v", err)
			}

			switch rel.TagName {
			case "v0.9.0":
				// Known broken — assert the bug is still reproducible so a
				// silent fix (without a release bump) would be caught.
				if rows >= expectedRows {
					t.Fatalf("v0.9.0 unexpectedly persisted %d rows; bug may have been hot-patched without a release bump", rows)
				}
				if rows != brokenInV090 {
					t.Logf("v0.9.0 persisted %d rows (originally observed %d) — still buggy, allowed", rows, brokenInV090)
				}
			default:
				if rows != expectedRows {
					t.Fatalf("expected %d rows, got %d", expectedRows, rows)
				}
			}
		})
	}
}

func fetchReleases() ([]ghRelease, error) {
	req, _ := http.NewRequest("GET", releasesAPI, nil)
	req.Header.Set("Accept", "application/vnd.github+json")
	if tok := os.Getenv("GITHUB_TOKEN"); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("GitHub API %d: %s", resp.StatusCode, body)
	}
	var rels []ghRelease
	if err := json.NewDecoder(resp.Body).Decode(&rels); err != nil {
		return nil, err
	}
	return rels, nil
}

func ensureBinary(tag, url string) (string, error) {
	dir := filepath.Join(cacheDir, tag)
	bin := filepath.Join(dir, "doltlite")
	if _, err := os.Stat(bin); err == nil {
		return bin, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}

	resp, err := http.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	zipPath := filepath.Join(dir, "asset.zip")
	f, err := os.Create(zipPath)
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		return "", err
	}
	f.Close()

	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return "", err
	}
	defer zr.Close()
	for _, zf := range zr.File {
		if filepath.Base(zf.Name) != "doltlite" {
			continue
		}
		rc, err := zf.Open()
		if err != nil {
			return "", err
		}
		out, err := os.OpenFile(bin, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
		if err != nil {
			rc.Close()
			return "", err
		}
		if _, err := io.Copy(out, rc); err != nil {
			rc.Close()
			out.Close()
			return "", err
		}
		rc.Close()
		out.Close()
		_ = os.Remove(zipPath)
		return bin, nil
	}
	return "", fmt.Errorf("doltlite binary not found inside %s", url)
}

func runRepro(t *testing.T, bin string) (int, error) {
	t.Helper()
	tmp := t.TempDir()
	db := filepath.Join(tmp, "test.dl")
	sqlPath := filepath.Join(tmp, "load.sql")

	if out, err := exec.Command(bin, db,
		`CREATE TABLE t (a INTEGER NOT NULL, b INTEGER NOT NULL, c INTEGER, d INTEGER, e TEXT, PRIMARY KEY (a,b));`,
	).CombinedOutput(); err != nil {
		return 0, fmt.Errorf("create table: %v: %s", err, out)
	}

	var sb strings.Builder
	sb.WriteString("BEGIN;\n")
	for i := 1; i <= expectedRows; i++ {
		fmt.Fprintf(&sb, "INSERT INTO t (a,b,c,d,e) VALUES (%d,%d,%d,%d,NULL);\n", i, i, i, i)
	}
	sb.WriteString("COMMIT;\n")
	if err := os.WriteFile(sqlPath, []byte(sb.String()), 0o644); err != nil {
		return 0, err
	}

	if out, err := exec.Command(bin, "-bail", db,
		"-cmd", ".read "+sqlPath,
		"SELECT dolt_commit('-A','-m','5k');",
	).CombinedOutput(); err != nil {
		return 0, fmt.Errorf("load: %v: %s", err, out)
	}

	out, err := exec.Command(bin, db, "SELECT COUNT(*) FROM t").Output()
	if err != nil {
		return 0, err
	}
	var rows int
	if _, err := fmt.Sscanf(strings.TrimSpace(string(out)), "%d", &rows); err != nil {
		return 0, fmt.Errorf("parse count %q: %v", out, err)
	}
	return rows, nil
}
