#!/bin/bash
# Start hived in API mode by re-loading the state_snapshot dumped by
# hived-replay-to.sh. The snapshot path makes hived's consistency check
# accept state head < block_log head (it only warns when a snapshot
# was loaded, vs aborting in the no-snapshot case).
#
# Usage: hived-api-serve.sh <block_num>
# Then query at http://127.0.0.1:18090
set -euo pipefail
BLOCK="${1:?usage: $0 <block_num>}"
DATA_DIR="${HIVED_DATA_DIR:-/tmp/hived-genesis}"
HIVED_BIN="${HIVED_BIN:-/home/martin/projects/hive/build/programs/hived/hived}"
CONFIG="${HIVED_API_CONFIG:-/home/martin/projects/hived-go/scripts/hived-api.conf}"
SNAP_NAME="parity-${BLOCK}"

cd "$DATA_DIR"
ln -sf "$CONFIG" "./api-config.ini"
# Wipe state so --load-snapshot can repopulate from scratch (loading on
# top of an existing state file is rejected by chainbase).
rm -rf "./blockchain/comments-rocksdb-storage" "./blockchain/shared_memory.bin"
exec "$HIVED_BIN" --data-dir=. --config=api-config.ini \
  --load-snapshot="$SNAP_NAME"
