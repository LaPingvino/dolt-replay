# Doltlite mutmap-flush rewrite — comprehensive plan

**Survives compaction.** If the current session restarts, read this top-to-bottom
and resume at whichever checkbox is unchecked.

## Mission

Bring **natural parity between INT and non-INT primary keys** in doltlite's
prolly tree, matching Dolt's `K ~[]byte, O Ordering[K]` design. Tim:
*"There is no reason that int keys should be treated differently than other keys."*

User's gate: don't bench or claim perf wins until the algorithm is structurally
unified. Slower-but-aligned ≫ faster-but-special-cased.

## Source trees

- **doltlite**: `/tmp/doltlite_src` (git, branch `joop/unify-flush-strategy`,
  pushed to `fork` remote = `git@github.com:LaPingvino/doltlite.git`).
- **Dolt** (reference): `/tmp/doltrepo` (sparse-checkout, only `go/store/prolly/`).
- **Bench harness**: `/home/joop/prayermatching/dolt-replay/bench_test.go` plus
  `/tmp/bench_partial.sh`, `/tmp/bench_intpk.sh`, `/tmp/bench_with_index.sh`.

## Verifying parity (run this anytime)

```bash
# Fast: takes ~5 seconds
cd /tmp/parity && rm -f parity_*.dl && N=10000
# (the canonical script lives at .../bench_test.go::TestNaturalParity once written;
# meanwhile use the inline form from the previous session log)
```

Current verdict (baseline = `joop/unify-flush-strategy` branch):
```
file size (10k rows):      INT 200KB  vs  TEXT 45MB    (222× gap)
startup (10k rows):        INT 11ms   vs  TEXT 151ms   (14× gap)
1000-update commit:        INT 33ms   vs  TEXT 274ms   (8× gap)
isIntKey sites in C:       54
isIntKey sites in Dolt Go: 0
```

Goal: storage gap → ≤2× (data content), perf gap → ≤2×, isIntKey count → 0.

## Reference: how Dolt achieves parity

Dolt (`/tmp/doltrepo/go/store/prolly/tree/`) has **zero** IntKey branches.
The whole tree layer is generic over `K ~[]byte` plus `Ordering[K]`:

```go
// node_cursor.go:42
type Ordering[K ~[]byte] interface {
    Compare(ctx context.Context, left, right K) int
}
```

INT keys are 8-byte big-endian byte arrays with a comparator that does
i64 comparison. TEXT keys are variable-length bytes with a string comparator.
Same structural code, same chunker, same on-disk format — only the comparator
and the byte content differ.

## Doltlite's current divergence (54 sites)

| File | sites | what they do |
|------|---:|---|
| prolly_mutmap.c | 17 | Separate storage path: INT entries inline `i64`, BLOB entries `sqlite3_malloc` the key bytes |
| prolly_mutate.c | 12 | At every comparison site: `if INTKEY encode i64 to 8-byte buf else use byte key` |
| prolly_diff.c | 11 | Same pattern as mutate.c, in diff path |
| prolly_btree.c | 6 | Threading `isIntKey` flag through API calls |
| prolly_node.c | 3 | Format split: `prollyNodeIntKey()` reads i64; otherwise `prollyNodeKey()` reads bytes |
| prolly_mutmap.h | 3 | Header types/sigs that take `isIntKey` |
| prolly_node.h | 1 | Format flags (PROLLY_NODE_INTKEY vs PROLLY_NODE_BLOBKEY) |
| prolly_three_way_diff.c | 1 | Trivial passthrough |

## Phased porting plan

### Phase 0 — preparation (DONE)

- [x] Read & summarize doltlite mutate stack
- [x] Read & summarize Dolt's tree/mutator + chunker
- [x] PR #729 (drop strategy heuristic) — structural-but-not-perf cleanup
- [x] Verify correctness parity (functional behavior identical for INT/TEXT)
- [x] Establish "natural parity" measurable verdict
- [x] Document scope

### Phase 1 — unify mutmap STORAGE (foundational, in progress)

**Goal**: `prolly_mutmap.c` stores all keys as `(ptr, len)` byte arrays. INT
keys get encoded once at insert time (8-byte big-endian) and stored as bytes.
Drop the `isIntKey` parameter and the inline `i64 intKey` field.

**Files touched**: `prolly_mutmap.c`, `prolly_mutmap.h`, callers in
`prolly_btree.c` and `prolly_mutate.c`.

**Approach**:
1. Add `prollyEncodeIntKey(i64) → 8 bytes` helper (or use existing `encodeI64BE`).
2. In `prollyMutMapInit`, drop the `isIntKey` parameter. Mutmap is comparator-agnostic.
3. In `prollyMutMapInsert/Find/etc`, drop the `intKey` overload — always take byte key.
4. Callers that have an `i64`: encode to 8-byte buffer, pass.
5. Comparator: `prollyCompareKeys` already handles both via flags — for now keep
   that (changing comparator is Phase 2). Mutmap just stores bytes; comparison
   happens via the function pointer/flags supplied by caller.

**Risk**: mutmap is performance-critical and called from many sites. Must verify
no functional regression via `dolt-replay`'s E2E suite + a doltlite-level smoke
test (basic CRUD + a few updates).

**Steps**:
- [ ] Read `prolly_mutmap.c` end-to-end (haven't yet — just the storage section)
- [ ] Sketch the new struct layout
- [ ] Patch storage (entry struct, copyEntryData, freeEntryData, hashKey)
- [ ] Patch lookup (compareEntries, findInOrder, etc.)
- [ ] Patch init/free signatures (drop isIntKey from public API)
- [ ] Patch all callers in prolly_btree.c, prolly_mutate.c
- [ ] Build clean, run dolt-replay E2E
- [ ] Re-run parity check — expect storage gap unchanged (this phase is in-memory only),
      expect functional correctness preserved, expect possibly slight perf change

### Phase 2 — unify on-disk node FORMAT

**Goal**: `prolly_node.c/h` represent INT keys as 8-byte big-endian byte arrays
in the chunk format, dropping `PROLLY_NODE_INTKEY` flag. The chunker writes one
format. The `PROLLY_NODE_INTKEY` flag becomes an in-memory hint for which
comparator to use (or even drops entirely — comparator becomes part of the
table metadata).

**Files touched**: `prolly_node.c/h`, `prolly_chunker.c` (boundary fn), all
readers (`prolly_cursor.c`, `prolly_diff.c`).

**Risk**: Format change. Existing doltlite databases would not be readable.
This is a deal-breaker for production but acceptable for the unification port
if Tim accepts it. Mitigation: detect old format on open and translate, OR
require an explicit migration step.

**Steps** (left for after Phase 1 is solid):
- [ ] Audit prolly_node.c for INT-specific encoding
- [ ] Decide on migration strategy with Tim
- [ ] Patch encode/decode to one format
- [ ] Update chunker boundary fn (currently may use intKey directly?)
- [ ] Update readers in prolly_cursor.c, prolly_diff.c

### Phase 3 — unify chunker boundary function

**Goal**: chunker boundary decisions are based on bytes alone. Currently the
TEXT-PK chunker is producing wildly more chunks than INT-PK (root cause of
222× file-size blow-up). Investigate `prolly_chunker.c`'s boundary check and
ensure it's not producing too many splits for high-entropy text keys.

**This is where the actual storage parity lands.** The 222× gap will only
close once the chunker stops over-splitting on text keys.

**Files touched**: `prolly_chunker.c::prollyWeibullCheck` and the boundary
selection logic.

**Steps**:
- [ ] Read prollyWeibullCheck — what does it do per-key?
- [ ] Compare with Dolt's chunker boundary fn
- [ ] Identify why high-entropy keys split more often
- [ ] Patch to make boundary independent of key entropy

### Phase 4 — drop comparator divergence (final cleanup)

- [ ] Audit remaining isIntKey/PROLLY_NODE_INTKEY references
- [ ] Replace with comparator function pointer in table metadata
- [ ] Remove the flag entirely

## Build/test loop

```bash
# Build
cd /tmp/doltlite_src && make doltlite 2>&1 | tail -3

# Smoke
rm -f /tmp/smoke.dl
./doltlite /tmp/smoke.dl "CREATE TABLE t (pk TEXT PRIMARY KEY, v TEXT); INSERT INTO t VALUES ('a','1'),('b','2'); SELECT * FROM t ORDER BY pk;"

# Use as bench/E2E binary
cp doltlite /tmp/dl_phase1
cd /home/joop/prayermatching/dolt-replay
PATH=/tmp/dlphase1bin:$PATH go test -run='TestE2E|TestDoltliteSession' -timeout=300s ./...
# (where /tmp/dlphase1bin/doltlite is a symlink to /tmp/dl_phase1)

# Parity check
[run the script from "Verifying parity" section above]
```

## Decision log

- **2026-05-03**: PR #729 opened against `fix/non-intkey-streaming-merge`.
  Originally claimed 100× perf win — was wrong, conflated forced-mergeWalk vs
  forced-streaming with actual workload routing. PR description corrected to
  reflect honest measurement. PR is structural cleanup, not perf fix.
- **2026-05-03**: User's gate clarified — natural parity (algorithm same
  regardless of key type) before any further perf work. 54 isIntKey sites
  must drop to 0.
- **2026-05-03**: Starting Phase 1 (mutmap storage unification) as the
  foundational piece. Other phases depend on this.

## Recovery hints (if compaction wipes context)

- The `joop/unify-flush-strategy` branch in `/tmp/doltlite_src` has PR #729 already.
- The `joop/unify-mutmap-storage` branch exists, branched off `joop/unify-flush-strategy`,
  CURRENTLY HAS NO COMMITS. That's where Phase 1 work goes.
- The `force-streaming` and `force-mergeWalk` binaries (`/tmp/dl_streaming`,
  `/tmp/dl_mergewalk`) are stale — discard them; build fresh from current branch.
- The bench harness at `bench_test.go` has good patterns; gate with
  `DOLTLITE_BENCH=1` and `DLITE=...` env vars.
- If you don't remember the parity numbers, re-run the script in
  "Verifying parity" — takes ~5 seconds.

## EXACT next concrete step (resume here)

You're partway into Phase 1 (mutmap storage unification). Branch is
`joop/unify-mutmap-storage`, no commits yet. The plan:

**Goal**: drop `isIntKey` from `ProllyMutMap` struct + drop `intKey` from
`ProllyMutMapEntry` struct. INT keys get encoded to 8-byte big-endian bytes
at insert time. All entries store keys as `(pKey, nKey)` byte arrays only.

**Files to change in this phase**:
1. `src/prolly_mutmap.h` — drop `intKey` field from `ProllyMutMapEntry`,
   drop `isIntKey` field from `ProllyMutMap`, drop `isIntKey` from `Init/InitMode`,
   drop `intKey` from `Insert/Delete/Find/FindRc/IterSeek` signatures
2. `src/prolly_mutmap.c` — drop `isIntKey` and `intKey` parameters from
   `compareEntries`, `hashKey`, `bsearch_key`, `hashEntryMatches`,
   `findPhysLazy`, `cmpInOrder`. Drop the conditional storage path in
   `copyEntryData`. Hash always over bytes. Compare always over bytes.
3. Callers in `src/prolly_btree.c` — every `prollyMutMapInsert/.../Delete/Find`
   call. There are ~50. Where they pass `intKey != 0`, encode i64 → 8 bytes
   first (`u8 buf[8]; encodeI64BE(buf, intKey); ... pKey=buf, nKey=8`).
   Where they have a real byte key, leave alone but drop the trailing `0`
   intKey arg.
4. Callers in `src/prolly_mutate.c` — same pattern, ~12 sites.
5. Callers in `src/prolly_diff.c` — same pattern, ~11 sites.

**Tactical advice for the C porter (LLM or human)**:
- DON'T try to do all 5 files in one commit. One file per commit, build between.
- DO the .h FIRST (will break the build) — then fix mutmap.c — then callers.
- For the integer-key encoding, there's already an `encodeI64BE` helper in
  `prolly_node.c` (used at lines 280-281 of original prolly_mutate.c). Reuse it.
- The hashKey function had a special INT path for hashing the i64 directly
  without allocating a buffer; in the unified version, the 8-byte buffer is
  already on the stack at the call site, so just hash those 8 bytes.

**After Phase 1 lands**: run dolt-replay E2E suite (must stay green). Run
parity check (storage gap should not change — this is in-memory unification
only). Commit, push to fork, prepare PR or stack on PR #729.

**After Phase 1 lands and is verified, Phase 2** (unifying on-disk node format)
is the next big lift. That's where the storage 222× gap actually closes.
