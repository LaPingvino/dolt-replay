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
2000-row doltlite ceiling that bites large-table replays.
