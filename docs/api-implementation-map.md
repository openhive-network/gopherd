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

## Implemented status (2026-07-11) — phases A–C

Transport DONE (internal/api/jsonrpc): JSON-RPC 2.0 over HTTP + batch + legacy
`call` convention; registry keyed by api.method; named + positional param
decoding; `--api-endpoint host:port` serves it read-only (offline-tested).

**49 methods live** across database_api / block_api / condenser_api — the full
read surface of A–C that is backed by existing state:
- Globals/config: get_dynamic_global_properties, get_config, get_chain_properties,
  get_version, get_feed_history, get_current_price_feed / median, get_witness_schedule,
  get_active_witnesses, get_reward_funds/fund, get_hardfork_properties/version,
  get_next_scheduled_hardfork.
- Blocks: get_block_header, get_block (FULL operation→JSON, all ~50 op types,
  reflection renderer; legacy + modern forms).
- Accounts: find_accounts/get_accounts (full object + authorities), get_account_count,
  lookup_accounts, get_owner_history, get_recovery_request.
- Witnesses: find_witnesses, get_witness_by_account, get_witness_count,
  lookup_witness_accounts, get_witnesses_by_vote.
- Market/DHF/expiring-object list-find: get_order_book, get_open_orders,
  get_conversion_requests, get_collateralized_conversion_requests,
  get_vesting_delegations, get_savings_withdraw_from/to, find_recurrent_transfers,
  get_escrow, list_proposals, find_proposals.
- Every method in both database_api (NAI assets) and condenser (legacy strings,
  positional) where applicable.

**Not done — but each is a Phase D or Phase E dependency, NOT A–C read work:**
- Need **JSON→operation parsing + mempool** (Phase D): broadcast_transaction(_synchronous),
  verify_authority, verify_account_authority, get_required_signatures,
  get_potential_signatures, get_transaction_hex. (The op→JSON renderer exists; the
  inverse parser + a mempool are the Phase-D build.)
- Need a **new index subsystem** (Phase E): account_history_api (get_ops_in_block,
  get_account_history, enum_virtual_ops, get_transaction), account_by_key
  (get_key_references), market_history (ticker/volume/trades/buckets),
  transaction_status (is_known_transaction, find_transaction), reputation
  (get_account_reputations), rc_api, get_active_votes (comment-vote index),
  list_proposal_votes (proposal-vote index).

So phases A–C (transport + read exposure of existing state + condenser aggregation)
are substantially complete; the remaining condenser methods are gated on the
Phase D (broadcast) and Phase E (index) subsystems by design.


## Phases D & E status (2026-07-11)

**Phase D (broadcast / tx utilities) — DONE (safe subset):**
- JSON→operation parser (op_parse.go): inverse of op→JSON, 46 wallet-broadcast op
  types via a prototype registry + reflection populator.
- get_transaction_hex, verify_authority, get_potential_signatures,
  get_required_signatures — reuse the chain's RequiredAuthorities + VerifyAuthority
  + sessionAuthFetcher.
- broadcast_transaction / _synchronous / network_broadcast_api.broadcast_transaction —
  parse + authority-validate + park in an in-memory Mempool. **Relay is deliberately
  NOT wired** (a network write must never hit live mainnet — the never-touch-mainnet
  rule); enabling relay is a future opt-in gated off mainnet.
- is_known_transaction — mempool + the applied-tx dedup window (objects.HasTransaction).

**Phase E — the parts backed by existing state are DONE:**
- account_by_key.get_key_references — on-demand authority scan (no index).
- get_ops_in_block — real ops from a parsed block (virtual ops omitted).
- get_active_votes — comment votes via RangeCommentVotesByComment.

**Phase E — remaining methods each need a NEW apply-time index subsystem (not built):**
- account_history_api.get_account_history / enum_virtual_ops / get_transaction — a
  per-account op index; ALSO blocked on gopherd emitting the full virtual-op stream
  (only a subset is emitted today). The biggest + highest-value; multi-day.
- market_history_api (ticker/volume/trade_history/market_history buckets) — a trade
  + OHLC-bucket index fed from fill_order.
- transaction_status_api.find_transaction — a tx_id→block index (the dedup window
  stores tx_id+expiration but not the block).
- reputation_api.get_account_reputations — a reputation index fed from vote rshares.
- rc_api — the whole RC consensus subsystem (separately deferred).

These are substantial subsystems (apply hooks + persistence + replay-time build);
the API methods are wired to slot in once each index exists.

**Total: 64 JSON-RPC methods across 7 APIs (database / condenser / block /
account_by_key / account_history / network_broadcast). All offline-tested; the
API is read-only except broadcast, which parks locally and never relays.**

## Scope correction — HAF / Hivemind boundary (2026-07-11)

In the modern Hive stack the CONSENSUS NODE (hived/gopherd) does NOT serve the
heavy history/social/market APIs — those are supplanted by the HAF stack, which
reads the node's block stream into Postgres and serves them from separate apps:
- **HAFAH** → account_history_api (get_account_history, enum_virtual_ops,
  get_ops_in_block, get_transaction).
- **Hivemind** → social/discussion/tags/follow + reputation_api + much of
  condenser's content/discussion surface (get_content, get_discussions_by_*,
  get_followers, get_active_votes reputation, etc.).
- **HAF market service** → market_history_api (ticker/volume/trades/buckets).

Therefore these are **out of scope for gopherd** — do NOT build them as node
subsystems. gopherd's job is the consensus-backed core the node owns:
- database_api, block_api, network_broadcast_api, condenser_api (consensus subset),
  account_by_key_api, transaction_status_api, rc_api — plus emitting a clean
  block/vop stream that HAF can ingest.

Net effect on the earlier "Phase E remaining" list: account_history, market_history,
reputation are **HAF/Hivemind's job, not gopherd's** — drop them. What genuinely
remains for the node itself: **rc_api** (needs the RC subsystem port) and any
polish on the consensus-core APIs already built. (The get_ops_in_block /
get_active_votes we added are harmless conveniences, but HAFAH/Hivemind are the
real source for those in production.)
