# gopherd API implementation map

Maps hived's full API surface (from `hive/libraries/plugins/apis/*`) to a gopherd
build plan. gopherd currently exposes **no JSON-RPC API**; but its typed state
stores (accounts, witnesses, witness_votes, DGPO, feed history, reward funds,
limit orders, proposals, escrows, conversions, vesting delegations, savings
withdrawals, block_log) already hold the data most read APIs return — so much of
this is "expose existing state over JSON-RPC", not new consensus work.

## Transport (build first)
hived serves **JSON-RPC 2.0** over HTTP + WebSocket via appbase. Two calling
conventions:
- **Modern `*_api`** — named-object args, e.g. `database_api.find_accounts {"accounts":["alice"]}`.
- **Legacy `condenser_api`** — POSITIONAL array args, e.g. `condenser_api.get_accounts [["alice"]]`. This is what apps/exchanges/wallets actually call.
Foundation to build: JSON-RPC dispatch, method registry, per-API arg decoding
(named vs positional), error envelope, HTTP + WS listeners. Read-only, bound to a
loopback/opt-in port (NOT the p2p port; unrelated to the mainnet-safety rule).

## The 22 API plugins

### Read / state-query APIs
| API | #methods | Backing gopherd state | Notes |
|---|---|---|---|
| **database_api** | 51 | mostly EXISTS | Canonical modern API. globals + list/find over every object type + tx utils. **Build first.** |
| **block_api** | 3 | block_log + state | get_block, get_block_header, get_block_range |
| **account_history_api** | 4 | **NEW index** | get_ops_in_block, get_transaction, get_account_history, enum_virtual_ops. Needs an account-history op index (big). |
| **account_by_key_api** | 2 | **NEW index** | get_key_references (pubkey→accounts). Needs a key→account index. |
| **market_history_api** | 7 | **NEW index** | ticker/volume/order_book/trade_history/market_history buckets. Needs trade+bucket index. |
| **rc_api** | 7 | **NEW (RC subsystem, deferred)** | resource params/pool, rc_accounts, direct delegations, rc_stats. See RC roadmap memo. |
| **reputation_api** | 1 | **NEW index** | get_account_reputations (from vote rshares). |
| **transaction_status_api** | 1 | **NEW index** | find_transaction (tx_id→block+status). |
| **witness_api** | 3 | witness-node feature | enable/disable/is_fast_confirm_enabled. |

### Aggregators (wrap the above)
| API | #methods | Notes |
|---|---|---|
| **condenser_api** | 71 | Legacy positional aggregator wrapping database/block/account_history/market/rc (+ follow/tags in full node). **The most-used external API.** |
| **wallet_bridge_api** | 35 | Wallet-facing aggregator (get_account, list_my_accounts, broadcast…). |

### Broadcast / node control
| API | #methods | Notes |
|---|---|---|
| **network_broadcast_api** | 1 | broadcast_transaction (needs mempool + p2p relay — couples with p2p work). |
| **chain_api** | 1 | push_transaction. |
| **network_node_api** | 3 | get_info, add_node, set_allowed_peers, get_connected_peers. |
| **node_status_api** | 1 | get_node_status. |
| **app_status_api** | 1 | get_app_status. |

### Test / debug (low priority)
| API | #methods | Notes |
|---|---|---|
| **debug_node_api** | 13 | testnet block generation, set_hardfork, etc. |
| **rewards_api** | 1 | simulate_curve_payouts. |
| **test_api** | 2 | test_api_a/b. |

## database_api methods (51) — the foundation
Globals: get_config, get_version, get_dynamic_global_properties, get_witness_schedule,
get_hardfork_properties, get_reward_funds, get_current_price_feed, get_feed_history,
get_active_witnesses.
list/find pairs (over gopherd typed stores that already exist): witnesses, witness_votes,
accounts, owner_histories, account_recovery_requests, change_recovery_account_requests,
escrows, withdraw_vesting_routes, savings_withdrawals, vesting_delegations,
vesting_delegation_expirations, hbd_conversion_requests, collateralized_conversion_requests,
decline_voting_rights_requests, limit_orders, proposals, proposal_votes, recurrent_transfers.
Plus: get_comment_pending_payouts, get_order_book, and tx utils get_transaction_hex,
get_required_signatures, get_potential_signatures, verify_authority (reuse the
sign_state we already have), verify_account_authority, verify_signatures,
is_known_transaction.

## condenser_api methods (71, the external contract)
get_version, get_active_witnesses, get_block(_header), get_ops_in_block, get_config,
get_dynamic_global_properties, get_chain_properties, get_current_median_history_price,
get_feed_history, get_witness_schedule, get_hardfork_version, get_next_scheduled_hardfork,
get_reward_fund, get_key_references, get_accounts, get_account_references,
lookup_account_names, lookup_accounts, get_account_count, get_owner_history,
get_recovery_request, get_escrow, get_withdraw_routes, get_savings_withdraw_from/to,
get_vesting_delegations, get_expiring_vesting_delegations, get_witnesses,
get_conversion_requests, get_collateralized_conversion_requests, get_witness_by_account,
get_witnesses_by_vote, lookup_witness_accounts, get_witness_count, get_open_orders,
get_transaction_hex, get_transaction, get_required_signatures, get_potential_signatures,
verify_authority, verify_account_authority, get_active_votes, get_account_history,
broadcast_transaction(_synchronous), get_account_reputations, get_ticker, get_volume,
get_order_book, get_trade_history, get_recent_trades, get_market_history(_buckets),
is_known_transaction, list/find_proposals, list_proposal_votes, find_recurrent_transfers,
find_rc_accounts, list_rc_accounts, list_rc_direct_delegations.

## Suggested build phases
- **A. Transport + globals** — JSON-RPC (HTTP+WS) framework; database_api globals
  (DGPO, config, version, hardfork, witness_schedule, feed/price, reward_funds);
  block_api (get_block[_header], get_block_range). All backed by existing state.
- **B. Account/witness/market-object reads** — database_api list/find over the
  existing typed stores + get_order_book; condenser get_accounts/lookup_accounts/
  get_witnesses/get_open_orders/get_conversion_requests/etc. Mechanical mapping.
- **C. condenser_api aggregator** — positional-arg wrappers over A+B; the
  compatibility layer external apps expect. Highest external value.
- **D. Broadcast** — network_broadcast_api / condenser broadcast_transaction;
  needs a mempool + p2p tx relay (couples with the p2p work).
- **E. Index-backed APIs (each a NEW subsystem, build as needed):**
  account_history_api (op index — heavily used via condenser.get_account_history),
  account_by_key_api (key index), market_history_api (bucket index),
  transaction_status_api (tx index), reputation_api, rc_api (RC subsystem, deferred).

Start point: **A** (transport + database_api globals + block_api) — pure exposure
of state we already compute, no new indexes, and immediately useful (block
explorers / health checks hit these first).

## Implemented status (2026-07-11) — phases A–C core

Transport DONE (internal/api/jsonrpc): JSON-RPC 2.0 HTTP + batch + legacy `call`
convention; registry keyed by api.method; named+positional param decoding.
`--api-endpoint host:port` serves it read-only. All offline-tested.

**28 methods live** across database_api / block_api / condenser_api:
- Globals: get_dynamic_global_properties, get_version, get_feed_history,
  get_current_price_feed / get_current_median_history_price, get_witness_schedule,
  get_active_witnesses, get_reward_funds / get_reward_fund, get_hardfork_properties /
  get_hardfork_version.
- Blocks: get_block_header, get_block (FULL operation→JSON expansion — reflection
  renderer covering all ~50 op types, legacy `["name",{...}]` + modern
  `{type,value}` forms). Reads the recent-block cache.
- Accounts: find_accounts / get_accounts (full account object + authorities),
  get_account_count, lookup_accounts.
- Witnesses: find_witnesses, get_witness_by_account.
- Market: get_order_book (HIVE/HBD).
- Every method exists in both database_api (NAI object assets) and condenser
  (legacy "X.XXX HIVE" strings, positional args) where applicable.

**Remaining in A–C (follow the same pattern; need small store-iteration helpers):**
list/find over proposals, conversions, escrows, delegations, savings withdrawals,
recurrent transfers; get_open_orders; get_witnesses_by_vote / get_witness_count /
lookup_witness_accounts; get_config (constants table); historical get_block via a
block_log reader (recent-cache only today).

**Out of A–C (Phase E, index-backed — each a new subsystem):** account_history_api
(get_ops_in_block, get_account_history, enum_virtual_ops, get_transaction),
account_by_key_api, market_history_api, transaction_status_api, reputation_api,
rc_api. **Broadcast (Phase D):** needs mempool + p2p tx relay.
