# Testing gopherd

gopherd is a second implementation of a consensus protocol. A bug rarely shows
up as a crash. More often the node computes something a little differently from
hived, and the two disagree about a block, possibly months later. The test suite
is built around that risk. It has five tiers, each slower than the one before.
All tiers except the replay need nothing but the source tree.

| Tier | Command | Time | Needs |
|---|---|---|---|
| Gate: format, vet, `go mod tidy`, unit tests | `make check` | ~2 min | Go, libzstd-dev |
| Race detector | `make test-race` | ~4 min | ~7 GB RAM |
| Fuzzing, every target | `make fuzz FUZZTIME=30s` | ~15 min at 30 s | |
| Replay checkpoints | `GOPHERD_BLOCK_LOG=… make test-replay` | ~3.5 min for 10M blocks | a mainnet `block_log` |
| Full replay, genesis to head | `gopherd replay --check-invariants=1000000 …` | ~5.5 h | `block_log`, ~20 GB RAM |

Run `make check` before every push; CI runs the same command. Build
prerequisites are in the README (libzstd and pkg-config: the block decoder
uses cgo).

## Unit and parity tests (`make test`)

There are about 1,400 test files. A few kinds deserve a note:

- **`internal/chain/testfixture`** ports hived's own C++ unit tests (`*_cpp`
  names). Each test builds a chain from genesis, activates hardforks, funds
  actors, pushes operations through the full block-apply path, and asserts what
  hived's test asserts. Use `testfixture.New(t)` for a test. Use `NewShared(tb)`
  when one state should serve many inputs, as in a fuzz target or benchmark.
- **Parity tests named after a hived source line** (`database.cpp:1517`, …) pin
  one behaviour to hived's. The test comment says which divergence the test
  would have caught. When you change consensus code, find the hived code it
  mirrors and cite it the same way.
- **Seed round-trip tests** (`TestXxxSeedsRoundTrip`) check that every mainnet
  encoding in a fuzz corpus decodes and re-encodes byte for byte.

## Fuzzing (`make fuzz`)

`scripts/fuzz.sh` finds every `Fuzz*` function and runs each one for
`FUZZTIME`. `make test` also replays every checked-in corpus, without fuzzing.
The targets cover all input that a peer or an API client controls, plus the
arithmetic that consensus depends on:

| Target | Package | Property |
|---|---|---|
| `FuzzReader`, `FuzzVarUint` | fcraw | No read sequence panics, moves backwards, or overruns the input. |
| `FuzzReadOp` | protocol/op | Decoding never panics. The decoded value is stable under re-encoding. |
| `FuzzReadSignedTransaction`, `FuzzReadBlockTxCount` | protocol/tx | Same as above. Byte ranges stay inside the input. A declared transaction count is bounded by the bytes left. |
| `FuzzUnmarshalSignedHeader`, `FuzzParseHeaderExtensions` | protocol/block | Same as above. |
| `FuzzReadAuthority` | protocol/auth | A decoded authority is in fc's canonical form: sorted, with repeats dropped. |
| `FuzzBlockBody` | chain | The block receive path (merkle check, transaction split, signer recovery) never panics, and its parts agree. |
| `FuzzDecodeMessage`, `FuzzMessageUnmarshal` | p2p/proto | The envelope and all 15 p2p message types never panic and are stable under re-encoding. |
| `FuzzReadAssetAndPrice`, `FuzzParseLegacyAsset`, `FuzzAssetJSON` | protocol/asset | Binary, legacy-string and JSON forms all round-trip. |
| `FuzzDecodePublicKey`, `FuzzDecodeWIF` | crypto | Keys round-trip. |
| `FuzzDecompress` | blocklog/zstd | No crash in libzstd, and output never exceeds the 16 MiB cap. |
| `FuzzApplyOp` | chain/testfixture | Any operation applied to a funded state, under five hardfork eras: no evaluator panics, and a valid operation conserves HIVE, HBD and VESTS. |
| `FuzzAPIDispatch`, `FuzzAPIBody` | api | Arbitrary parameters to every JSON-RPC method, and arbitrary request bodies: nothing panics. |
| `FuzzRegenerateManaMatchesHived` | state/objects | Matches a `math/big` transcription of hived's `regenerate_mana` exactly. |
| `FuzzMulDivExact` | chain | Matches the exact truncated quotient whenever the result fits in int64. |
| `FuzzU128MatchesBig`, `FuzzApproxSqrt` | math128 | Every 128-bit operation matches `math/big`. The square root stays within fc's bounds. |

**Why most properties are "stable" rather than "round-trips exactly".** Like
fc, gopherd's decoders accept non-minimal varints. They also decode `flat_set`
and `flat_map` members the way fc does, by inserting them, which sorts them
and drops repeats. Both hived and gopherd hash transactions and blocks as they
were received (`full_block.cpp:145`, `full_transaction.cpp:72`), so no
consensus value depends on re-encoding. The mainnet seeds must still
round-trip exactly.

**Seeds** come from mainnet. `internal/fuzzseed` regenerates them from a
`block_log`:

    GOPHERD_FUZZSEED_BLOCK_LOG=/path/to/block_log go test ./internal/fuzzseed

It writes one operation per operation type, one transaction per leading
operation type, headers from every million blocks, and small blocks from every
five million. `FuzzApplyOp` rewrites the accounts in those operations onto its
own fixture accounts, so fuzzing starts inside the evaluators.

**When the fuzzer finds something**, it writes the input to
`testdata/fuzz/<Target>/`. Fix the bug and commit the input together with the
fix. `go test` then replays it from that point on. If the finding comes from a
wrong property rather than a bug, fix the property. Keep the input only if it
is a useful seed. The header corpus must stay mainnet-only, because every entry
there must round-trip exactly.

## Race detector (`make test-race`)

Run with the race detector, tests use 5–10 times their normal memory. The chain
fixture package alone peaks at 6.5 GB even with one test at a time, so the
defaults are `RACE_P=1 RACE_PARALLEL=1`. On a machine that is also replaying,
run it inside a memory-capped scope so the kernel kills the tests and not the
replay:

    systemd-run --user --scope -p MemoryMax=9G make test-race

## Replay checkpoints (`make test-replay`)

This test replays the start of a mainnet `block_log`, 10M blocks by default
(`GOPHERD_REPLAY_TO` changes it). Every 250,000 blocks it requires two things.
First, hived's conservation invariants must hold. Second, the chain totals
must match `test/replay/checkpoints.json`. The totals are the head block id,
the HIVE, HBD and virtual supply, the vesting pool, the reward fund, and the
total reward shares.

The recorded values are a **regression baseline**, not an independent check
against hived. They come from a gopherd build whose full replay agreed with
hived. If a change moves them, confirm the new values against hived before
rewriting the file:

    GOPHERD_BLOCK_LOG=… go test -tags replay ./test/replay -run TestReplayCheckpoints -update

## Full replay

The strongest check gopherd has is a replay from genesis to the head of a
mainnet `block_log` with invariants checked along the way:

    gopherd replay --data-dir=… --check-invariants=1000000 --check-invariants-stop=false \
        --mem-limit-gib=12 --cold-backend=pebble --cold-dir=… /path/to/block_log

It must end with `status: PASS` and `block errors: 0`. Every progress line also
reports pebble's memory use, the number of sessions the GC had to clean up
(should stay 0), and process memory outside the Go heap. If any of these grows
steadily, look at it before it causes an OOM. `GOPHERD_RC_ATTRIBUTION=1` breaks
the summary's RC diagnostics (`rc stale max`, `rc not-regen`) down by call
site. Those counters measure where gopherd's RC bookkeeping order differs from
hived's. Both must stay 0: a full replay to block 62,251,407 (commit ecc10a3,
6 h 11 min, about 14 GB resident) reported neither, with 0 invariant failures
and 0 sessions cleaned up by the GC. A non-zero count is a regression.

## CI

`.gitlab-ci.yml` runs `lint`, `unit` (with coverage), `testnet` (the
`-tags testnet` build), `race`, `fuzz-smoke` (15 s per target) and `build` on
merge requests and on master. A scheduled pipeline also runs `fuzz-long`
(10 minutes per target). Jobs use `golang:1.26-bookworm` on the hive group's
`public-runner-docker` runners. The `race` job needs about 7 GB.

## Known gaps

These are the most valuable places to add tests:

- **No hived oracle.** Apart from the replay checkpoints, which are recorded
  from gopherd itself, nothing runs hived next to gopherd. The RC daily stamps
  that HF29 commits have not been compared with hived's.
- **Evaluator coverage** is about 25% of statements. `FuzzApplyOp` reaches
  many more paths, but it only checks for panics and conservation, not for
  behaviour.
- **`regenerate_mana` where hived throws.** For a regeneration above
  INT64_MAX, gopherd saturates where hived throws. This is reachable only by an
  RC bar of about 1e17 that is untouched for over 2.5 years. Applying blocks
  never meets it, but a gopherd witness could admit such a transaction.
- **`fixed_string` account names.** hived truncates names at an embedded NUL
  and refuses names longer than 16 bytes when it decodes them. gopherd keeps
  them as strings and relies on later validation.
