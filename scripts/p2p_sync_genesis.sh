#!/usr/bin/env bash
#
# p2p_sync_genesis.sh — from-scratch P2P sync of mainnet to the LIVE head, as a
# detached user service (survives the terminal that launched it).
#
# This is the "no snapshot, no block_log" path: an empty data dir, the
# compiled-in seeds, every block fetched from peers and applied through the
# evaluators. It writes a local block_log (so a restart replays it instead of
# re-fetching 110M blocks) and spills frozen comments to a pebble cold tier.
#
#   * PASSIVE: never --p2p-serve. The node only leeches.
#   * No --target-block: it reaches the head window and follows the tip.
#   * Memory-capped with a user cgroup (MemoryMax, swap off). The Go soft
#     limit sits below the cap so the pebble caches + memtables (off-heap,
#     see --cold-cache-mb) fit under it.
#
# Disk: a p2p-written block_log reaches ~540 GB at 110M blocks (peer payloads
# are level-3 zstd); the cold tier ~40-100 GB; state ~2 GB. Check `df` first.
#
#   scripts/p2p_sync_genesis.sh            # start
#   systemctl --user status gopherd-sync   # is it running
#   tail -f $WORK/sync.log                 # progress (STATS lines)
#   systemctl --user stop gopherd-sync     # SIGTERM -> shutdown snapshot
#
# Measured on a 12-core / 31 GB desktop (2026-10): see README "Hardware".
set -euo pipefail

BIN="${BIN:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/bin/gopherd}"
WORK="${WORK:-$HOME/gopherd-sync}"
UNIT="${UNIT:-gopherd-sync}"
MEM_MAX="${MEM_MAX:-16G}"                 # cgroup hard cap
MEM_LIMIT_GIB="${MEM_LIMIT_GIB:-10}"      # Go soft limit; leave room for page cache
CACHE_MB="${CACHE_MB:-512}"               # pebble state block cache (off-heap)
COLD_CACHE_MB="${COLD_CACHE_MB:-256}"     # pebble cold-tier block cache (off-heap)
RESIDENT_CAP="${RESIDENT_CAP:-2000000}"   # comments kept in RAM; the rest spill
MAX_PEERS="${MAX_PEERS:-12}"
API="${API:-127.0.0.1:8090}"
PPROF="${PPROF:-127.0.0.1:6060}"
# EXTRA_ARGS: anything else to pass to `gopherd node` (never --p2p-serve here).
EXTRA_ARGS="${EXTRA_ARGS:-}"

[ -x "$BIN" ] || { echo "no gopherd at $BIN (make build)" >&2; exit 1; }
case "$EXTRA_ARGS" in *p2p-serve*) echo "REFUSING: --p2p-serve must never be set on a from-scratch sync" >&2; exit 1;; esac
if systemctl --user is-active --quiet "$UNIT"; then
  echo "$UNIT is already running (systemctl --user status $UNIT)" >&2; exit 1
fi

DATA="$WORK/data"; COLD="$WORK/cold"; SNAPS="$WORK/snapshots"; LOG="$WORK/sync.log"
mkdir -p "$DATA" "$COLD" "$SNAPS"
# Run a private copy so a later `make build` cannot touch the running binary.
cp "$BIN" "$WORK/gopherd"

cat <<BANNER
=====================================================================
 gopherd MAINNET P2P SYNC — genesis -> live head, PASSIVE
 unit      : $UNIT (systemctl --user ...)
 serve     : OFF  (never --p2p-serve)
 block_log : ON   ($DATA/blockchain; restart replays it)
 mem       : $MEM_MAX cgroup / ${MEM_LIMIT_GIB}GiB Go limit ; caches $CACHE_MB+$COLD_CACHE_MB MB
 peers     : <=$MAX_PEERS outbound, compiled-in seeds
 api       : $API   pprof: $PPROF
 data=$DATA
 cold=$COLD
 log =$LOG
=====================================================================
BANNER

# shellcheck disable=SC2086
exec systemd-run --user --unit="$UNIT" --collect \
  -p MemoryMax="$MEM_MAX" -p MemorySwapMax=0 \
  -p KillSignal=SIGTERM -p TimeoutStopSec=3600 \
  -p WorkingDirectory="$WORK" \
  -p StandardOutput=append:"$LOG" -p StandardError=append:"$LOG" \
  "$WORK/gopherd" node \
    --data-dir="$DATA" \
    --backend=pebble \
    --comment-resident-cap="$RESIDENT_CAP" \
    --cold-backend=pebble --cold-dir="$COLD" --cold-cache-mb="$COLD_CACHE_MB" \
    --mem-limit-gib="$MEM_LIMIT_GIB" \
    --cache-mb="$CACHE_MB" \
    --max-outbound-peers="$MAX_PEERS" \
    --shutdown-snapshot-dir="$SNAPS" \
    --api-endpoint="$API" \
    --pprof-addr="$PPROF" \
    $EXTRA_ARGS
