#!/bin/bash
# Reproduces a doltlite v0.9.0 bug where bulk INSERT VALUES via `.read`
# silently drops rows past ~2000 in a multi-column table.
#
# Setup:
#   doltlite from https://github.com/dolthub/doltlite
#   Tested on: DoltLite dev (SQLite 3.54.0, 64-bit), build 0.9.0.r121.gf4daf9279
#
# Expected: 5000 rows after each scenario.
# Actual:   ~167 rows in scenario A; 5000 in scenario B (single-column).
set -e

DB1=$(mktemp --suffix=.dl); rm -f "$DB1"
DB2=$(mktemp --suffix=.dl); rm -f "$DB2"
SQL=$(mktemp --suffix=.sql)
trap 'rm -f "$DB1" "$DB2" "$SQL"' EXIT

echo "=== Scenario A: 5000 wide-row INSERTs via .read (BEGIN/COMMIT wrapped) ==="
doltlite "$DB1" "CREATE TABLE t (
  a INTEGER NOT NULL,
  b INTEGER NOT NULL,
  c INTEGER,
  d INTEGER,
  e TEXT,
  PRIMARY KEY (a, b)
);" >/dev/null

{
  echo "BEGIN;"
  for i in $(seq 1 5000); do
    echo "INSERT INTO t (a,b,c,d,e) VALUES ($i, $i, $i, $i, NULL);"
  done
  echo "COMMIT;"
} > "$SQL"

doltlite -bail "$DB1" -cmd ".read $SQL" "SELECT dolt_commit('-A','-m','5k');" >/dev/null
N=$(doltlite "$DB1" "SELECT COUNT(*) FROM t" 2>&1)
echo "Scenario A row count: $N (expected 5000)"

echo
echo "=== Scenario B: 5000 single-column INSERTs via INSERT...SELECT generate_series ==="
doltlite "$DB2" "CREATE TABLE t (i INTEGER PRIMARY KEY);" >/dev/null
doltlite "$DB2" "INSERT INTO t SELECT value FROM generate_series(1, 5000); SELECT dolt_commit('-A','-m','5k');" >/dev/null
M=$(doltlite "$DB2" "SELECT COUNT(*) FROM t" 2>&1)
echo "Scenario B row count: $M (expected 5000)"

echo
if [ "$N" != "5000" ]; then
  echo "BUG REPRODUCED: scenario A persisted only $N of 5000 rows."
  exit 1
fi
echo "OK — bug not reproduced (may be fixed upstream)."
