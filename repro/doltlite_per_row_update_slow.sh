#!/bin/bash
# Reproduces doltlite's per-row UPDATE-in-transaction slowness vs SQLite.
#
# Setup:  one table with N existing rows.
# Test:   M individual UPDATE-by-PK statements wrapped in BEGIN/COMMIT.
# Result: doltlite is ~50-200x slower than SQLite at this workload.
#
# Why this matters: `dolt diff -r sql` represents an UPDATE that touches every
# row of a 30k-row table as 30k individual UPDATE statements (one per PK).
# Replaying that diff via doltlite's `.read` takes minutes where SQLite takes
# fractions of a second, even with a single wrapping transaction.
#
# Tested against: doltlite 0.9.1 (release tools binary).
set -e

DLITE=${DLITE:-doltlite}
SQLITE=${SQLITE:-sqlite3}

TMPD=$(mktemp -d)
trap "rm -rf $TMPD" EXIT

mk_table_and_seed_inserts() {
    local n_rows=$1
    cat <<EOF
CREATE TABLE writings (
    id INTEGER PRIMARY KEY,
    phelps TEXT,
    language TEXT,
    type TEXT
);
EOF
    echo "BEGIN;"
    awk -v n="$n_rows" 'BEGIN { for (i=1; i<=n; i++) printf "INSERT INTO writings VALUES (%d, '\''BH%d'\'', '\''en'\'', '\''prayer'\'');\n", i, i }'
    echo "COMMIT;"
}

mk_per_row_updates() {
    local n_updates=$1
    echo "BEGIN;"
    awk -v n="$n_updates" 'BEGIN { for (i=1; i<=n; i++) printf "UPDATE writings SET type = '\''hidden_word'\'' WHERE id = %d;\n", i }'
    echo "COMMIT;"
}

bench() {
    local label=$1 cmd=$2 db=$3 sql=$4
    local t0 t1
    t0=$(date +%s.%N)
    $cmd "$db" -bail -cmd ".read $sql" >/dev/null 2>&1 || $cmd "$db" ".read $sql" >/dev/null 2>&1
    t1=$(date +%s.%N)
    awk -v t0="$t0" -v t1="$t1" -v label="$label" 'BEGIN { printf "  %-22s %.3fs\n", label, t1-t0 }'
}

run_scenario() {
    local n_rows=$1 n_updates=$2

    # Fresh seed file
    mk_table_and_seed_inserts $n_rows > $TMPD/seed.sql
    mk_per_row_updates $n_updates > $TMPD/upd.sql

    echo
    echo "=== Table size: $n_rows rows | Workload: $n_updates per-row UPDATEs in BEGIN/COMMIT ==="

    # SQLite
    rm -f $TMPD/test.sqlite
    $SQLITE $TMPD/test.sqlite ".read $TMPD/seed.sql" >/dev/null
    bench "sqlite3" $SQLITE $TMPD/test.sqlite $TMPD/upd.sql

    # doltlite
    rm -f $TMPD/test.dl
    $DLITE $TMPD/test.dl ".read $TMPD/seed.sql" >/dev/null
    bench "doltlite" $DLITE $TMPD/test.dl $TMPD/upd.sql
}

echo "doltlite per-row UPDATE inside BEGIN/COMMIT — perf comparison vs sqlite"
echo "Versions:"
$DLITE -version 2>&1 | head -1 | sed 's/^/  doltlite: /' || true
$SQLITE -version 2>&1 | head -1 | sed 's/^/  sqlite3:  /' || true

# Scaling: hold rows constant, vary updates
for upd in 100 1000 5000; do
    run_scenario 30000 $upd
done

# Scaling: hold updates constant, vary rows
for rows in 1000 10000 100000; do
    run_scenario $rows 1000
done
