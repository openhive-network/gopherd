#!/usr/bin/env bash
#
# p2p_live_head.sh — warm-start from the validated snapshot, then P2P-sync all
# the way to the LIVE mainnet head and stay there (live mode).
#
# Differs from p2p_mainnet_leech.sh (from-0, hard-stop below the tip): this one
# deliberately REACHES the tip and follows it at 3s/block.
#
#   * Warm start  : --load-snapshot (consensus-validated slim snapshot).
#   * No hard stop: no --target-block — the point is to reach the head window.
#   * PASSIVE by default: no --p2p-serve, so all three provide-paths
#     (advertise INV, serve block-ids, serve blocks) stay gated off. Set
#     SERVE=1 only once the node has proven it keeps up in real time.
#   * EPHEMERAL (--no-block-log): a snapshot warm start would otherwise write a
#     PARTIAL local block_log starting at snapshot-head+1, and the restart path
#     (node.go recoverFrom = writer head) would then try to replay that partial
#     log from its block 1 against empty typed-stores. No local log = no such
#     trap; a restart just reloads the snapshot.
#
# Restarting: wipe DATA first (stale pebble K/V from the previous run is only
# partially overwritten by a snapshot load). Cheapest correct restart is a
# fresh DATA + fresh COLD.
set -euo pipefail

BIN="${BIN:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/bin/gopherd}"
WORK="${WORK:-/home/martin/p2p-live}"
SNAPSHOT="${SNAPSHOT:-/home/martin/snapfix-slim/block-107957387}"
MEM_MAX="${MEM_MAX:-28G}"                 # cgroup hard cap (box has 31G)
MEM_LIMIT_GIB="${MEM_LIMIT_GIB:-22}"      # Go soft limit; 14 GC-spiralled at the tip
RESIDENT_CAP="${RESIDENT_CAP:-500000}"    # proven at the tip from this snapshot
MAX_PEERS="${MAX_PEERS:-12}"
API="${API:-127.0.0.1:8091}"
SERVE="${SERVE:-0}"
# WORK_TIMING=1 logs per-block apply avg/p50/p99/max. This is the evidence the
# SERVE=1 decision needs: advertising inventory is only safe if apply time stays
# well under the 3s block interval, since a p99 spiking past it is exactly the
# "advertise then can't serve" shape of the 2026-07-09 incident. Off by default
# (it adds a log line every 10s); turn it ON for any run that leads to serving.
WORK_TIMING="${WORK_TIMING:-0}"
# On clean exit / SIGTERM, save a dir snapshot at the head here. Without this a
# restart rewinds to whatever --load-snapshot was taken (currently 2026-07-08)
# and re-syncs the whole delta over P2P — ~900K blocks, ~2h. With it, restart
# from the newest block-<N>/ in this dir and resume at head in ~25min.
# Set to "" to disable.
SNAP_DIR="${SNAP_DIR:-/home/martin/p2p-live-snaps}"
# Block-production DRY RUN: set WITNESS_KEYS to a file of `account WIF` lines.
# Blocks are built, signed, self-verified, timed and DISCARDED — never applied
# (which would fork us off the chain we are following) and never broadcast.
# PRODUCE_EVERY_SLOT=1 builds every slot instead of only when the schedule names
# us; on mainnet we are never scheduled, so that is what yields timing data.
WITNESS_KEYS="${WITNESS_KEYS:-}"
PRODUCE_EVERY_SLOT="${PRODUCE_EVERY_SLOT:-0}"
# PRODUCE_REAL=1 switches from dry run to REAL production: when the schedule
# names one of our witnesses AND the chain's registered signing key matches the
# key we hold, the block is applied and BROADCAST. A key mismatch is logged and
# the slot skipped (what hived does), so this is inert without the right key.
PRODUCE_REAL="${PRODUCE_REAL:-0}"
# Mmap-backed typed state (hived's chainbase equivalent). Saved on clean exit;
# a restart with the SAME dir resumes AT HEAD in seconds instead of rebuilding
# ~27GB of typed stores from the snapshot (~20min). Empty on first run, so it
# simply starts working after the first clean shutdown. Set to "" to disable.
PERSIST_STATE_DIR="${PERSIST_STATE_DIR:-$WORK/pstate}"

[ -x "$BIN" ] || { echo "no gopherd at $BIN" >&2; exit 1; }
[ -d "$SNAPSHOT" ] || { echo "snapshot dir not found: $SNAPSHOT" >&2; exit 1; }

SERVE_ARGS=()
[ "$WORK_TIMING" = "1" ] && SERVE_ARGS+=(--log-work-timing)
if [ -n "$WITNESS_KEYS" ]; then
  [ -r "$WITNESS_KEYS" ] || { echo "WITNESS_KEYS not readable: $WITNESS_KEYS" >&2; exit 1; }
  if [ "$PRODUCE_REAL" = "1" ]; then
    SERVE_ARGS+=(--produce --witness-keys="$WITNESS_KEYS")
    echo "!! PRODUCE_REAL=1 — this node will APPLY and BROADCAST blocks when the signing key matches."
  else
    SERVE_ARGS+=(--produce-dry-run --witness-keys="$WITNESS_KEYS")
    [ "$PRODUCE_EVERY_SLOT" = "1" ] && SERVE_ARGS+=(--produce-dry-run-every-slot)
  fi
fi
if [ -n "$SNAP_DIR" ]; then
  mkdir -p "$SNAP_DIR"
  SERVE_ARGS+=(--shutdown-snapshot-dir="$SNAP_DIR")
fi
if [ -n "$PERSIST_STATE_DIR" ]; then
  mkdir -p "$PERSIST_STATE_DIR"
  SERVE_ARGS+=(--persist-state-dir="$PERSIST_STATE_DIR")
fi
if [ "$SERVE" = "1" ]; then
  SERVE_ARGS+=(--p2p-serve)
  echo "!! SERVE=1 — this node will ADVERTISE INVENTORY AND SERVE BLOCKS to mainnet peers."
  echo "!! Only correct if it is at the head and keeping up in real time."
fi

DATA="$WORK/data"; COLD="$WORK/cold"; LOG="$WORK/live.log"
mkdir -p "$DATA" "$COLD"

cat <<BANNER
=====================================================================
 gopherd MAINNET P2P -> LIVE HEAD
 snapshot   : $SNAPSHOT
 target     : live head (no --target-block)
 serve      : $([ "$SERVE" = 1 ] && echo "ON (advertise+serve)" || echo "OFF (passive leech)")
 block_log  : OFF (--no-block-log; state+cold only)
 mem        : $MEM_MAX cgroup / ${MEM_LIMIT_GIB}GiB soft ; peers<=$MAX_PEERS
 head snap  : ${SNAP_DIR:-OFF (restart will re-sync the full delta)}
 persist    : ${PERSIST_STATE_DIR:-OFF (every restart reloads the snapshot, ~20min)}
 produce    : $([ -n "$WITNESS_KEYS" ] && { [ "$PRODUCE_REAL" = "1" ] && echo "REAL (apply+broadcast IF key matches)" || echo "DRY RUN (build+sign+verify+discard; every-slot=$PRODUCE_EVERY_SLOT)"; } || echo "OFF")
 data=$DATA
 cold=$COLD
 log =$LOG
=====================================================================
BANNER

exec systemd-run --user --scope --collect \
  -p MemoryMax="$MEM_MAX" -p MemorySwapMax=0 \
  env GHIVE_COLD_COMPRESS=zstd \
  "$BIN" node \
    --data-dir="$DATA" \
    --backend=pebble \
    --no-block-log \
    --load-snapshot="$SNAPSHOT" \
    --comment-resident-cap="$RESIDENT_CAP" \
    --cold-backend=pebble --cold-dir="$COLD" \
    --mem-limit-gib="$MEM_LIMIT_GIB" \
    --cache-mb=1024 \
    --max-outbound-peers="$MAX_PEERS" \
    --api-endpoint="$API" \
    "${SERVE_ARGS[@]}" \
    2>&1 | tee "$LOG"
