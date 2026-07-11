#!/usr/bin/env bash
# api_smoke.sh — hit a running gopherd node's JSON-RPC API over HTTP and report
# pass/fail per method. Validates the live wire path end-to-end against real
# replayed state. Usage: api_smoke.sh [endpoint]  (default 127.0.0.1:8091)
#
# A row PASSES if the response carries a "result" and no "error". Rows tagged
# ERR (expected-error / negative) pass when the response DOES carry an error.
set -u
EP="${1:-127.0.0.1:8091}"
URL="http://$EP"
pass=0 fail=0

call() { curl -s -m 8 -X POST "$URL" -H 'content-type: application/json' \
  -d '{"jsonrpc":"2.0","id":1,"method":"'"$1"'","params":'"$2"'}'; }

check() { # kind method params
  local kind="$1" method="$2" params="$3" resp err res
  resp="$(call "$method" "$params")"
  err="$(printf '%s' "$resp" | jq -c '.error // empty' 2>/dev/null)"
  res="$(printf '%s' "$resp" | jq -c '.result' 2>/dev/null)"
  if [ "$kind" = ERR ]; then
    if [ -n "$err" ]; then printf '  PASS  %-52s (rejected)\n' "$method"; pass=$((pass+1))
    else printf '  FAIL  %-52s (expected error, got %s)\n' "$method" "${res:0:60}"; fail=$((fail+1)); fi
  else
    if [ -z "$err" ] && [ "$res" != "null" ] && [ -n "$res" ]; then
      printf '  PASS  %-52s %s\n' "$method" "${res:0:64}"; pass=$((pass+1))
    else printf '  FAIL  %-52s err=%s res=%s\n' "$method" "${err:0:50}" "${res:0:50}"; fail=$((fail+1)); fi
  fi
}

echo "== gopherd API smoke test @ $URL =="
echo "-- database_api globals"
check OK database_api.get_dynamic_global_properties '{}'
check OK database_api.get_config '{}'
check OK database_api.get_version '{}'
check OK database_api.get_feed_history '{}'
check OK database_api.get_current_price_feed '{}'
check OK database_api.get_witness_schedule '{}'
check OK database_api.get_active_witnesses '{}'
check OK database_api.get_reward_funds '{}'
check OK database_api.get_hardfork_properties '{}'
check OK database_api.get_order_book '{"limit":5}'
echo "-- database_api find/list"
check OK database_api.find_accounts '{"accounts":["gtg","blocktrades"]}'
check OK database_api.find_witnesses '{"owners":["gtg"]}'
check OK database_api.list_proposals '{"limit":10}'
echo "-- condenser_api"
check OK condenser_api.get_dynamic_global_properties '[]'
check OK condenser_api.get_version '[]'
check OK condenser_api.get_chain_properties '[]'
check OK condenser_api.get_hardfork_version '[]'
check OK condenser_api.get_next_scheduled_hardfork '[]'
check OK condenser_api.get_feed_history '[]'
check OK condenser_api.get_current_median_history_price '[]'
check OK condenser_api.get_active_witnesses '[]'
check OK condenser_api.get_witness_schedule '[]'
check OK condenser_api.get_reward_fund '["post"]'
check OK condenser_api.get_account_count '[]'
check OK condenser_api.get_accounts '[["gtg","therealwolf"]]'
check OK condenser_api.lookup_accounts '["a",10]'
check OK condenser_api.lookup_witness_accounts '["",10]'
check OK condenser_api.get_witness_count '[]'
check OK condenser_api.get_witness_by_account '["gtg"]'
check OK condenser_api.get_witnesses_by_vote '["",21]'
check OK condenser_api.get_open_orders '["gtg"]'
check OK condenser_api.get_order_book '[5]'
echo "-- rc_api (RC active at 30M)"
check OK rc_api.get_resource_params '{}'
check OK rc_api.get_resource_pool '{}'
check OK rc_api.find_rc_accounts '{"accounts":["gtg","blocktrades"]}'
check OK rc_api.list_rc_accounts '{"start":"a","limit":10}'
check OK rc_api.get_rc_stats '{}'
check OK rc_api.get_rc_operation_stats '{"operation":"vote"}'
check OK condenser_api.find_rc_accounts '[["gtg"]]'
check OK condenser_api.list_rc_accounts '["a",10]'
echo "-- negative (must reject)"
check ERR rc_api.list_rc_accounts '{"start":"a","limit":0}'
check ERR rc_api.list_rc_accounts '{"start":"a","limit":1001}'
check ERR rc_api.list_rc_direct_delegations '{"start":["gtg"],"limit":10}'
check ERR rc_api.get_rc_operation_stats '{"operation":"not_an_op"}'

echo
echo "== $pass passed, $fail failed =="
[ "$fail" -eq 0 ]
