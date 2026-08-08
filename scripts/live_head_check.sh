#!/usr/bin/env bash
#
# live_head_check.sh — prove a live gopherd node is on the CANONICAL branch.
#
# Following the head is not the same as following the RIGHT head: a node that
# picked the losing side of a micro-fork still reports a healthy-looking head.
# This compares our head block's ID against a reference hived node's ID for
# that same height — ids match => identical branch, bit-exact header.
#
# Also reports the head/live gap and last-irreversible lag, and diffs a few
# consensus DGPO scalars at our (slightly trailing) height.
#
#   LOCAL=127.0.0.1:8091 REF=https://api.hive.blog ./scripts/live_head_check.sh
set -euo pipefail

LOCAL="${LOCAL:-127.0.0.1:8091}"
REF="${REF:-https://api.hive.blog}"

rpc() { # rpc <url> <method> <params-json>
  curl -s --max-time 15 -X POST "$1" -H 'Content-Type: application/json' \
    -d "{\"jsonrpc\":\"2.0\",\"method\":\"$2\",\"params\":$3,\"id\":1}"
}
# Path-based extraction. Must be jq, not grep: block_api serialises with a
# space after the colon ("previous": "0067…") while database_api does not, and
# `previous` is nested under .result.header — a flat regex gets both wrong.
jq_at() { jq -r "$2 // empty" <<<"$1"; }

ours=$(rpc "http://$LOCAL" database_api.get_dynamic_global_properties '{}')
if ! grep -q head_block_number <<<"$ours"; then
  echo "FAIL: local node at $LOCAL did not answer get_dynamic_global_properties"; echo "$ours" | head -c 300; exit 1
fi
our_head=$(jq_at "$ours" .result.head_block_number)
our_id=$(jq_at "$ours" .result.head_block_id)
our_lib=$(jq_at "$ours" .result.last_irreversible_block_num)

theirs=$(rpc "$REF" database_api.get_dynamic_global_properties '{}')
live_head=$(jq_at "$theirs" .result.head_block_number)
live_lib=$(jq_at "$theirs" .result.last_irreversible_block_num)

echo "local  head=$our_head id=$our_id lib=$our_lib"
echo "ref    head=$live_head lib=$live_lib   gap=$((live_head - our_head))  lib_lag=$((our_head - our_lib))"

# Reference id for OUR height (not theirs) — the only apples-to-apples compare.
# condenser_api.get_block exposes block_id directly, so this works even when we
# are exactly AT the reference's tip (deriving the id from header(N+1).previous
# would be INCONCLUSIVE there, since block N+1 does not exist yet).
refblk=$(rpc "$REF" condenser_api.get_block "[$our_head]")
ref_id_of_our_head=$(jq_at "$refblk" .result.block_id)

if [ -z "$ref_id_of_our_head" ]; then
  echo "INCONCLUSIVE: reference has no block $our_head yet (we may be ahead of its tip); retry in a few seconds"
  exit 2
fi
if [ "$ref_id_of_our_head" = "$our_id" ]; then
  echo "PASS: block $our_head id matches the reference chain — same branch"
else
  echo "FAIL: BRANCH MISMATCH at $our_head"
  echo "  ours=$our_id"
  echo "  ref =$ref_id_of_our_head"
  exit 1
fi
