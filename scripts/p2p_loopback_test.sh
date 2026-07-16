#!/usr/bin/env bash
#
# p2p_loopback_test.sh — Tier-1 offline P2P validation for the gopherd sync
# layer. Two gopherd nodes on loopback, ZERO mainnet contact:
#
#   Node A  = block source. Seeded to height N_A, serves over 127.0.0.1.
#   Node B  = leecher. Seeded to height N_B (< N_A), dials ONLY A, catches up.
#
# What it proves (end-to-end, over the REAL binary / TCP transport / apply +
# pebble + block_log stack — coverage the in-process Go tests can't give):
#   * a node that is BEHIND still fully catches up with the hived-1.28.7
#     catch-up gate active (handleInv ignoring tip inventory while syncing),
#   * 0 apply_block failures, no queue-full / reorder drops.
#
# Scope note: the seed blocks are historical (genesis-era timestamps), so node
# A is itself "catching up" and — by the SAME isCatchingUp() suppression on its
# own advertise side — never sends item_ids_inventory. B's gate therefore never
# needs to fire here (invIgnored stays 0, which is correct). The gate FIRING +
# self-clearing on live blocks is covered by the Go tests
# (TestHandleInvIgnoredWhileCatchingUp, TestINVPropagation over real TCP) and
# by Tier-2 (leech from a live node). This script is the "does the gate break
# real catch-up?" guard.
#
# SAFETY: every node runs with --no-default-seeds and binds/dials loopback
# only. No compiled mainnet seed is ever dialed. Node B is passive (no
# --p2p-serve); node A serves ONLY on 127.0.0.1 to node B. This cannot touch
# live mainnet. See the never-touch-mainnet rule.
#
# Usage:
#   scripts/p2p_loopback_test.sh
#   SRC=/path/to/block_log N_A=6000 N_B=4000 scripts/p2p_loopback_test.sh
#
set -euo pipefail

# ---- config (override via env) --------------------------------------------
REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BIN="${BIN:-$REPO/bin/gopherd}"
SRC="${SRC:-/home/martin/p2psync-from0/blockchain/block_log}"  # shared source block_log
WORK="${WORK:-/home/martin/p2ptier1}"           # scratch data dirs (wiped)
N_A="${N_A:-5000}"                              # node A seed height
N_B="${N_B:-4000}"                              # node B seed height (< N_A; gap < 2048 ring)
A_P2P="${A_P2P:-127.0.0.1:2001}"
B_API="${B_API:-127.0.0.1:8092}"
TIMEOUT="${TIMEOUT:-90}"                         # seconds to wait for B to catch up

log()  { echo "[$(date +%H:%M:%S)] $*"; }
fail() { echo "FAIL: $*" >&2; exit 1; }

A_DIR="$WORK/nodeA"
B_DIR="$WORK/nodeB"
A_LOG="$WORK/nodeA.serve.log"
B_LOG="$WORK/nodeB.leech.log"
A_PID=""; B_PID=""

cleanup() {
  [ -n "$B_PID" ] && kill "$B_PID" 2>/dev/null || true
  [ -n "$A_PID" ] && kill "$A_PID" 2>/dev/null || true
  wait 2>/dev/null || true
}
trap cleanup EXIT INT TERM

# ---- preflight -------------------------------------------------------------
[ -x "$BIN" ]  || fail "gopherd binary not found/executable at $BIN (build: go build -o bin/gopherd ./cmd/gopherd)"
[ -e "$SRC" ]  || fail "source block_log not found at $SRC (set SRC=...)"
[ "$N_B" -lt "$N_A" ] || fail "N_B ($N_B) must be < N_A ($N_A)"
[ $((N_A - N_B)) -lt 2048 ] || fail "gap N_A-N_B must be < 2048 (recent-block ring); got $((N_A - N_B))"
command -v curl >/dev/null || fail "curl required"

log "config: SRC=$SRC N_A=$N_A N_B=$N_B ring-gap=$((N_A - N_B)) work=$WORK"
rm -rf "$WORK"; mkdir -p "$WORK"

# ---- phase 1: seed A and B by replaying the shared source to staggered heights
seed() {
  local dir="$1" h="$2" name="$3"
  log "seed $name: replaying $SRC -> head $h (self-replay, no p2p)"
  "$BIN" node --data-dir="$dir" --backend=pebble --no-default-seeds \
    --source-block-log="$SRC" --max-blocks="$h" >"$WORK/$name.seed.log" 2>&1 \
    || { tail -20 "$WORK/$name.seed.log"; fail "$name seed replay failed"; }
  [ -f "$dir/blockchain/block_log" ] || fail "$name seed did not write a local block_log"
}
seed "$A_DIR" "$N_A" nodeA
seed "$B_DIR" "$N_B" nodeB

# ---- phase 2: start A in serve mode; wait until it has recovered + is listening
log "start A: serve on $A_P2P (recovers to $N_A, fills 2048-block ring)"
"$BIN" node --data-dir="$A_DIR" --backend=pebble --no-default-seeds \
  --p2p-endpoint="$A_P2P" --p2p-serve >"$A_LOG" 2>&1 &
A_PID=$!
for _ in $(seq 1 60); do
  grep -q "recover: caught up to block $N_A" "$A_LOG" 2>/dev/null && \
    grep -q "starting (seeds=" "$A_LOG" 2>/dev/null && break
  kill -0 "$A_PID" 2>/dev/null || { tail -20 "$A_LOG"; fail "node A exited early"; }
  sleep 0.5
done
grep -q "starting (seeds=" "$A_LOG" || { tail -20 "$A_LOG"; fail "node A never reached serve loop"; }
log "A ready (head=$N_A, serving)"

# ---- phase 3: start B in leech mode (passive), dialing ONLY A ---------------
log "start B: leech from A, API on $B_API (passive, no serve)"
"$BIN" node --data-dir="$B_DIR" --backend=pebble --no-default-seeds \
  --extra-seed="$A_P2P" --api-endpoint="$B_API" >"$B_LOG" 2>&1 &
B_PID=$!

head_of_B() {
  curl -s -m 3 -X POST "http://$B_API" -H 'content-type: application/json' \
    -d '{"jsonrpc":"2.0","id":1,"method":"condenser_api.get_dynamic_global_properties","params":[]}' 2>/dev/null \
    | grep -o '"head_block_number":[0-9]*' | head -1 | grep -o '[0-9]*' || true
}

# ---- phase 4: poll B's head until it reaches A's, or time out ---------------
log "waiting up to ${TIMEOUT}s for B to catch up ${N_B} -> ${N_A}"
deadline=$(( $(date +%s) + TIMEOUT ))
bhead=0
while [ "$(date +%s)" -lt "$deadline" ]; do
  kill -0 "$B_PID" 2>/dev/null || { tail -25 "$B_LOG"; fail "node B exited early"; }
  h="$(head_of_B)"; [ -n "$h" ] && bhead="$h"
  [ "$bhead" -ge "$N_A" ] && break
  sleep 1
done

# ---- phase 5: assertions ---------------------------------------------------
log "B head = $bhead (target $N_A)"
[ "$bhead" -ge "$N_A" ] || { tail -30 "$B_LOG"; fail "B did not catch up: head=$bhead want >=$N_A"; }

# The P2P link was actually used: B connected to A, and B advanced past its
# own seed height (its local block_log stops at N_B, so anything beyond N_B
# could only have come from A over the wire — not a recover no-op).
grep -q "peer connected: $A_P2P" "$B_LOG" || { tail -30 "$B_LOG"; fail "B never connected to A over loopback"; }
[ "$bhead" -gt "$N_B" ] || fail "B did not advance past its seed height $N_B over P2P (head=$bhead)"

# No apply failures.
if grep -q "apply_block .* failed" "$B_LOG"; then
  grep "apply_block .* failed" "$B_LOG" | head; fail "node B logged apply_block failures"
fi

# Healthy STATS: no queue-full / reorder drops. Report the gate counter.
laststats="$(grep 'STATS ' "$B_LOG" | tail -1 || true)"
if [ -n "$laststats" ]; then
  qfull=$(echo "$laststats"     | grep -o 'qfull=[0-9]*'       | grep -o '[0-9]*')
  rdrop=$(echo "$laststats"     | grep -o 'reorderDrop=[0-9]*' | grep -o '[0-9]*')
  ignored=$(echo "$laststats"   | grep -o 'invIgnored=[0-9]*'  | grep -o '[0-9]*')
  log "B STATS: qfull=${qfull:-?} reorderDrop=${rdrop:-?} invIgnored=${ignored:-?}"
  [ "${qfull:-0}" = "0" ] || fail "node B had queue-full drops (qfull=$qfull)"
  [ "${rdrop:-0}" = "0" ] || fail "node B had reorder drops (reorderDrop=$rdrop)"
else
  # A ring-bounded loopback catch-up (<2048 blocks) completes faster than the
  # 5s STATS tick, so a STATS line often isn't emitted. That's expected; the
  # guaranteed signals above (head reached target + P2P link used + 0 apply
  # failures) are the proof.
  log "B STATS: (none emitted — catch-up finished before the first 5s tick)"
fi

echo
echo "PASS: node B connected to A over loopback and caught up ${N_B} -> ${bhead}"
echo "      (blocks past ${N_B} came from A over P2P), with 0 apply failures."
echo "      Catch-up path intact with the hived-1.28.7 tip-inventory gate active."
