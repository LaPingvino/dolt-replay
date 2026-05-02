#!/bin/bash
# Probe whether the doltlite TEXT-PK slowness depends on the *type label*
# (TEXT vs CHAR(N) vs BLOB vs INTEGER), the *value content* (UUID-shaped
# vs short hex vs same hex content as integer), or specifically the use
# of TEXT keys.
set -e

DLITE=${DLITE:-doltlite}
TMPD=$(mktemp -d)
trap "rm -rf $TMPD" EXIT

N=30000
M=2000   # smaller to keep total runtime sane

run_one() {
    local label="$1" pk_decl="$2" gen_pk="$3"

    python3 - <<PYEOF > "$TMPD/seed.sql"
import random
random.seed(42)
print("CREATE TABLE t (pk $pk_decl PRIMARY KEY, payload TEXT);")
print("BEGIN;")
for i in range(1, $N + 1):
    pk = $gen_pk
    if isinstance(pk, str):
        print(f"INSERT INTO t VALUES ('{pk}', 'x');")
    else:
        print(f"INSERT INTO t VALUES ({pk}, 'x');")
print("COMMIT;")
PYEOF

    python3 - <<PYEOF > "$TMPD/upd.sql"
import random
random.seed(42)
print("BEGIN;")
pks = []
for i in range(1, $N + 1):
    pks.append($gen_pk)
import random as r2
r2.seed(99)
chosen = r2.sample(pks, $M)
for pk in chosen:
    if isinstance(pk, str):
        print(f"UPDATE t SET payload='y' WHERE pk='{pk}';")
    else:
        print(f"UPDATE t SET payload='y' WHERE pk={pk};")
print("COMMIT;")
PYEOF

    rm -f "$TMPD/test.dl"
    $DLITE "$TMPD/test.dl" ".read $TMPD/seed.sql" >/dev/null
    T0=$(date +%s.%N); $DLITE "$TMPD/test.dl" ".read $TMPD/upd.sql" >/dev/null; T1=$(date +%s.%N)
    awk -v t0="$T0" -v t1="$T1" -v m="$M" -v label="$label" 'BEGIN { printf "  %-40s %7.3fs (%6.3fms/upd)\n", label, t1-t0, (t1-t0)*1000/m }'
}

echo "doltlite TEXT-PK slowness probe — $M UPDATE-by-PK against $N-row table, BEGIN/COMMIT"
echo "Versions: $($DLITE -version 2>&1 | head -1)"
echo
echo "PK type / value variants:"
run_one "INTEGER PK / int values"         "INTEGER" "i"
run_one "INTEGER PK / large int values"   "INTEGER" "(random.getrandbits(63))"
run_one "TEXT PK / short int-as-str"      "TEXT"    "str(i)"
run_one "TEXT PK / 8-hex-chars only"      "TEXT"    "f'{random.getrandbits(32):08x}'"
run_one "TEXT PK / 32-hex-chars (md5len)" "TEXT"    "f'{random.getrandbits(128):032x}'"
run_one "TEXT PK / UUID-shaped"           "TEXT"    "f'uuid-{random.getrandbits(32):08x}-{i}'"
run_one "CHAR(36) PK / UUID-shaped"       "CHAR(36)" "f'uuid-{random.getrandbits(32):08x}-{i}'"
run_one "VARCHAR(255) PK / UUID-shaped"   "VARCHAR(255)" "f'uuid-{random.getrandbits(32):08x}-{i}'"
run_one "BLOB PK / UUID bytes"            "BLOB"    "(random.getrandbits(128).to_bytes(16,'big').hex())"
