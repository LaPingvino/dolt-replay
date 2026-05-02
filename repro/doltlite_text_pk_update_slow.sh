#!/bin/bash
# doltlite per-row UPDATE-by-PK is dramatically slower than SQLite when the
# primary key is TEXT (UUID-shaped) rather than INTEGER.
#
# Workload: M individual `UPDATE … WHERE pk = '…'` statements wrapped in one
# BEGIN/COMMIT, run via `.read script.sql`. Same script run against vanilla
# sqlite3 for comparison.
#
# Result on doltlite 0.9.1, sqlite 3.53 (linux x64):
#
#   N=30000, M=5000:
#     INTEGER PK : sqlite 0.031s   doltlite 0.107s    (~3x  )
#     TEXT PK    : sqlite 0.082s   doltlite 74.041s   (~900x)
#
# The TEXT-PK case ought to be O(log N) per UPDATE just like INTEGER. Instead
# doltlite spends ~15 ms per UPDATE — suggesting the prolly-tree mutmap path
# isn't using the index for TEXT keys, or is doing something pathological per
# row (full chunk-store traversal, key re-encoding, etc).
#
# Why this matters: any dolt repo with TEXT/UUID primary keys (the canonical
# choice for distributed setups) hits this on every per-row diff replay. It
# turned a 780-commit `dolt → doltlite` clone of a 1.4 GB working set from
# "tens of minutes" into "days," because every commit that runs `UPDATE
# writings SET … WHERE version='…'` against the 30k-row writings table
# replays as 30k single-row UPDATEs by TEXT PK.
set -e

DLITE=${DLITE:-doltlite}
SQLITE=${SQLITE:-sqlite3}
TMPD=$(mktemp -d)
trap "rm -rf $TMPD" EXIT

run_case() {
    local label="$1" pk_type="$2" n_rows="$3" n_updates="$4"

    case "$pk_type" in
        INTEGER)
            python3 - <<PYEOF > "$TMPD/seed.sql"
print("CREATE TABLE writings (id INTEGER PRIMARY KEY, type TEXT);")
print("BEGIN;")
for i in range(1, $n_rows + 1):
    print(f"INSERT INTO writings VALUES ({i}, 'prayer');")
print("COMMIT;")
PYEOF
            sqlite3 "$TMPD/setup.sqlite" ".read $TMPD/seed.sql"
            sqlite3 "$TMPD/setup.sqlite" "SELECT id FROM writings LIMIT $n_updates" > "$TMPD/pks.txt"
            python3 - <<PYEOF > "$TMPD/upd.sql"
print("BEGIN;")
with open("$TMPD/pks.txt") as f:
    for line in f:
        print(f"UPDATE writings SET type='hidden' WHERE id={line.strip()};")
print("COMMIT;")
PYEOF
            ;;
        TEXT)
            python3 - <<PYEOF > "$TMPD/seed.sql"
import random
random.seed(42)
print("CREATE TABLE writings (version TEXT PRIMARY KEY, type TEXT);")
print("BEGIN;")
for i in range(1, $n_rows + 1):
    uuid = f"{random.getrandbits(32):08x}-{i}"
    print(f"INSERT INTO writings VALUES ('uuid-{uuid}', 'prayer');")
print("COMMIT;")
PYEOF
            sqlite3 "$TMPD/setup.sqlite" ".read $TMPD/seed.sql"
            sqlite3 "$TMPD/setup.sqlite" "SELECT version FROM writings LIMIT $n_updates" > "$TMPD/pks.txt"
            python3 - <<PYEOF > "$TMPD/upd.sql"
print("BEGIN;")
with open("$TMPD/pks.txt") as f:
    for line in f:
        v = line.strip()
        print(f"UPDATE writings SET type='hidden' WHERE version='{v}';")
print("COMMIT;")
PYEOF
            ;;
    esac

    rm -f "$TMPD/setup.sqlite"
    echo "  --- $label : $pk_type PK, $n_rows-row table, $n_updates updates ---"

    rm -f "$TMPD/test.sqlite"
    $SQLITE "$TMPD/test.sqlite" ".read $TMPD/seed.sql"
    T0=$(date +%s.%N); $SQLITE "$TMPD/test.sqlite" ".read $TMPD/upd.sql" >/dev/null; T1=$(date +%s.%N)
    awk -v t0="$T0" -v t1="$T1" -v m="$n_updates" 'BEGIN { printf "    sqlite:   %7.3fs (%6.3fms/upd)\n", t1-t0, (t1-t0)*1000/m }'

    rm -f "$TMPD/test.dl"
    $DLITE "$TMPD/test.dl" ".read $TMPD/seed.sql" >/dev/null
    T0=$(date +%s.%N); $DLITE "$TMPD/test.dl" ".read $TMPD/upd.sql" >/dev/null; T1=$(date +%s.%N)
    awk -v t0="$T0" -v t1="$T1" -v m="$n_updates" 'BEGIN { printf "    doltlite: %7.3fs (%6.3fms/upd)\n", t1-t0, (t1-t0)*1000/m }'
}

echo "doltlite per-row UPDATE-by-PK perf: TEXT PK is ~900x slower than INTEGER PK"
echo "Versions:"
$DLITE -version 2>&1 | head -1 | sed 's/^/  doltlite: /' || true
$SQLITE -version | sed 's/^/  sqlite3:  /'

# Headline scenario
run_case "headline" INTEGER 30000 5000
run_case "headline" TEXT    30000 5000

# Scaling check (TEXT PK only — INTEGER PK is fast across the board)
echo
echo "Scaling: TEXT-PK with varying update counts"
for m in 100 500 2500 10000; do
    run_case "scaling-$m" TEXT 30000 $m
done
