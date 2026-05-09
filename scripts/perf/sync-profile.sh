#!/usr/bin/env bash
# Capture a CPU + heap profile of cmd/ghive-sync over a self-replay
# window. See docs/perf/sync-profile.md for a written-up baseline.
#
# Usage:
#   scripts/perf/sync-profile.sh [BLOCKS]
#
# BLOCKS defaults to 200000 (~75 seconds wall on a fast box).

set -euo pipefail

BLOCKS=${1:-200000}
ROOT=$(cd "$(dirname "$0")/../.." && pwd)
SRC=${SRC:-/home/martin/projects/hived-go/block_log}
OUT=${OUT:-/tmp/perf-profile}
BIN=$ROOT/bin/ghive-sync

mkdir -p "$ROOT/bin"
go build -o "$BIN" ./cmd/ghive-sync

rm -rf "$OUT" "$OUT.pprof" "$OUT.mem"
mkdir -p "$OUT"

echo "[+] capturing CPU profile over $BLOCKS blocks ..."
"$BIN" \
  --data-dir="$OUT" --backend=mem \
  --source-block-log="$SRC" \
  --max-blocks="$BLOCKS" \
  --cpuprofile="$OUT.pprof" \
  --memprofile="$OUT.mem" \
  > "$OUT.log" 2>&1

echo "[+] done. CPU=$OUT.pprof  HEAP=$OUT.mem  LOG=$OUT.log"
echo
echo "Top-30 cum:"
go tool pprof -top -cum -nodecount=30 "$OUT.pprof" | head -45
echo
echo "Top-30 alloc_space:"
go tool pprof -top -alloc_space -nodecount=30 "$OUT.mem" | head -45
