#!/bin/bash
# Replay hived to a specific block AND dump a state_snapshot at that
# block, then exit. hived-api-serve loads that snapshot back so its
# consistency check (state head vs block_log head) passes with just a
# warning rather than aborting.
#
# Usage: hived-replay-to.sh <block_num>
set -euo pipefail
BLOCK="${1:?usage: $0 <block_num>}"
DATA_DIR="${HIVED_DATA_DIR:-/tmp/hived-genesis}"
HIVED_BIN="${HIVED_BIN:-/home/martin/projects/hive/build/programs/hived/hived}"
SNAP_NAME="parity-${BLOCK}"

# Always force-replay so we land at exactly the requested block (clean state).
rm -rf "$DATA_DIR/blockchain/comments-rocksdb-storage" "$DATA_DIR/blockchain/shared_memory.bin"

# state_snapshot lives under $DATA_DIR/snapshot/<NAME> by default.
# Wipe any previous snapshot of the same name before re-dumping.
rm -rf "$DATA_DIR/snapshot/$SNAP_NAME"

# Need plugin = state_snapshot for --dump-snapshot to do anything; the
# default config in the test data dir already has it.
cd "$DATA_DIR"
exec "$HIVED_BIN" --data-dir=. \
  --replay-blockchain --exit-at-block="$BLOCK" \
  --dump-snapshot="$SNAP_NAME"
