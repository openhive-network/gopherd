#!/bin/bash
# Assemble the gtg block_log prefix, trim it to a clean block boundary,
# replay ghive 8.22M→TARGET with invariant checks, build an isolated
# hived checkpoint at TARGET, and full-state-diff Go vs hived.
#
# Phase 1 (deterministic) runs here. Phase 2 (hived serve + diff) is
# printed at the end to run once Phase 1's snapshot exists.
set -euo pipefail

TARGET="${1:-20000000}"
EXT=/home/martin/blocklog-ext
GT=/tmp/gtools
UTIL=/home/martin/projects/hive/build/programs/util/block_log_util

echo "[1/5] Concatenate parallel chunks → $EXT/blockchain/block_log"
cat "$EXT"/chunks/chunk.* > "$EXT/blockchain/block_log"
ls -la "$EXT/blockchain/block_log" | awk '{printf "  assembled %.2f GB\n",$5/1e9}'

echo "[2/5] find-end (locate last full block in the partial download)"
"$UTIL" --block-log "$EXT/blockchain/block_log" --find-end 2>&1 | grep -iE "block|end|truncat|head" | tail -5 || true

echo "[3/5] truncate to $TARGET (clean boundary; rebuilds a valid trailer)"
"$UTIL" --block-log "$EXT/blockchain/block_log" --truncate --block-num "$TARGET" 2>&1 | tail -3
"$UTIL" --block-log "$EXT/blockchain/block_log" --get-head-block-number 2>&1 | tail -2

echo "[4/5] generate artifacts for the truncated log"
"$UTIL" --block-log "$EXT/blockchain/block_log" --generate-artifacts 2>&1 | tail -2 || true

echo "[5/5] ghive replay 8,221,695-snapshot → $TARGET (invariants every 200k, snapshot at TARGET)"
rm -rf /tmp/go-20m-snap
"$GT" replay --backend=mem --progress-every=1000000 \
  --load-snapshot=/tmp/go-audit-snap2/block-8221695 \
  --check-invariants=200000 --check-invariants-stop=false \
  --save-dir-snapshot-at="$TARGET" --save-dir-snapshot-to=/tmp/go-20m-snap \
  "$EXT/blockchain/block_log"

echo
echo "PHASE 1 DONE. Phase 2 (hived reference + diff):"
echo "  rm -rf /tmp/hcp20/blockchain && mkdir -p /tmp/hcp20/blockchain"
echo "  ln -sf $EXT/blockchain/block_log /tmp/hcp20/blockchain/block_log"
echo "  ln -sf $EXT/blockchain/block_log.artifacts /tmp/hcp20/blockchain/block_log.artifacts"
echo "  HIVED=/home/martin/projects/hive2/build/programs/hived/hived"
echo "  \$HIVED --data-dir=/tmp/hcp20 --config=/tmp/hcp-config.ini --replay-blockchain --exit-at-block=$TARGET"
echo "  \$HIVED --data-dir=/tmp/hcp20 --config=/tmp/hcp-config.ini   # serve (edit endpoint to :18091)"
echo "  $GT state-jsonl --snapshot=/tmp/go-20m-snap/block-$TARGET --out=/tmp/go_$TARGET.jsonl"
echo "  /tmp/drift-hunter --dump-jsonl=/tmp/hived_$TARGET.jsonl --hived-url=http://127.0.0.1:18091"
echo "  $GT snapshot-diff <(grep -vE 'limit_order|convert|savings|vesting_deleg|escrow|proposal|reward_fund' /tmp/go_$TARGET.jsonl) /tmp/hived_$TARGET.jsonl"
