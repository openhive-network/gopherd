# gopherd vs hived parity review (2026-10-05)

Reference: hived `develop` at 4f1961fe3 (mainnet `HIVE_BLOCKCHAIN_VERSION` 1.28.8,
HF29 defined in `hardfork.d/1_29.hf` but not scheduled). gopherd at 3276037.

Method: source-to-source comparison of seven areas (HF29 rules, RC, virtual ops,
tx validation / mempool / production, social+transfer evaluators, per-block
processing, governance+account evaluators). Every headline finding was re-read in
both trees before being listed. No replay or test run backs these; where a finding
depends on mainnet state ("if such a row exists") that is said.

Block IDs match through head, so none of this is detectable by the block-id check.
Block IDs do not commit to state, RC, pending-tx decisions or produced blocks.

Status column: `[ ]` open, `[x]` fixed in tree, `[~]` partially fixed, `[-]` won't fix / documented.

---

## A. Would split consensus or reject a valid hived block

| # | Status | Finding | gopherd | hived |
|---|---|---|---|---|
| A1 | [x] | **HF29 can never activate.** Activation is a static block table with HF29 = NeverActivates; nothing consumes `hpo.next_hardfork/next_hardfork_time`. hived activates when the witness-voted time is reached. | `internal/chain/hardforks.go:27-63`, `internal/protocol/hardfork/hardfork.go:121`, tally at `internal/chain/witness_schedule.go:508-600` | `database_hardfork.cpp:119-147` |
| A2 | [x] | **RC payer lookup incomplete.** `PayerOf` is a tag switch on `Operations[0]` only; no case for account_update2, witness_set_properties, custom, custom_binary, pow, pow2, witness_block_approve. Under HF29 (RC is consensus) a tx led by one of those is rejected ("no resource user"). hived walks every op, active→owner→posting, with three special cases. | `internal/chain/rc_cost.go:295-396`, `internal/chain/rc_phase5.go:132,159` | `rc/resource_user.cpp:10-64` |
| A3 | [x] | **RC state-bytes table wrong by orders of magnitude.** Authority member flat 200 (hived 24×43800 / 36×43800); comment `224 + permlink` (hived `96×43800 + 192×168 + 168×permlink`); vote 224 (hived 144×168); convert ×24 too many; transfer_to_vesting 0 (hived 16×720); request_account_recovery always (hived only when threshold≠0); change_recovery_account / recover_account 0; create_proposal floor vs ceil hours; recurrent_transfer ignores memo+executions; delegate_vesting_shares amount==0 case; update_proposal_votes charged on unapprove; delegate_rc extra usage missing. | `internal/chain/rc_cost.go:140-267` | `rc/resource_sizes.cpp:55-164`, `rc/resource_count.cpp:196-553` |
| A4 | [x] | **Per-tx state charge** flat 256; hived `128 × ceil(expiration hours)`. | `internal/chain/rc_count_resources.go:31,119` | `rc/resource_count.cpp:621-623` |
| A5 | [x] | **Mana floor −10 %** (`rcMaxNegativePercent`); hived clamps at 0. No such constant exists in hived. | `internal/chain/rc_phase5.go:60,148` | `util/manabar.hpp:87-95` |
| A6 | [x] | **Positive max-RC delta credited twice** on power-up / fill / delegation: `UpdateAccountAfterVestChange` adds it, then the caller adds `LastMaxRC − oldMaxRC` again. | `internal/state/objects/rc_objects.go:879,928-933`, `manabar.go:250-260` | `rc/rc_utility.cpp:468-488` |
| A7 | [x] | **RC pool dynamics** differ: block usage never subtracted from pool, weights computed differently, `pool[new_accounts]` not pinned to `available_account_subsidies`, usage not scaled by resource_unit before pooling, `usage_in_window` only hourly. | `internal/chain/rc_phase2.go:221-286,383`, `rc_phase5.go:112`, `rc_finalize.go:62-116` | `rc/rc_utility.cpp:784-827`, `rc_objects.hpp:81-95` |
| A8 | [x] | **Regenerate-before-change missing on power-up**: recipient regenerated after vests moved. hived's HF29 assert "not regenerated prior to VEST change" fires here. | `internal/chain/evaluator/transfer_to_vesting.go:112`, `create_vesting.go:193` | `database.cpp:1023,1039`, `rc_utility.cpp:410-424` |
| A9 | [x] | **Vote on paid-out post with allow_votes=false rejected.** hived checks `allows_votes` only while the cashout object exists; gopherd checks it before the paid gate. Same for `can_vote`, which gopherd checks *after* the paid-out early return (hived first). | `internal/chain/evaluator/comment.go:490-568` | `hive_evaluator_social.cpp:612-625` |
| A10 | [x] | **Beneficiary cap (8) unconditional**; hived soft-fork (`!is_in_control() || ≤8`). A hived block with 9..127 beneficiaries fails in gopherd. | `comment.go:415-417` | `hive_evaluator_social.cpp:123-124` |
| A11 | [x] | **Version / hardfork-vote tally over the wrong 21.** gopherd tallies the newly picked set; hived tallies `wso.current_shuffled_witnesses` of the object that just became current. Flips `next_hardfork_time` a round apart at a 16/17-of-21 vote. | `internal/chain/witness_schedule.go:536-548` | `witness_schedule.cpp:270-300` |
| A12 | [x] | **Scheduled-witness check advisory**; no `head_block_time < timestamp` and no `slot > 0` assert. A validly signed block at the wrong slot is accepted. | `internal/chain/database.go:795-822`, `mismatchWarn` :35 | `database.cpp:2800-2818` |
| A13 | [x] | **Threshold-0 authority** unsatisfiable in gopherd, trivially satisfied in hived. Existence on mainnet unverified. | `internal/chain/sign_state.go:86` | `sign_state.hpp:167-296` |
| A14 | [x] | **delegate_vesting_shares 0→0** on an existing amount-0 row: hived rejects (`delta >= min_update`), gopherd deletes the row. Also minima asserts skipped when `!InControl` citing a few-sat vesting-price residue — if real, that residue is itself a dgpo divergence. | `internal/chain/evaluator/vesting.go:254-293,525` | `hive_evaluator_transfer.cpp:800-833` |
| A15 | [ ] | **delegate_rc errors during block apply** in some paths; hived swallows custom-op exceptions when not in control. | `internal/chain/evaluator/custom_json_rc.go:280,287,313` | `hive_evaluator.cpp:355-370` |
| A16 | [x] | **Version constant 1.28.7**; hived 1.28.8 (2026-10-05). After HF29 hived rejects blocks advertising 1.28.x; must be bumped with A1. | `internal/protocol/network/network_mainnet.go:52-54` | `config.hpp:139` | Follow-through: `hardfork.HasPlaceholderTime` + the HF29 RELEASE CHECKLIST in hardfork.go; the witness never votes a placeholder time; `TestHardforkReleaseConsistency` fails if the version and the time move apart. |
| A17 | [x] | **RC charged on the wrong clock.** Transaction RC (regeneration and the per-tx expiration-hours charge) used the NEW block's timestamp; hived's finalize_transaction runs before update_global_dynamic_data, so head_block_time() is the PREVIOUS block's. Every payer was credited three extra seconds of regeneration per charge. Found while double-checking A4. | `internal/chain/database.go` (applyRCForTx call), `parallel.go`, `mempool_select.go` | `rc/rc_utility.cpp:527,841`, `database.cpp` _apply_block order |

## B. Would produce / relay what hived rejects (security and mempool)

| # | Status | Finding | gopherd | hived |
|---|---|---|---|---|
| B1 | [x] | **Unsigned recover_account / witness_set_properties accepted.** `other` authorities (new+recent owner authority; witness signing key) are never collected; with all sets empty verification returns nil. Evaluators only compare public data. | `internal/chain/verify_authority.go:156-164,252-260` | `transaction_util.cpp:124-127`, `hive_operations.hpp:475-489,851-855` |
| B2 | [x] | **Post-HF28 role upgrade still allowed** (owner for active, active/owner for posting). hived forbids it when `allow_strict_and_mixed_authorities`. | `verify_authority.go:299-313` | `transaction_util.cpp:84-150` |
| B3 | [x] | **pow / pow2 no deprecation gates** (HF13 / HF17) and no work validation; pow2 with new_owner_key needs no signature and creates accounts + witnesses feeding the miner slot. | `internal/chain/evaluator/pow.go:64-155` | `hive_evaluator.cpp:498-577` |
| B4 | [ ] | **Admission does not trial-apply**; hived applies evaluator + RC before relaying. Selection newest-first and permanent eviction of failures (hived FIFO, re-applies pending after each block). Pending lifetime 24 h vs hived's `min(expiration, first_seen+1h)`. | `internal/chain/mempool_select.go:226-382`, `internal/plugins/witness/witness.go:1521-1609`, `tx_validation.go:63-70` | `database.cpp:624-668,2546-2563`, `block_producer.cpp:162-207`, `db_with.hpp:106-160` |
| B5 | [ ] | **Production slot**: only slot 1 considered, no 750 ms lag cut-off, no 500 ms early start, no 33 % participation gate. | `witness.go:918,968,1051,1085` | `witness_plugin.cpp:310-355,469-506` |
| B6 | [x] | limit_order_create/create2 HF20+ 28-day expiration cap missing (comment claims it exists). | `internal/chain/evaluator/limit_order.go:38-45,216` | `hive_evaluator_transfer.cpp:548-551,588-591` |
| B7 | [x] | withdraw_vesting HF28 "rate would not change" missing on the non-zero path. | `vesting.go:98-145` | `hive_evaluator_transfer.cpp:387-389` |
| B8 | [x] | set_withdraw_vesting_route: `HIVE_MAX_WITHDRAW_ROUTES` (10) not enforced; no route counter. | `vesting.go:148-197` | `hive_evaluator_transfer.cpp:409-410` |
| B9 | [x] | custom / custom_json / custom_binary: no protocol validate (auth count, id ≤ 32, JSON validity), no HF26 8192-byte caps, custom_binary accepted (hived rejects). | `internal/chain/evaluator/stubs.go:36-38`, `custom_json_rc.go:30-50`, `internal/protocol/op/validate*.go` | `hive_evaluator.cpp:328-379`, `hive_operations.cpp:412-447` |
| B10 | [~] | comment: HF21 3 s edit interval + `last_post_edit`; parent-change-on-edit check; permlink char class; json_metadata validity/UTF-8; depth soft limit. | `comment.go:86-262`, `internal/protocol/op/validate.go:43-51,147-179` | `hive_evaluator_social.cpp:186-308`, `hive_operations.cpp:135-136` |
| B11 | [x] | comment_options beneficiaries gate uses `AbsRshares != 0`; hived uses `was_voted_on` (dust votes count post-HF26). | `comment.go:410-414` | `hive_evaluator_social.cpp:121` |
| B12 | [x] | feed_publish HF20 orientation (base HBD / quote HIVE) not enforced in evaluator. | `internal/chain/evaluator/feed_publish.go:20-40` | `hive_evaluator.cpp:599-605` |
| B13 | [x] | witness_update validate: props unchecked (fee 1..1e9, block size range, interest ≤ 100 %), symbol error swallowed; HF14 canon fee symbol. witness_set_properties: fee 0 accepted (min 1); sbd_*/hbd_* key preference reversed; decode failures swallowed. | `validate.go:208-224`, `validate_more.go:421-455`, `internal/chain/evaluator/witness.go:275-336` | `hive_operations.cpp:276-296`, `hive_evaluator.cpp:66-69,142-178` |
| B14 | [x] | transfer memo allows 2048 (hived < 2048) and no UTF-8 check. | `validate.go:105-123` | `hive_operations.cpp:241-249` |
| B15 | [x] | account_update / account_update2: temp account not rejected; HF20 `validate_auth_size` missing in account_update; accounts-exist missing in update2; update2 validate minimal. account_update2 writes the null memo key (hived ignores sentinel). | `internal/chain/evaluator/account_update.go:75,166,195-269,220-222` | `hive_evaluator_account.cpp:299-373`, `hive_operations.cpp:105-121` |
| B16 | [x] | account_witness_proxy: both "Proxy must change" asserts missing. | `witness.go:536-592` | `hive_evaluator.cpp:218,254` |
| B17 | [~] | update_proposal_votes: no `can_vote` check. create/update_proposal: no permlink→comment existence check. update_proposal has no Validate (negative daily_pay accepted). | `internal/chain/evaluator/dhf.go:66-156,171-183,313-343` | `dhf_evaluator.cpp:67-72,113-118,167`, `dhf_operations.cpp:25-38` |
| B18 | [x] | request_account_recovery: `is_impossible`, `weight_threshold != 0`, accounts-exist, unconditional auth-size missing; threshold 0 with no request succeeds. recover_account: `new != recent` / impossibility not re-checked. No Validate for the three recovery ops. | `internal/chain/evaluator/recovery.go:120-235` | `hive_evaluator_account.cpp:480-540`, `hive_operations.cpp:727-736` |
| B19 | [~] | create_claimed_account: accounts-exist missing. claim_account subsidy check applied at admission (hived only when processing a block). AccountCreate/ClaimAccount/… validate gaps (auth validity, auth size, fee max, json). | `account_create_variants.go:61-82,110-185`, `validate.go:232-246`, `validate_more.go:205-317` | `hive_evaluator_account.cpp:420-433,493-495`, `hive_operations.cpp:45-92` |
| B20 | [ ] | sign_state edge cases: `visited` set rejects a cycle hived accepts; account_auth_count counts followed auths > 125 vs hived top-level ≥ 125 with memoised approvals; >40-entry authority pre-rejected. HF28 flag read from `dgpo.HeadBlockNumber` in one place and `blockNum` in another. | `sign_state.go:70-141`, `verify_authority.go:277-280`, `tx_validation.go:53` | `sign_state.hpp:78-258` |

## C. State drift (needs state to confirm whether it has fired)

| # | Status | Finding | gopherd | hived |
|---|---|---|---|---|
| C1 | [x] | **update_proposal ignores HF25 `update_proposal_end_date` extension**; shortened proposals keep paying and stay votable. Both HF-gated extension asserts missing. | `dhf.go:313-343` | `dhf_evaluator.cpp:123-150` |
| C2 | [x] | **DHF tick with zero active proposals returns before refreshing `last_budget_time`**; hived refreshes hourly. Next paying tick over-scales (gopherd clamps at 86400, hived has no clamp). `total_votes` of not-yet-active proposals never recomputed. | `internal/chain/proposals.go:155,205-232` | `util/dhf_processor.cpp:244-256` |
| C3 | [x] | **LIB uses current schedule**; hived HF26+ uses the future one. | `internal/chain/lib.go:28-47` | `database.cpp:2928-2934,3022-3028` |
| C4 | [ ] | HF28 migration deletes revoked voters' proposal votes immediately; hived stages decline requests processed under the 200 budget. | `hardforks.go:448-451,516-553` | `database_dhf.cpp:147-262` |
| C5 | [ ] | HF23 clear_account misses savings withdrawals where the cleared account is the **recipient** (poloniex, steemit are in the list). | `internal/chain/hardfork_accounts.go:~330-360` | `database_market.cpp:182-229` |
| C6 | [ ] | HF28 inflation base `floor(A·p) − floor(T·p)` from maintained VirtualSupply vs hived single floor on a fresh recompute. | `internal/chain/funds.go` (processFundsHF16, HF28 block) | `database.cpp:1744-1751` |
| C7 | [ ] | recurrent transfer tie-break `(trigger, from, to, pair_id)` vs hived `(trigger_date, object id)`; decides which fails on insufficient balance. | `internal/chain/recurrent_transfers.go:62-74` | `database_dhf.cpp:42-50` |
| C8 | [-] | Tie-breaks approximated by `(Created, Name)` instead of account id: curator payout order, governance expiry order. Known; consensus-relevant only for same-block-created equal-weight curators or once the 200 budget is hit. | `comment_cashout.go:1266-1300`, `governance_votes.go:95-124` | `database_comment.cpp:129-139`, `database.cpp:3502-3545` |
| C9 | [x] | transfer HIVE→hive.fund rounding to 0 HBD still adjusts VirtualSupply (hived skips when 0 minted). Intra-block only. | `internal/chain/evaluator/transfer.go:124-134` | `global_property_object.hpp:335-348` |
| C10 | [x] | account_create by `temp` pre-HF11 → recovery "" vs hived "steem" (changes who may recover today). | `account_create.go:25-33` | `hive_evaluator_account.cpp:136-144` |
| C11 | [ ] | `ctx.BlockNum` (N) used where hived reads `head_block_num()` (N−1) during tx apply: pow thresholds 201600/864000, owner-history gate 3186477, pre-HF20 shutdown threshold. Single-block boundaries, historic. | `pow.go:194-235`, `account_update.go:166,245`, `database.go:1605` | `hive_evaluator.cpp:482-486`, `database.cpp:1703`, `database_witness.cpp:296` |
| C12 | [ ] | Pre-HF28 power-down with zero modulo chunk cancels instead of scheduling a zero fill; pre-HF25 period_pay==0 skips `adjust_balance(0)` interest accrual. Historic/dead. | `vesting_withdrawals.go:~175`, `proposals.go:~265` | `database_vesting.cpp:126-127,265-276`, `dhf_processor.cpp:175-218` |

## D. Virtual operations

Tags 50..92 and FC_REFLECT field order of all 43 vop structs match. No extra vops.
Totals: 20 matching, 7 divergent, 16 never emitted. No API exposes the stream
(`LastBlockVOps` has no caller; `op_json.go` has no virtual-tag cases); replay has
`--emit-vops` off by default and parallel apply drops evaluator vops silently.

| Status | Missing (hived site) |
|---|---|
| [x] | NOW EMITTED: fill_vesting_withdraw, fill_order, limit_order_cancelled (both paths), fill_collateralized_convert_request, collateralized_convert_immediate_conversion, changed_recovery_account, delayed_voting, vesting_shares_split, ineffective_delete_comment, clear_null_account_balance, consolidate_treasury_balance, hardfork_hive_restore, liquidity_reward. ALSO NOW EMITTED: interest (via objects.OnInterestPaid hook), hardfork_hive, account_created for genesis accounts and the HF21/HF24 treasuries. STILL MISSING: system_warning |

| Status | Divergent |
|---|---|
| [x] | producer_reward pre-HF16 liquid path never emitted (`funds.go:238-243`; `database.cpp:1853-1859`) |
| [x] | account_created missing for genesis accounts, steem.dao at HF21, hive.fund at HF24 |
| [x] | dhf_conversion missing from the transfer-evaluator path (`transfer.go:98-125`; `hive_evaluator_transfer.cpp:289`) |
| [ ] | comment_benefactor_reward skipped for zero-share / missing-account / no-feed beneficiaries (`comment_cashout.go:641-666`) |
| [-] | effective_comment_vote.pending_payout 0 (hived's AH plugin supplements it; consensus value matches) |
| [-] | comment_reward running totals pre-HF17 (documented at `comment_cashout.go:820`) |
| [ ] | shutdown_witness / pow_reward one-block boundary (see C11) |

## E. Housekeeping

| Status | Item |
|---|---|
| [ ] | Untracked `cmd/dgpodump`, `cmd/scanbenef` import `hived-go/...`; `go build ./...` fails. |
| [ ] | Stale comments: `database.go:194-202,623-631` claim ApplyBlock enforces the custom-op block limit (it does not; `enforceCustomOpBlockLimit` has no caller); `hardfork29.go` / `hardfork_testnet.go` claim mainnet does not register HF29 (hived registers it unconditionally). |
| [~] | rc_stats period fixed to 28800 blocks; `rcFinalizeBlock` runs after `processHardforks` (hived before; HF20 block only); `RCDelegationExpiration` 5-day refund table has no hived counterpart and no caller. |
| [x] | Early-block hold cap (hived 0b6f8d091) already ported (`internal/p2p/sync/early_block.go:32`). |
| [x] | `GlobalRemoveThreshold = 200` is faithful: hived's `set_remove_threshold` has no non-test caller. |

## Confirmed matches (where there was reason to doubt)

HF29 ALLOW_MODIFY_LAST_RECURRENT_TRANSFER, FIX_LIMIT_ORDER_ASSETS,
VALIDATE_ALL_PROPOSAL_IDS incl. the 2026-09-01 remove_proposal rewrite and the
per-block removal budget; RC compute_cost / curve params / regen arithmetic /
differential usage / HF20 seeding; HF20+ vote math incl. HF26 edit recompute;
withdraw intervals; limit-order match/fill rounding; process_funds HF16+ splits;
update_virtual_supply; update_median_feed; conversions; vesting withdrawal routes;
comment cashout curves and payout order; witness schedule shuffle, pay
normalisation, median props, HF26 rotation; DHF budget/period_pay/convert_funds;
TaPoS, expiration, canonicality, HF28 redundant-signature rule, chain-id switch;
ba899e6 signature bypass fix is complete.

## Unresolved without state or a replay

- Does any mainnet account have a threshold-0 authority (A13)?
- Did `temp` create accounts pre-HF11 (C10)? Any pow op in block 201600/864000, owner rotation in 3186477 (C11)?
- Was any HF23-cleared account the recipient of a pending savings withdrawal (C5)?
- Did any mainnet update_proposal carry the end-date extension (C1)? Did the first post-HF21 DHF tick see passed_time > 3600 in gopherd (C2)?
- Does gopherd's `computeDecayPerTimeUnit` reproduce hived's jsonball conversion bit-for-bit?
- Does hived's own replay ever trip the HF29 `last_max_rc` asserts that gopherd counts instead of throwing?
