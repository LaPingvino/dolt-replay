# Known Issues

## ✅ Fixed locally (commit 367929a): silent-skip on combined schema+data commits

**Was:** any commit that includes both `ALTER TABLE X` and row churn on
`X` had the row-level DELETE/INSERT/UPDATE statements silently dropped
by `dolt diff -r sql`. On bahaiwritings, this caused
`prayer_book_structure` to end up +4,952 rows (29% over source).

**Fix:** `doltDiffSQLViaRebase` (commit 367929a) — when `dolt diff -r sql`
writes `Incompatible schema change, skipping data diff` to stderr, the
tool branches from parent into `__replay_baseline`, applies the schema
delta, then diffs `__replay_baseline → child --data`. Schemas match by
construction, so the upstream guard no longer fires and the DML emits.

This is exactly the "schema-then-diff-against-rebased-baseline" approach
proposed by nicktobey upstream in dolthub/dolt#10988. The test matrix
(48 cells in `schema_replay_test.go`, including nicktobey's case 1 +
case 2 + the bahaiwritings shape) now passes 100%. Round-trip
integration check on the actual bahaiwritings 482-commit history is
running at the time of writing; results will land in
`/tmp/bahaiwritings-replay/compare.log`.

Upstream conversation:
- dolthub/dolt#10988 — algorithm + worked examples + test spec for the
  upstream fix (consumer-side workaround vs. proper diff-time fix).
- dolthub/doltlite#738/#739/#740 — companion gaps in doltlite's system
  tables that the work surfaced.

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

## SQLite/doltlite DDL gaps (partial coverage)

When replaying `dolt → doltlite`, several MySQL DDLs have no direct
SQLite/doltlite equivalent. The schema-replay test suite covers the
type-change shapes via the SQLite table-rebuild pattern (CREATE new
table, copy data, DROP old, RENAME). The remaining shapes still skip
with an inline `-- skipped` comment:

- `ALTER TABLE … MODIFY COLUMN …` — covered for tests via the rebuild
  pattern in `schema_replay_test.go`. Real source repos that emit
  MODIFY directly still skip; would need detection-and-rewrite to
  the rebuild pattern in the dialect translator.
- `ALTER TABLE … DROP PRIMARY KEY` / `ADD PRIMARY KEY` — partially
  handled by the PK-rebuild path in `applyToDoltlite`.
- `ALTER TABLE … ADD CONSTRAINT … FOREIGN KEY …`
- `ALTER TABLE … DROP COLUMN <pk_col>` (errors at apply time;
  surfaces as a `--continue-on-error` failure rather than a silent skip)

For a faithful clone, run with `--continue-on-error` and audit the
final schema against `dolt --schema-only dump` of the source.

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
