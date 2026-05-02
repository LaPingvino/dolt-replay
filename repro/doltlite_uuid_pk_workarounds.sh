#!/bin/bash
set +e
DLITE=${DLITE:-doltlite}
TMPD=$(mktemp -d)
trap "rm -rf $TMPD" EXIT
N=30000
M=2000

bench() {
    local label="$1" seed="$2" upd="$3"
    rm -f "$TMPD/test.dl"
    $DLITE "$TMPD/test.dl" ".read $seed" >/dev/null
    T0=$(date +%s.%N); $DLITE "$TMPD/test.dl" ".read $upd" >/dev/null; T1=$(date +%s.%N)
    awk -v t0="$T0" -v t1="$T1" -v m="$M" -v label="$label" 'BEGIN { printf "  %-50s %7.3fs (%6.3fms/upd)\n", label, t1-t0, (t1-t0)*1000/m }'
}

echo "doltlite UUID PK workaround probe — $M UPDATEs against $N-row table"
echo

# 1) INTEGER PK column with 128-bit hex string values pushed in (pretend it's int)
python3 - <<PYEOF > "$TMPD/seed1.sql"
import random
random.seed(42)
print("CREATE TABLE t (pk INTEGER PRIMARY KEY, payload TEXT);")
print("BEGIN;")
for i in range(1, $N + 1):
    h = f"{random.getrandbits(128):032x}"
    print(f"INSERT INTO t VALUES ('{h}', 'x');")
print("COMMIT;")
PYEOF
python3 - <<PYEOF > "$TMPD/upd1.sql"
import random
random.seed(42)
all_pks = []
for i in range(1, $N + 1):
    all_pks.append(f"{random.getrandbits(128):032x}")
import random as r2
r2.seed(99)
chosen = r2.sample(all_pks, $M)
print("BEGIN;")
for pk in chosen:
    print(f"UPDATE t SET payload='y' WHERE pk='{pk}';")
print("COMMIT;")
PYEOF
bench "INTEGER PK col, 128-bit hex string values (lie)" "$TMPD/seed1.sql" "$TMPD/upd1.sql"

# 2) Composite (BIGINT hi, BIGINT lo) PK for UUID
python3 - <<PYEOF > "$TMPD/seed2.sql"
import random
random.seed(42)
print("CREATE TABLE t (pk_hi BIGINT, pk_lo BIGINT, payload TEXT, PRIMARY KEY (pk_hi, pk_lo));")
print("BEGIN;")
for i in range(1, $N + 1):
    v = random.getrandbits(128)
    hi = (v >> 64) - 2**63   # shift unsigned-128 → signed pair
    lo = (v & ((1<<64)-1)) - 2**63
    print(f"INSERT INTO t VALUES ({hi}, {lo}, 'x');")
print("COMMIT;")
PYEOF
python3 - <<PYEOF > "$TMPD/upd2.sql"
import random
random.seed(42)
all_pks = []
for i in range(1, $N + 1):
    v = random.getrandbits(128)
    hi = (v >> 64) - 2**63
    lo = (v & ((1<<64)-1)) - 2**63
    all_pks.append((hi, lo))
import random as r2
r2.seed(99)
chosen = r2.sample(all_pks, $M)
print("BEGIN;")
for hi, lo in chosen:
    print(f"UPDATE t SET payload='y' WHERE pk_hi={hi} AND pk_lo={lo};")
print("COMMIT;")
PYEOF
bench "Composite (BIGINT, BIGINT) PK for UUID hi/lo" "$TMPD/seed2.sql" "$TMPD/upd2.sql"

# 3) INTEGER PK with 64-bit truncated UUID (collision risk minimal at 30k rows)
python3 - <<PYEOF > "$TMPD/seed3.sql"
import random
random.seed(42)
print("CREATE TABLE t (pk INTEGER PRIMARY KEY, payload TEXT);")
print("BEGIN;")
for i in range(1, $N + 1):
    pk = random.getrandbits(63)
    print(f"INSERT INTO t VALUES ({pk}, 'x');")
print("COMMIT;")
PYEOF
python3 - <<PYEOF > "$TMPD/upd3.sql"
import random
random.seed(42)
all_pks = [random.getrandbits(63) for _ in range(1, $N + 1)]
import random as r2
r2.seed(99)
chosen = r2.sample(all_pks, $M)
print("BEGIN;")
for pk in chosen:
    print(f"UPDATE t SET payload='y' WHERE pk={pk};")
print("COMMIT;")
PYEOF
bench "INTEGER PK, 63-bit truncated UUID" "$TMPD/seed3.sql" "$TMPD/upd3.sql"
