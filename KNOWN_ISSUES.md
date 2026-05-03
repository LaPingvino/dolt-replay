# Known Issues

## Upstream dolt: `dolt diff -r sql` drops data when commit also has schema changes

**Repro**: take any commit that includes both `ALTER TABLE X ADD COLUMN`
and significant row churn on `X` (e.g. the bahaiwritings `uibsgeher`
"Rebuild prayer_book_structure" commit: ALTERs + 5,123 DELETEs +
2,325 INSERTs).

- `dolt diff --stat <parent> <commit>` reports the row counts correctly.
- `dolt diff -r sql <parent> <commit>` emits **only** the ALTERs — every
  DELETE/INSERT/UPDATE row-level statement is silently swallowed.

**Impact on bahaiwritings clone**: `prayer_book_structure` ends up +4,952
rows (29% over source) — almost exactly the 5,123 DELETEs the rebuild
commit dropped. Tables without combined-schema-and-data commits clone
byte-for-byte (`inventory`, `languages`, ...).

**Workarounds** (none implemented):
1. After a commit that contains an `ALTER TABLE X …`, re-sync `X` rows
   (delete-where-not-in-source plus insert-where-not-in-target). Heavy.
2. Use `dolt sql -q "SELECT * FROM dolt_diff_X AS OF '<commit>'"` to
   read deltas independently of the SQL emitter; would need a row→SQL
   translator.
3. File upstream — failure mode is in the SQL emitter, not the stored
   delta (`--stat` sees the rows fine).

## Local: `INSERT OR IGNORE` in PK rebuild drops NOT-NULL-violating rows

When the new schema declares `NOT NULL` on a column where some old rows
have `NULL`, the rebuild's `INSERT OR IGNORE` drops them. On
bahaiwritings, this costs ~482 `writings` rows at the
`il22vdccku` "Change primary key to version" commit (1% under source).

**Workaround** (none implemented): backfill `NULL` versions with synthetic
UUIDs in the rebuild SELECT — source did this implicitly via dolt's MySQL
`DEFAULT uuid()` evaluated at ALTER time; doltlite stores the default as
the literal string `'uuid()'` because it doesn't recognize the function
form, so the default never fires for backfill.

## SQLite/doltlite DDL gaps (full-clone limitation)

When replaying `dolt → doltlite`, several MySQL DDLs have no direct
SQLite/doltlite equivalent and require a full table rebuild (CREATE
new table, copy data, DROP old, RENAME). dolt-replay currently
**skips** these statements with an inline `-- skipped` comment so the
walk doesn't halt, but the schema then drifts from source:

- `ALTER TABLE … MODIFY COLUMN …`
- `ALTER TABLE … DROP PRIMARY KEY` / `ADD PRIMARY KEY`
- `ALTER TABLE … ADD CONSTRAINT … FOREIGN KEY …`
- `ALTER TABLE … DROP COLUMN <pk_col>` (errors at apply time;
  surfaces as a `--continue-on-error` failure rather than a silent skip)

For a faithful clone, run with `--continue-on-error` and audit the
final schema against `dolt --schema-only dump` of the source. A future
version could implement schema-rebuild emulation for these cases.

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
