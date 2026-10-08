#!/usr/bin/env bash
# scripts/parity-replay-compare.sh — replay a block_log with BOTH gopherd and a
# MAINNET hived, then diff the resulting state.
#
# Block ids do not commit to state, so a clean gopherd replay proves only that
# no block was rejected. This script adds the other half: hived's state at the
# same head, pulled over database_api and compared field by field by
# parity-compare-state.py (dgpo totals, feed median, every witness, every
# account's balances / vesting / withdrawals / proxy, orders, conversions,
# savings withdrawals).
#
# usage: parity-replay-compare.sh <block_log> <mainnet hived binary> <scratch dir>
#
# Everything heavy runs inside a systemd --user scope with a MemoryMax cap, and
# the two replays run one after the other: this box locks up when builds and
# replays overlap. A testnet hived build cannot replay mainnet blocks (check
# `hived --version` for node_type), and hived < 1.27 cannot read a compressed
# block_log.
set -euo pipefail
LOG="${1:?block_log}"; HIVED="${2:?mainnet hived binary}"; SCRATCH="${3:?scratch dir}"
GOPHERD="${GOPHERD:-$(dirname "$0")/../bin/gopherd}"
PORT="${HIVED_PORT:-18091}"
mkdir -p "$SCRATCH/go" "$SCRATCH/hived/blockchain"

HEAD=$("$GOPHERD" inspect "$LOG" | awk '/^head_block:/{print $2}')
echo "block_log head: $HEAD"

echo "[1/4] gopherd replay genesis -> $HEAD (invariants every 100k, vops on, snapshot at head)"
systemd-run --user --scope -p MemoryMax=12G -p MemorySwapMax=0 -q \
  "$GOPHERD" replay --backend=mem --mem-limit-gib=9 --prefetch=8 --progress-every=500000 \
  --check-invariants=100000 --check-invariants-stop=false --emit-vops \
  --save-dir-snapshot-at="$HEAD" --save-dir-snapshot-to="$SCRATCH/go/snap" "$LOG" \
  2>&1 | tee "$SCRATCH/go/replay.log" | grep -E "block errors|status:|invariant|PASS|FAIL" || true

echo "[2/4] gopherd state -> $SCRATCH/go_$HEAD.jsonl"
systemd-run --user --scope -p MemoryMax=6G -q \
  "$GOPHERD" state-jsonl --snapshot="$SCRATCH/go/snap/block-$HEAD" --out="$SCRATCH/go_$HEAD.jsonl"

echo "[3/4] hived replay -> $HEAD, then serve database_api on :$PORT"
ln -sf "$LOG" "$SCRATCH/hived/blockchain/block_log"
cat > "$SCRATCH/hived/config.ini" <<CFG
log-appender = {"appender":"stderr","stream":"std_error","time_format":"iso_8601_milliseconds"}
log-logger = {"name":"default","level":"info","appender":"stderr"}
plugin = chain p2p webserver json_rpc database_api block_api account_by_key account_by_key_api
shared-file-size = 8G
block-log-split = -1
webserver-http-endpoint = 127.0.0.1:$PORT
webserver-thread-pool-size = 2
p2p-endpoint = 127.0.0.1:0
p2p-max-connections = 0
p2p-seed-node =
CFG
systemd-run --user --unit=parity-hived --collect -p MemoryMax=14G -p MemorySwapMax=0 \
  -p WorkingDirectory="$SCRATCH/hived" \
  -p StandardOutput=append:"$SCRATCH/hived/hived.log" -p StandardError=append:"$SCRATCH/hived/hived.log" \
  "$HIVED" --data-dir="$SCRATCH/hived" --config="$SCRATCH/hived/config.ini" --replay-blockchain
for _ in $(seq 1 2000); do
  h=$(curl -s -m 5 -d '{"jsonrpc":"2.0","method":"database_api.get_dynamic_global_properties","params":{},"id":1}' "http://127.0.0.1:$PORT" 2>/dev/null \
      | python3 -c 'import sys,json; print(json.load(sys.stdin)["result"]["head_block_number"])' 2>/dev/null || true)
  [ "$h" = "$HEAD" ] && break
  systemctl --user is-active --quiet parity-hived.service || { echo "hived stopped early:"; tail -20 "$SCRATCH/hived/hived.log"; exit 1; }
  sleep 10
done
echo "hived serving at head $h"

echo "[4/4] compare"
python3 "$(dirname "$0")/parity-compare-state.py" "$SCRATCH/go_$HEAD.jsonl" "http://127.0.0.1:$PORT" | tee "$SCRATCH/compare.txt"
echo "hived keeps running as unit parity-hived; stop it with: systemctl --user stop parity-hived"
