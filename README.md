# dolt-replay

Replay commit history between [Dolt](https://github.com/dolthub/dolt) and
[Doltlite](https://github.com/dolthub/doltlite) databases.

Supports four source/target combos:

- `dolt → dolt`
- `dolt → doltlite`
- `doltlite → dolt`
- `doltlite → doltlite` *(useful for migrating across incompatible doltlite
  version upgrades — replay history through a fresh DB built by the current
  binary)*

For each commit in the source range, the tool extracts a diff for one table,
translates dialect quirks for the target (varchar→TEXT, smallint→INTEGER,
backticks↔double-quotes), applies the SQL, then creates a new commit using
the original message + author + date.

## Usage

```sh
go run . \
  --src-kind dolt    --src ~/some/dolt-repo \
  --dst-kind doltlite --dst /tmp/out.dl \
  --table writings --limit 5
```

Required flags: `--src-kind`, `--src`, `--dst-kind`, `--dst`, `--table`.
Optional: `--limit N` (default 5), `--dry-run` (print SQL only).

## Status

POC. Works end-to-end for small tables; see `KNOWN_ISSUES.md` for the
2000-row doltlite ceiling that bit large-table replays on v0.9.0
(fixed in v0.9.1 — verified).

## Tests

```sh
go test ./...                                          # unit tests (fast, hermetic)
go test -tags doltlite_releases -v -run TestDoltlite   # downloads every doltlite release
                                                       # and runs the bulk-INSERT repro
```

The `doltlite_releases` build tag opt-in is heavy: it fetches each
release's `linux-x64` tools zip from GitHub on first run and caches them
under `testdata/doltlite-bins/`. It also asserts that v0.9.0 *still*
reproduces the bug (#710), so a future hot-patch without a release bump
would surface as a test failure rather than a silent change.
