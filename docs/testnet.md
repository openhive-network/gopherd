# Running a testnet (and interoperating with hived's)

gopherd's testnet build targets **hived's testnet**, not a chain of its own. A
node built with `-tags testnet` joins a network of hived testnet nodes, syncs
from them, and produces blocks they accept.

An earlier version of this support was a bespoke gopherd-only chain — its own
chain ID, a `TST` key prefix, 21 pre-elected `init-1..init-21` witnesses. It was
self-consistent and could never interoperate: two nodes with different genesis
state disagree at block 1 no matter how correct each one is alone.

## Quick start

```sh
scripts/testnet-up            # 2 hived + 1 gopherd, 5 witnesses, all producing
scripts/testnet-status        # block-id agreement + who produced what
scripts/testnet-state-diff    # chain STATE comparison (see the warning below)
scripts/testnet-down
```

`testnet-up` needs a **testnet** hived build (`--version` must report
`"node_type":"testnet"`); a mainnet hived uses a different chain ID and will
not peer.

## Why block-id agreement is not enough

Hive block IDs hash the block header — they do **not** commit to chain state.
Two nodes can agree on every block ID while disagreeing completely about
supply, vesting and rewards.

Every consensus bug found while making gopherd join a hived testnet was
invisible to block-ID comparison and showed up only in `testnet-state-diff`:

| symptom in state | cause |
|---|---|
| `last_hardfork` 0 vs 28; reward fund non-zero vs 0 | block 1 must apply every hardfork already due by its timestamp |
| vesting shares 66,932,455,491,605 vs 51,369,685 | those hardforks must run at the **top** of block 1, or HF1's 1,000,000x vesting split scales block 1's producer reward instead of the empty genesis |
| inflation 2093 vs 6773 sat/block | genesis witness schedule must seed hived's default weights, or `witness_pay_normalization_factor` is 0 and the producer is paid 1/21 |

Always run `testnet-state-diff` after changing consensus code. Block IDs
matching proves the nodes agree on block *contents*, nothing more.

## How a hived testnet is configured

hived takes `--alternate-chain-spec=<file>`; `genesis_time` and
`hardfork_schedule` are both mandatory:

```json
{
  "genesis_time": 1451606400,
  "init_witnesses": ["wit-a", "wit-b", "wit-c", "wit-d"],
  "hardfork_schedule": [{"hardfork": 28, "block_num": 1}]
}
```

gopherd must be given the **same** witness list in the **same order**
(`--init-witness=wit-a,wit-b,wit-c,wit-d`), because genesis creates those
accounts and any difference is a genesis difference.

Key facts, all taken from a running node via `database_api.get_config` rather
than from `config.hpp` — the header and the shipped binary disagree:

- **chain ID `18dcf0a2...`**, not `sha256("testnet")` as the header says. It is
  hived's `--chain-id` *default*. gopherd accepts `--chain-id` to match a
  network started with a different one (refused on mainnet builds, where the
  chain ID is a consensus constant).
- **address prefix is `STM`**, testnet included.
- legacy asset names are **`TESTS`/`TBD`**, not `STEEM`/`SBD`. The binary NAI
  codes are identical to mainnet. Getting this wrong makes every
  legacy-serialised transaction fail with *"missing required active
  authority"* — an error that points at the key when the fault is the
  serialisation.
- initminer's key is `regenerate(sha256("init_key"))`, WIF
  `5JNHfZYKGaomSFvd4NUdQ9qMcEAC43kujbfjueTHpVapX1Kzq2n`, and every
  `init_witness` shares it.

## Electing witnesses

A fresh testnet creates its `init_witnesses` but gives them **no votes**, so
the schedule contains only `initminer` and one node produces every block.
Governance weight comes from VESTS and initminer holds the initial supply as
liquid HIVE, so:

```sh
gopherd testnet-bootstrap --witnesses=wit-a,wit-b,wit-c,wit-d --endpoint=http://127.0.0.1:8751
```

vests some of it and votes for each witness in one signed transaction.
`testnet-up` does this for you.

## Producing blocks

```
--witness-keys=FILE      # "account WIF" lines; keys are never flags (ps is public)
--produce                # real production; requires --fork-switching
```

`--produce` requires `--fork-switching` so a produced block stays reversible
and the node can reorg off it if the network picks another block for that slot.
Production is still key-gated: gopherd refuses the slot unless the chain's
registered signing key for the witness matches the key it holds.

## Known divergence

`current_supply` and `total_vesting_fund_hive` are **1558 satoshi** higher on
gopherd than on hived. The offset is established once, at block 1, and does not
grow — per-block inflation matches exactly, verified constant over many blocks
on repeated fresh chains. Because it shifts the VESTS/HIVE price slightly, the
resulting `total_vesting_shares` differ by a larger proportion once a large
`transfer_to_vesting` lands.

It does not prevent interoperation: blocks produced by either implementation
are accepted by the other, and block IDs match. It is a genuine parity gap and
`scripts/testnet-state-diff` reproduces it in seconds.
