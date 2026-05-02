#!/bin/bash
# git bisect test script — exit 0 if doltlite TEXT-PK UPDATE is FAST, 1 if SLOW.
# Threshold: 1.5 ms/upd (v0.9.0 was 0.385, v0.9.1 is 3.989, so 1.5 is mid).
set +e
cd /tmp/doltlite_src/build || exit 125

# Rebuild incrementally
make -s doltlite -j$(nproc) >/tmp/build.log 2>&1
if [ ! -x ./doltlite ]; then
    echo "BUILD FAILED at $(cd /tmp/doltlite_src && git rev-parse --short HEAD)" >&2
    tail -20 /tmp/build.log >&2
    exit 125  # skip — broken commit
fi

TMPD=$(mktemp -d)
trap "rm -rf $TMPD" EXIT
N=10000; M=1000

python3 - <<PYEOF > "$TMPD/seed.sql"
import random
random.seed(42)
print("CREATE TABLE t (pk TEXT PRIMARY KEY, payload TEXT);")
print("BEGIN;")
for i in range(1, $N + 1):
    print(f"INSERT INTO t VALUES ('uuid-{random.getrandbits(32):08x}-{i}', 'x');")
print("COMMIT;")
PYEOF

# Stable PK list
sqlite3 "$TMPD/setup.sqlite" ".read $TMPD/seed.sql"
sqlite3 "$TMPD/setup.sqlite" "SELECT pk FROM t LIMIT $M" > "$TMPD/pks.txt"
{
    echo "BEGIN;"
    awk '{print "UPDATE t SET payload='\''y'\'' WHERE pk='\''"$1"'\'';"}' "$TMPD/pks.txt"
    echo "COMMIT;"
} > "$TMPD/upd.sql"

rm -f "$TMPD/test.dl"
./doltlite "$TMPD/test.dl" ".read $TMPD/seed.sql" >/dev/null 2>&1
T0=$(date +%s.%N)
timeout 120 ./doltlite "$TMPD/test.dl" ".read $TMPD/upd.sql" >/dev/null 2>&1
T1=$(date +%s.%N)
DT=$(awk -v t0="$T0" -v t1="$T1" 'BEGIN{print (t1-t0)*1000/'"$M"'}')
SHA=$(cd /tmp/doltlite_src && git rev-parse --short HEAD)
SUBJ=$(cd /tmp/doltlite_src && git log -1 --format=%s)
printf "%s  %s  ms/upd=%.3f  %s\n" "$SHA" "$(printf '%6.3f' "$DT")" "$DT" "$SUBJ" | tee -a /tmp/bisect_log.txt >&2

# 1.5 ms/upd threshold; exit 0 if fast (good), 1 if slow (bad)
awk -v dt="$DT" 'BEGIN { exit (dt > 1.5) ? 1 : 0 }'
