# Replay Correctness Plan

**Survives compaction.** Resume from any section. The test suite is the source of truth — when in doubt, run it (`go test -run TestReplaySchema_ -v -timeout 5m`) and let the pass/fail/skip pattern tell you where we are.

## Goal

Make dolt-replay produce **faithful end-state replay** across all four src→dst combinations for arbitrary commit histories — including combined schema-and-data commits, the case dolthub/dolt#10988 surfaces. The test suite in `schema_replay_test.go` is the contract; every case becomes a test, and the implementation is whatever makes those tests green without regressing the existing ones.

The strong invariant: **if `dolt-replay` returns successfully, the target's final state is byte-equivalent to the source's final state for the replayed commits.**

## Test Matrix

Each row is a case category. Each column is a (src-kind, dst-kind) pair. Cell values: ✅ pass, ⏭ skip-with-known-gap, ❌ fail, — not yet implemented in the test suite.

| Case                | dlite→dlite | dlite→dolt | dolt→dlite | dolt→dolt |
|---------------------|:---:|:---:|:---:|:---:|
| Simple              | ✅ | ✅ | ✅ | ✅ |
| AddNullable (bahaiwritings) | ✅ | ✅ | ✅ | ✅ |
| AddWithDefault (case 1)     | ✅ | ✅ | ✅ | ✅ |
| DropThenAdd (case 2)        | ✅ | ✅ | ✅ | ✅ |
| DropOnly                    | ✅ | ✅ | ✅ | ✅ |
| RenameColumn                | ✅ | ✅ | ✅ | ✅ |
| TypeWidening                | ✅ | ✅ | ✅ | ✅ |
| TypeNarrowing (overflow)    | ✅ | ✅ | ✅ | ✅ |
| DropTable                   | ✅ | ✅ | ✅ | ✅ |
| CreateTableMidHistory       | ✅ | ✅ | ✅ | ✅ |
| MultiTable                  | ✅ | ✅ | ✅ | ✅ |
| RowOrderingPreserved        | ✅ | ✅ | ✅ | ✅ |

**48/48 cells green** as of commit 367929a. Round-trip chain tests
(`TestReplayChain_DoltDoltliteDolt`, `TestReplayChain_DoltliteDoltliteDoltlite`)
also pass — see commit cdcc1f6.

Update this table after each loop tick.

## Status: workarounds in place, upstream fixes filed

All twelve cases pass on the consumer side via the workarounds documented here. The corresponding upstream fixes are filed and reference the consumer-side implementation:

- **dolthub/dolt#10988** — silent-skip on combined schema-and-data commits. See [comment 4375376092](https://github.com/dolthub/dolt/issues/10988#issuecomment-4375376092) (consumer-side description) and [comment 4375380955](https://github.com/dolthub/dolt/issues/10988#issuecomment-4375380955) (upstream recommendation with worked examples + test spec).
- **dolthub/doltlite#738** — `dolt_schema_diff` errors `unknown operation` on DROP TABLE.
- **dolthub/doltlite#739** — `dolt_schema_diff` requires `table_name` filter to not error on certain change shapes.
- **dolthub/doltlite#740** — no `dolt_commit_ancestors` view + second-resolution dates make commit ordering ambiguous.

## Workarounds active in main.go

1. **`doltDiffSQLViaRebase`** (commit 367929a) — when `dolt diff -r sql` writes `Incompatible schema change, skipping data diff` to stderr, branch from parent into `__replay_baseline`, apply the schema delta there, then diff baseline→child for data. Closes the case 1/case 2 algorithm gap from #10988.

2. **Pragma-based DROP detector** (commit 8e06894) — when doltlite `dolt_schema_diff` errors `unknown operation` and the table doesn't appear in `pragma_table_info` at HEAD, emit `DROP TABLE` ourselves. Workaround for doltlite#738.

3. **Rename detection in `deriveAlterFromCreate`** (commit bf90969) — single-drop + single-add at same position with matching def is emitted as `ALTER TABLE ... RENAME COLUMN`, preventing data loss across the schema migration.

4. **Positional column mapping** (commit 5188a74) — when a schema-at-child column doesn't appear as `to_<col>` in the HEAD-aligned `dolt_diff_<table>` header (because the column was renamed before HEAD), fall back to position-matching against `pragma_table_info` at HEAD.

5. **Initialize-anchored walker** (commit cdcc1f6) — `doltliteLog` moves the `Initialize data repository` commit to position 0 regardless of date sort. Required for chain replays where a source commit's preserved date can pre-date the target's auto-init. Partial workaround for doltlite#740.

6. **`dliteCommitSep` test helper** (1.1s sleep between commits) — keeps test commits in distinct seconds. Workaround for doltlite#740.

7. **Empty-commit tolerance in `applyToDolt`** — table-rebuild patterns produce no semantic dolt diff; tolerate `nothing to commit` and `no changes added to commit`.

## The Correct Algorithm (nicktobey, dolthub/dolt#10988)

For each commit transition (parent A → child B, table T):

1. Make a temporary branch from A: A2 = A
2. Apply A→B's schema change to A2
3. Compute data diff between A2 and B for table T (now both have the same schema, so the diff is purely semantic)
4. Replay onto target: emit the schema change first, then the data diff

This is what handles cases 1 and 2 cleanly:
- Case 1 (ADD COLUMN DEFAULT 6): after step 2, A2 has rows populated with c=6 too. Diff with B is empty for the schema change's effect; only later real data updates show.
- Case 2 (DROP a + ADD b, then UPDATE b=10): after step 2, A2 has the new schema with b=NULL. Diff with B (b=10) shows the UPDATE explicitly.

Required primitives:
- **doltlite source**: branch + apply ALTER + diff via `dolt_diff_<table>`. doltlite has `dolt_branch`, `dolt_checkout`, and the diff system tables. The branch+apply step mutates the source repo, which we'd want to do on a throwaway temp clone (or use a transient branch and clean it up).
- **dolt source**: same algorithm, requires shelling out `dolt branch / dolt checkout / dolt sql` against a temp clone.

## Implementation Strategy (Per Loop Tick)

Each tick:

1. **Pick** the next failing or missing test case. Use the matrix above; prefer columns of completeness (fill out a row across directions before opening a new row).
2. **Run** the targeted test. Read the failure mode. Don't guess.
3. **Implement** the minimal fix in main.go. Either:
   - Source-side: extend doltDiffSQL or doltliteDiffSQL
   - Target-side: extend applyToDolt or applyToDoltlite
   - Or add a primitive (a new helper, a new system-table query)
4. **Verify**: rerun the targeted test + the full schema_replay suite (catch regressions).
5. **Commit** if green. Skip with explicit reason if a deeper algorithm fix is needed.
6. **Update** this plan's matrix.
7. **Schedule** next tick (or stop if matrix is acceptably full).

Discipline: one-commit-per-green-build. Never leave the tree red between ticks. If a fix takes more than one tick, the intermediate states still build clean (commits guarded behind feature flags or branches).

## Key Files

- `main.go` — extraction (`doltDiffSQL`, `doltliteDiffSQL`) + application (`applyToDolt`, `applyToDoltlite`) + dialect translation
- `schema_replay_test.go` — directed test suite (this is the contract)
- `main_test.go` — older e2e tests (TestE2E_*) using dolt-source fixtures; check for overlap when adding cases here
- `bench_test.go` — INT-PK perf benches; orthogonal to correctness work
- `DOLTLITE_REWRITE_PLAN.md` — separate, older plan for a related effort; don't conflate

## Mechanical Conventions

- **Test source repos**: use `t.TempDir()`. For doltlite source, run `SELECT 1;` once to trigger the auto Initialize commit before any explicit commits.
- **doltlite commit separation**: 1.1s sleep between commits (`dliteCommitSep(t)` helper). Until the parent-walker is rewritten, this is non-negotiable.
- **dolt-target dates**: must be RFC3339. `normalizeDateForDolt()` handles the conversion.
- **dolt-target authors**: skip `--author` when email is empty; let dolt's config win.
- **Building dolt-replay in tests**: `requireDoltliteOnly(t)` / `requireDoltliteAndDolt(t)` build a fresh binary per test; tests are hermetic.
- **PATH**: tests rely on `dolt` and `doltlite` being on PATH. CI/local runs need to set this.

## Resume Steps After Compaction

1. `cd /home/joop/prayermatching/dolt-replay && git status` — confirm branch
2. `git log --oneline -10` — see what landed since this plan was last updated
3. `go test -run TestReplaySchema_ -v -timeout 5m` — see current pass/skip/fail
4. Compare actual against the matrix above. The matrix is hand-maintained; trust the test runner if they disagree, then update the matrix.
5. Pick next tick from the matrix.

## Cross-Reference

- Upstream issue: https://github.com/dolthub/dolt/issues/10988
- Our prototype commit: https://github.com/lapingvino/dolt-replay/commit/0b4a8904a03bb1c16a779e7a36807d9de5128a1f
- Our followup with the test suite: https://github.com/lapingvino/dolt-replay/commit/a9a7bbb (HEAD as of this writing)
