#!/bin/bash
# Bisect: when did doltlite's TEXT-PK UPDATE workload become slow?
set -e

CACHE=/tmp/dl_bisect_textpk
mkdir -p "$CACHE"
TMPD=$(mktemp -d)
trap "rm -rf $TMPD" EXIT

N=10000
M=1000

# Smaller workload so even slow versions complete in reasonable time
python3 - <<PYEOF > "$TMPD/seed.sql"
import random
random.seed(42)
print("CREATE TABLE t (pk TEXT PRIMARY KEY, payload TEXT);")
print("BEGIN;")
for i in range(1, $N + 1):
    print(f"INSERT INTO t VALUES ('uuid-{random.getrandbits(32):08x}-{i}', 'x');")
print("COMMIT;")
PYEOF

# Pre-build the UPDATE script using sqlite to read PKs (so all versions act on the same keys)
sqlite3 "$TMPD/setup.sqlite" ".read $TMPD/seed.sql"
sqlite3 "$TMPD/setup.sqlite" "SELECT pk FROM t LIMIT $M" > "$TMPD/pks.txt"
{
  echo "BEGIN;"
  awk '{print "UPDATE t SET payload=" "'"'"'y'"'"'" " WHERE pk=" "'"'"'" $1 "'"'"';"}' "$TMPD/pks.txt"
  echo "COMMIT;"
} > "$TMPD/upd.sql"

echo "Bisect: $M TEXT-PK UPDATEs against $N-row table"
echo

for VER in v0.4.0 v0.5.0 v0.6.0 v0.7.0 v0.7.6 v0.8.0 v0.8.2 v0.9.0 v0.9.1; do
    BIN="$CACHE/$VER/doltlite"
    if [ ! -x "$BIN" ]; then
        mkdir -p "$CACHE/$VER"
        ASSET=doltlite-tools-linux-x64-${VER#v}.zip
        URL="https://github.com/dolthub/doltlite/releases/download/$VER/$ASSET"
        echo "  fetching $VER..." >&2
        curl -sL -o "$CACHE/$VER/asset.zip" "$URL" || { echo "    no asset"; continue; }
        unzip -qoj "$CACHE/$VER/asset.zip" "*/doltlite" -d "$CACHE/$VER" 2>/dev/null \
            || unzip -qoj "$CACHE/$VER/asset.zip" "doltlite" -d "$CACHE/$VER" 2>/dev/null \
            || { echo "    no doltlite binary in $ASSET"; continue; }
        chmod +x "$BIN"
    fi
    rm -f "$TMPD/test.dl"
    if ! "$BIN" "$TMPD/test.dl" ".read $TMPD/seed.sql" >/dev/null 2>&1; then
        printf "  %-8s SEED FAILED\n" "$VER"
        continue
    fi
    T0=$(date +%s.%N)
    if ! timeout 600 "$BIN" "$TMPD/test.dl" ".read $TMPD/upd.sql" >/dev/null 2>&1; then
        T1=$(date +%s.%N)
        printf "  %-8s TIMEOUT (>%.0fs)\n" "$VER" "$(echo "$T1-$T0" | bc)"
        continue
    fi
    T1=$(date +%s.%N)
    awk -v t0="$T0" -v t1="$T1" -v m="$M" -v ver="$VER" 'BEGIN { printf "  %-8s %7.3fs (%6.3fms/upd)\n", ver, t1-t0, (t1-t0)*1000/m }'
done
