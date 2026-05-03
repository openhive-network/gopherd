#!/bin/bash
# Build a hived data dir whose block_log is *truncated* at the given
# block, so that hived's consistency check (state head == block_log
# head) passes when API mode starts.
#
# This is the only path I've found that lets hived serve API queries
# at an arbitrary past block WITHOUT recompiling hived (banned per the
# project rule).
#
# Usage: hived-prepare-checkpoint.sh <block_num>
# Result: /tmp/hived-cp-<N>/ ready for hived-serve-checkpoint.sh.
set -euo pipefail
BLOCK="${1:?usage: $0 <block_num>}"
SRC_DIR="${HIVED_DATA_DIR:-/tmp/hived-genesis}"
CP_DIR="/tmp/hived-cp-${BLOCK}"
HIVED_BIN="${HIVED_BIN:-/home/martin/projects/hive/build/programs/hived/hived}"
UTIL_BIN="${HIVED_UTIL_BIN:-/home/martin/projects/hive/build/programs/util/block_log_util}"

mkdir -p "$CP_DIR/blockchain"

# Copy the base config (plugin list etc.) so hived starts cleanly under $CP_DIR.
cp /tmp/cp-base-config.ini "$CP_DIR/config.ini"

# 1. Copy the *split* block_log parts and artifacts. Reusing them lets
#    us avoid recompressing the chain. Truncate operates on a *single*
#    monolithic block_log, so we'll merge them next.
echo "[1/4] Copy split block_log into $CP_DIR..."
cp -n "$SRC_DIR"/blockchain/block_log_part.* "$CP_DIR/blockchain/" 2>/dev/null || true

# 2. Merge to a monolithic block_log capped at $BLOCK so truncate has
#    something to work with. merge-block-logs writes to its --output dir.
echo "[2/4] Merge split parts up to block $BLOCK..."
"$UTIL_BIN" --merge-block-logs \
  -i "$CP_DIR/blockchain" \
  -o "$CP_DIR/blockchain" \
  -n "$BLOCK"

# 3. Drop split parts now that the merged file exists; otherwise hived
#    refuses to open both forms in the same dir.
rm -f "$CP_DIR"/blockchain/block_log_part.* "$CP_DIR"/blockchain/block_log.artifacts.* 2>/dev/null || true

# 4. Replay hived in $CP_DIR to $BLOCK. Since block_log head == $BLOCK,
#    state head will match exactly and the consistency check passes.
echo "[4/4] Replay hived to $BLOCK in $CP_DIR..."
cd "$CP_DIR"
"$HIVED_BIN" --data-dir=. --replay-blockchain --exit-at-block="$BLOCK" 2>&1 | tail -3
echo "checkpoint ready at $CP_DIR"
