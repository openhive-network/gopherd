#!/usr/bin/env bash
#
# p2p_mainnet_leech.sh — full from-0 P2P sync against LIVE mainnet, PASSIVE.
#
# Validates the gopherd p2p block-processing pipeline (fetch / decompress /
# reorder / dedup / apply) end-to-end over the real chain, WITHOUT ever
# providing blocks to the network:
#
#   * PASSIVE: never --p2p-serve. All three provide-paths (advertise INV,
#     serve block-ids, serve blocks) are gated off, and the node daemon wires
#     no gossip relay — it is structurally incapable of the 2026-07-09
#     "advertise then can't serve" failure. It only leeches.
#   * HARD STOP below the live tip via --target-block, so it never even
#     reaches the head window (belt over the passive braces).
#   * EPHEMERAL (--no-block-log): state + cold tier only, so it fits disk.
#
# This intentionally connects to mainnet (leech only). That overrides the
# standing never-mainnet rule and was explicitly authorised for this run.
#
set -euo pipefail

BIN="${BIN:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/bin/gopherd}"
WORK="${WORK:-/home/martin/p2p-mainnet-leech}"
TARGET="${TARGET:-107900000}"     # stop here — must stay BELOW the live tip
TARGET_CEILING=108500000          # refuse anything at/above this (tip-safety)
MEM_MAX="${MEM_MAX:-28G}"         # cgroup hard cap (box has 31G)
MEM_LIMIT_GIB="${MEM_LIMIT_GIB:-24}"
RESIDENT_CAP="${RESIDENT_CAP:-500000}"
MAX_PEERS="${MAX_PEERS:-12}"
API="${API:-127.0.0.1:8091}"

# ---- safety asserts --------------------------------------------------------
[ -x "$BIN" ] || { echo "no gopherd at $BIN" >&2; exit 1; }
# `node --help` exits non-zero (flag help convention); capture then grep so
# pipefail doesn't misread the help exit code as "flag missing".
HELP_OUT="$("$BIN" node --help 2>&1 || true)"
# Go's flag package prints flags single-dash in --help ("-no-block-log").
echo "$HELP_OUT" | grep -q "no-block-log" || { echo "binary lacks --no-block-log; rebuild" >&2; exit 1; }
if [ "$TARGET" -ge "$TARGET_CEILING" ]; then
  echo "REFUSING: --target $TARGET >= ceiling $TARGET_CEILING (would risk the live tip)" >&2; exit 1
fi
# Belt: this script NEVER passes --p2p-serve. Assert it isn't sneaking in via env.
case "${EXTRA_ARGS:-}" in *p2p-serve*) echo "REFUSING: --p2p-serve must never be set on a mainnet leech" >&2; exit 1;; esac

DATA="$WORK/data"; COLD="$WORK/cold"; LOG="$WORK/leech.log"
mkdir -p "$DATA" "$COLD"

cat <<BANNER
=====================================================================
 gopherd MAINNET P2P LEECH — PASSIVE, EPHEMERAL, HARD-STOP
 target-block : $TARGET   (live tip must stay above this)
 serve        : OFF  (never --p2p-serve; cannot provide blocks)
 block_log    : OFF  (--no-block-log; state+cold only)
 mem cap      : $MEM_MAX cgroup / ${MEM_LIMIT_GIB}GiB soft ; peers<=$MAX_PEERS
 data=$DATA
 cold=$COLD
 log =$LOG
=====================================================================
BANNER

# systemd-run --scope in the FOREGROUND of this script so the process is
# tracked (memory-capped, swap disabled). NOTE: mainnet seeds are the
# compiled defaults — we deliberately do NOT pass --no-default-seeds.
exec systemd-run --user --scope --collect \
  -p MemoryMax="$MEM_MAX" -p MemorySwapMax=0 \
  "$BIN" node \
    --data-dir="$DATA" \
    --backend=pebble \
    --no-block-log \
    --target-block="$TARGET" \
    --comment-resident-cap="$RESIDENT_CAP" \
    --cold-backend=pebble --cold-dir="$COLD" \
    --mem-limit-gib="$MEM_LIMIT_GIB" \
    --cache-mb=1024 \
    --max-outbound-peers="$MAX_PEERS" \
    --api-endpoint="$API" \
    2>&1 | tee "$LOG"
