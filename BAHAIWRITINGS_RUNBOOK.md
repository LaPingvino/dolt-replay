# Bahaiwritings round-trip findings

Run start: 2026-05-05 01:31:28
Run end (hop2 failed): 2026-05-05 02:03:07

## Hop 1 (dolt → doltlite) — succeeded

- 482 commits walked, 477 replayed, 5 skipped (empty-diff or root-parent), 0 failed.
- Wall time: ~15.5 min.
- Output: `/tmp/bw-hop1.dl` (≈1 GB).

## Hop 2 (doltlite → dolt) — failed at commit ~13/470

The chain.sh watcher kicked off hop2 automatically. It got through the
schema-establishment commits and failed on the first big data-load commit
("Add English prayerbook to writings table").

### Failure mode

Replay emit included:
```sql
INSERT INTO `writings` (`id`,`phelps`,`language`,`version`,`notes`,`bpn`,...)
VALUES ('BH10581','BH10581','en',NULL,'376','376',...)
```

Dolt rejected: `error: 'BH10581' is not a valid value for 'int'` — the
`id` column is INT, but the slot received the string 'BH10581' (which
should have gone in the `phelps` slot).

### Root cause

The positional column-mapping fallback in `doltliteDiffSQL` (added in
commit 5188a74) is too aggressive when the schema at the historical
commit has *more columns* than the HEAD-aligned `dolt_diff_<table>`
header. Concretely:

- **Schema at the prayerbook commit** (from doltlite's dolt_schema_diff
  to_create_statement): `(id, phelps, language, version, name, type,
  notes, bpn, bpnlink, ..., sources, text)`.
- **HEAD's pragma_table_info on writings**: `(phelps, language, version,
  name, type, notes, link, text, source, source_id, is_verified)` — no
  `id`, no `bpn`, no `bpnlink`. The schema has *evolved* (columns
  dropped, others added).

For schema-at-child column `id`, direct `to_id` lookup on the dolt_diff
header fails (HEAD has no `id`). Positional fallback uses headCols[0] =
'phelps', which *does* have `to_phelps`. So the `id` slot gets mapped to
phelps's data column. For schema-at-child column `phelps` (i=1), direct
lookup of `to_phelps` succeeds — same column. Both `id` and `phelps`
emit-slots point at `to_phelps`'s value, and we get the duplicate
'BH10581' shown above.

### Why the test suite didn't catch this

The 48-cell matrix exercises histories where the schema at HEAD *matches*
the schema-at-child for every test (no dropped columns between
intermediate commits and HEAD). The positional fallback is only stressed
when columns have been added by HEAD (rename case). It hadn't been
stressed by columns dropped between intermediate and HEAD.

### UPDATE 02:14 — fix landed (commit d5014fd)
### UPDATE 02:14 — past the big import commit on hop2 retry

Hop2 retry got past the "Import from SQLite database" seed commit
(commit 1, the heavy one) and is moving through smaller schema-only
commits. No emitted-SQL-rejection errors yet under the new column
mapping. Throughput pending.

### UPDATE 03:30 — round-trip incomplete; new failure mode is clean

Hop2 retry stopped at commit 14 of 470 with status:

- 14 commits walked (mostly schema setup + the big initial Import).
- 2 errors logged — both at the first writings-data commit ("Add
  English prayerbook"), this time with a different message:

  ```
  Field 'id' doesn't have a default value
  ```

- A2 row counts: only `languages` and `writings` tables present,
  both empty. A1 has 21 tables with millions of rows.

The `id`-aliasing data corruption from the original failure is gone
— the two-pass column mapping correctly skips the unrecoverable
`id` slot. But now the INSERT lacks `id` entirely, and dolt rejects
because the table was created with `id INT PRIMARY KEY` (NOT NULL,
no default).

### Root cause is fundamental for histories like bahaiwritings

`dolt_diff_<table>` is HEAD-aligned. When a column existed at an
intermediate commit but was dropped before HEAD, that column's
values are unrecoverable from the HEAD-aligned diff header. No
amount of column-mapping cleverness on the consumer side can
reconstruct data the source-side diff layer has erased.

For bahaiwritings.writings specifically: `id INT PK` was dropped
mid-history (after the Phelps codes became the natural key).
Replaying through a doltlite intermediate where HEAD lacks `id`
means none of the early `INSERT INTO writings (id=..., ...)`
operations can be reconstructed. The target rejects the projected
INSERTs because `id` is still in the schema at that intermediate
commit.

### Fix shape (tomorrow)

The two-pass column mapping (commit d5014fd) is the right local
fix and lands a regression test. The bahaiwritings-class case
needs a higher-level move: project the **schema emit** to drop
columns whose values aren't recoverable. Specifically:

- When `doltliteSchemaChangeSQL` emits a CREATE TABLE for a "new
  table" commit (parent = no table, child = table with cols X),
  intersect X with HEAD's table cols. Cols missing at HEAD are
  dropped from the CREATE.
- Subsequent ALTER ADD COLUMN ops should also project away cols
  not at HEAD.
- The PRIMARY KEY clause needs special-casing — if the only PK
  col is being dropped, fall back to whatever PK the table ends
  up with at HEAD (look up via pragma_table_info or AS OF HEAD).

This is multi-tick work and changes semantics (target's intermediate
schemas no longer match source's intermediate schemas — only the
final state matches). Worth doing if bahaiwritings-class round
trips are a priority. Defer until we discuss whether final-state
round-trip is enough for the consumer's use case.

### What's solid

- 48-cell unit matrix: green.
- Chain tests (dolt→doltlite→dolt and doltlite→doltlite→doltlite,
  on synthetic histories): green.
- Three doltlite issues filed (#738, #739, #740) covering the
  system-table gaps.
- Two upstream comments on dolthub/dolt#10988 (consumer-side
  description + recommendation with worked examples + test spec).
- d5014fd column-mapping fix prevents the data corruption that
  the original failure exhibited.

### Files to inspect when reviewing

- `/tmp/bahaiwritings-replay/findings.md` — this file.
- `/tmp/bahaiwritings-replay/hop1.log`, `hop2.log` — full replay logs.
- `/tmp/bahaiwritings-replay/compare.log` — A1 vs A2 row counts.
- `/tmp/bw-hop1.dl` — the doltlite intermediate (~1 GB).
- `/tmp/bw-hop2-dst/` — partial dolt destination.



The two-pass column mapping prevents same-slot aliasing. First pass
satisfies direct-name lookups and claims the target dolt_diff
columns; second pass attempts positional fallback only against
unclaimed columns. Schema-at-child cols with no recoverable mapping
are skipped from the emit.

Suite + chain tests still green. Added `TestReplaySchema_DropColAfterDataInsert`
as a regression case.

Hop2 retry started at 02:10 with `--continue-on-error`, currently
on commit 2 of 470 (the big "Import from SQLite database" seed
commit). ETA unknown — slow start.

### Fix shape (original, kept for reference)

Two parts:

1. **Positional fallback should be conservative**: only map schema-at-child[i]
   to headCols[i] when `headCols[i]` *doesn't already correspond to another
   schema-at-child column by direct name lookup*. Otherwise mapping
   collides with the rename path and produces duplicates like the one
   above.

2. **When schema-at-child has columns that don't exist at HEAD AND aren't
   covered by positional mapping**, skip those columns from the emit.
   The data is genuinely unrecoverable without a per-commit `AS OF`
   read; emitting the row without the missing column is the best we
   can do (might still violate NOT NULL constraints; in that case the
   test should skip with a documented gap).

A test case to add: source history where commit C2 drops a column that
existed at C1, then C3 adds rows that use the new schema. Replaying via
a doltlite intermediate where HEAD = post-C3 schema should still produce
correct row data for C1's rows on the dolt target.

### Why this matters

The 48/48 matrix is a strong unit-level contract but misses this class
of schema-evolution history. The bahaiwritings repo is a real-world
example where intermediate schemas diverge substantially from HEAD —
the writings table has had several columns added and dropped over its
482-commit history.

The fix is bounded in scope (improve `doltliteDiffSQL`'s column-slot
construction). Should be a single-tick change with a new test case
matching the bahaiwritings shape.

## What was committed during the wait

- `b441e39` — plan doc updated to 48/48 + workaround list.
- `f3f092b` — drop dead doltliteCreateAtCommit helper.
- `76c93a5` — KNOWN_ISSUES.md updated to reflect #10988 fix locally.

All pushed to `lapingvino/dolt-replay@main`.

## Files left for inspection

- `/tmp/bw-hop1.dl` — the doltlite intermediate from hop1 (1 GB, kept).
- `/tmp/bw-hop2-dst/` — partial dolt destination (only the early
  schema commits applied before failure).
- `/tmp/bahaiwritings-replay/{hop1.log, hop2.log, status.txt}` — full
  replay logs.
