# drift-hunter

A "what diverges where" diff tool. Given a target block, it pulls the
same state field from both ghive (Go) and hived (C++) and prints every
disagreement.

The cycle this tool replaces:

1. Notice that account X is N satoshis short at block B.
2. Spend an hour tracing one field at a time backward through the
   chain to find the structural bug.

With drift-hunter, you discover *every* diverging field at the
suspect block in one shot — DGPO, witness_schedule, feed_history,
reward_funds, plus per-witness state for the top 21 (or any list).

## Quick start

You need a Go state snapshot at block N AND a hived process serving
its API at the same block N.

### 1. Make a Go snapshot at block N

```sh
./bin/ghive-tools replay --save-snapshot-to=./snapshots/blk-5148656 \
                         --stop-at=5148656
```

(Or, slow fallback path: skip this and pass `--go-replay=path/to/block_log`
to drift-hunter — it'll replay from genesis, which takes minutes.)

### 2. Spin up hived API at block N

```sh
scripts/hived-prepare-checkpoint.sh 5148656     # one-time per block
scripts/hived-serve-checkpoint.sh   5148656 &   # binds to :18090
```

(A separate agent is wrapping these into a single `hived-at <BLOCK>`
helper; once it lands, swap to that.)

### 3. Run drift-hunter

```sh
./bin/drift-hunter \
  --block=5148656 \
  --hived-url=http://localhost:18090 \
  --go-snapshot=./snapshots/blk-5148656
```

Output (one drift per line, greppable):

```
BLOCK 5,148,656: dgpo.current_aslot Go=5193905 hived=5194004 (Δ=-99)
BLOCK 5,148,656: witness_schedule.shuffled[03] Go="rabbit-13" hived="rabbit-18"
BLOCK 5,148,656: witness:gxt-1080-sc-0028.signing_key.hex_or_b58_marker Go="0x..." hived="STM..."
```

Last line on stderr summarizes:

```
BLOCK 5,148,656: 4 drifts (1 dgpo, 1 witness_schedule, 2 witness:gxt-1080-sc-0028)
```

## Common workflows

### Bisect over a range

Find the FIRST 100K-block window where a drift first appears:

```sh
./bin/drift-hunter \
  --range=5000000:5200000:100000 \
  --hived-url=http://localhost:18090 \
  --go-snapshot=./snapshots/blk-5148656
```

(Note: the snapshot path is fixed — for a true range bisection you'd
need one snapshot per block plus one hived per block. Today the tool
just runs the comparators against the current snapshot and current
hived for each iteration. Future work: parameterize snapshot paths
via `--go-snapshot-pattern=./snapshots/blk-{}`.)

### Filter to one table

```sh
./bin/drift-hunter --block=N ... | grep dgpo
./bin/drift-hunter --block=N ... | grep witness_schedule
./bin/drift-hunter --block=N ... | grep witness:bitbo
```

### Specific witnesses only

```sh
./bin/drift-hunter --block=N ... \
  --witness=gxt-1080-sc-0028,rabbit-13,bitbo
```

### Skip per-witness scan (faster)

```sh
./bin/drift-hunter --block=N ... --skip-witness
```

## Field coverage

| Table              | Fields covered                                                                                                                                                                                                                                  |
|--------------------|-------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `dgpo`             | `head_block_number`, `current_witness`, `current_supply`, `current_hbd_supply`, `virtual_supply`, `total_vesting_fund_hive`, `total_vesting_shares`, `total_reward_fund_hive`, `total_reward_shares2`, `hbd_interest_rate`, `hbd_print_rate`, `hbd_start_percent`, `hbd_stop_percent`, `content_reward_percent`, `vesting_reward_percent`, `proposal_fund_percent`, `reverse_auction_seconds`, `delegation_return_period`, `available_account_subsidies`, `last_irreversible_block_num`, `downvote_pool_percent`, `num_pow_witnesses`, `total_pow`, `current_aslot`, `dhf_interval_ledger` |
| `witness_schedule` | `num_scheduled_witnesses`, `next_shuffle_block_num`, `current_virtual_time`, `timeshare_weight`, `miner_weight`, `elected_or_top19_weight`, `witness_pay_normalization_factor`, `max_voted_witnesses`, `max_miner_witnesses`, `max_runner_witnesses`, `hardfork_required_witnesses`, `majority_version`, `median_account_creation_fee`, `median_maximum_block_size`, `median_hbd_interest_rate`, `shuffled[N]` (each slot)                                                                                                              |
| `feed_history`     | `current_median_history`, `market_median_history`, `current_min_history`, `current_max_history` (base + quote each), `price_history.len`, last 5 `price_history` entries                                                                       |
| `reward_fund:NAME` | `reward_balance`, `recent_claims`, `last_update`, `content_constant`, `percent_curation_rewards`, `percent_content_rewards`, `author_reward_curve`, `curation_reward_curve`                                                                       |
| `witness:NAME`     | `created`, `url`, `votes`, `total_missed_blocks`, `last_confirmed_block_num`, `pow_worker`, `virtual_last_update`, `virtual_position`, `virtual_scheduled_time`, `running_version`, `hardfork_version_vote`, `hardfork_time_vote`, `hbd_exchange_rate.{base,quote}`, `last_hbd_exchange_update`, `available_witness_account_subsidies`, `props.{account_creation_fee,maximum_block_size,hbd_interest_rate,account_subsidy_budget,account_subsidy_decay}`, `signing_key` (presence-only) |

## Adding a new comparator

Each comparator is a small function in `comparators.go` that pulls
one logical state object from both sides and emits drift records.

```go
// 1. Add the field to the hivedXxx struct in hived_state.go.
type hivedFoo struct {
    // ...
    NewField int64 `json:"new_field"`
}

// 2. Add a new comparator branch in compareXxx.
add(cmpInt64(block, "foo", "new_field", goFoo.NewField, hivedFoo.NewField))
```

For uint128/asset/uint128 fields, use `cmpBytes` plus `hivedDecimalToUint128BE`
or `cmpInt64` plus `hivedAsset.AmountInt()` respectively.

## Limitations

- `--range` mode does not currently re-load the snapshot per block.
  In practice you need one snapshot per block AND one hived per block;
  this tool would need a `--go-snapshot-pattern` flag plus a
  `--hived-restart-pattern` flag to fully automate. The single-block
  mode is the primary workflow.
- `signing_key` comparison is presence-only — Go stores raw bytes,
  hived ships base58 (`STM...`) — equivalence isn't checked, only
  whether both sides have a value.
- We don't compare per-account state, comments, or the orderbook.
  Add as needed via the comparator-extension recipe above.
- The order of `price_history` slot indexes is keyed off the *tail* in
  both sides — if the deque lengths disagree the indexed comparison
  may misalign; the `price_history.len` drift surfaces first.
