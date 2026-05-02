# Known Issues

## ✅ Fixed in doltlite v0.9.1

The bulk-INSERT row-loss bug below was fixed upstream in
[v0.9.1](https://github.com/dolthub/doltlite/releases/tag/v0.9.1)
(release notes: "Bulk `.read INSERT VALUES` row loss (#713)"). Verified
locally with the original 5000-row repro: all 5000 rows persist in both
autocommit and `BEGIN; ... COMMIT;`-wrapped modes. The notes below are
kept for historical reference; users on v0.9.0 should upgrade.

## doltlite v0.9.0 (fixed in v0.9.1): ~2000-row ceiling on bulk INSERT VALUES via `.read`

When applying many `INSERT INTO ... VALUES (...)` statements to a doltlite
database via `.read file.sql`, only the last ~167-1900 rows persist (count
varies with row width). Rows past the ceiling are silently dropped — no
error is reported, the commit succeeds, but `SELECT COUNT(*)` shows the
shortfall.

Wrapping in `BEGIN; ... COMMIT;` does not fully resolve it for wide rows
(5-column tables observed).

`INSERT INTO t SELECT FROM generate_series(...)` on a 1-column table
persists correctly to at least 5000 rows, suggesting the bug is specific
to many literal multi-column `VALUES` clauses being parsed/applied.

A reproduction script is included at `repro/doltlite_2000_row_ceiling.sh`.

Filed upstream: https://github.com/dolthub/doltlite/issues/710

### Impact on dolt-replay

For tables whose initial-population commit contains >~2000 INSERTs (e.g.
the `quran_verse_numbering` table with 6236 rows), only a fraction will
be replayed into a doltlite target. The tool emits a WARNING and proceeds
with what it can.

Workarounds while upstream is unfixed:
- Use a doltlite source if your starting state is already in a doltlite
  DB (read path doesn't trigger the bug).
- For dolt → doltlite of large tables, dump source via `dolt sql` to
  CSV and use doltlite's `.import` (untested in this POC).
