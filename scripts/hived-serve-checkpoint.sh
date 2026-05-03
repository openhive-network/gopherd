#!/bin/bash
# Serve hived API from a prepared checkpoint dir. The block_log there is
# truncated to the checkpoint block, so hived's consistency check passes
# without forcing a replay.
#
# Usage: hived-serve-checkpoint.sh <block_num>
# After this is running, query at http://127.0.0.1:18090
set -euo pipefail
BLOCK="${1:?usage: $0 <block_num>}"
CP_DIR="/tmp/hived-cp-${BLOCK}"
HIVED_BIN="${HIVED_BIN:-/home/martin/projects/hive/build/programs/hived/hived}"
API_CONFIG="${HIVED_API_CONFIG:-/home/martin/projects/hived-go/scripts/hived-api.conf}"

if [ ! -d "$CP_DIR" ]; then
  echo "$CP_DIR not found — run scripts/hived-prepare-checkpoint.sh $BLOCK first" >&2
  exit 1
fi

cd "$CP_DIR"
ln -sf "$API_CONFIG" "./api-config.ini"
exec "$HIVED_BIN" --data-dir=. --config=api-config.ini
