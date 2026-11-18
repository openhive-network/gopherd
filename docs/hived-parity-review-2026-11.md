# gopherd vs hived: changes since the October review (2026-11-19)

Reference: hived `develop` 4f1961fe3..60baf76bd (2026-10-05 to 2026-11-18,
302 commits, mainnet `HIVE_BLOCKCHAIN_VERSION` 1.28.9, HF29 still at the
2100-01-01 placeholder). The previous review is
[hived-parity-review-2026-10.md](hived-parity-review-2026-10.md); start the next
one from 60baf76bd.

About 45 of the commits touch consensus, block validation or p2p; the rest are
RocksDB, snapshot, CI, docs, wallet and API-documentation work with no gopherd
counterpart. Each relevant commit was read against the gopherd code it would
affect. The table lists what changed in gopherd; the second table lists the
commits that needed nothing.

## Ported

| hived | What | gopherd commit | Gate |
|---|---|---|---|
| 30b411b94 | Bound a block's tx count by its size before decoding. gopherd had the bug more widely: one 15-byte `trx` (op count) or one block (tx count) from any peer made it reserve 64-200 GiB and die with a fatal out-of-memory. All wire counts are now bounded. | 62fa394 | always |
| 536d41ea6 | Check a block's merkle root before fork_db. In gopherd a bad-body copy under a genuine id wedged the head (hive#982), and catch-up applied block bodies unchecked (`SkipBlockSignature` skipped `validateBlock`). Body and id are now checked on the receive path for every block (60 us/block at 62M), the sender disconnected; failed blocks leave the fork DAG; p2p applies check `previous`. | 7123654 | always |
| f4da286a3, 150a8dc67 | remove_proposal checks every id's owner and range once majority_version reaches 1.28.9, in control now; lookups use the id's low 32 bits. Version 1.28.9. | 479aa4f | majority 1.28.9 / in control / HF29 |
| — | reset_account / set_reset_account had no validator and no authority, so an unsigned one was admitted (pre-existing). | b8758bf | admission |
| 366bc0533 | Downvote mana delta in 128 bits. | ff9a724 | HF29 |
| 86d893b0b | `daily_pay` cap at creation; HF29 clamps every proposal, #0 included. | d66b9f2 | in control / HF29 |
| 857016bfc | Version and hardfork-vote tally with 0.N spelled 1.N. | bad9e88 | from 2026-11-12 (active) |
| 7ae0cbdfb, 125882c23 | Pruned peers: hello carries `last_known_block_*`; a peer below our servable tail is rejected at the hello or disconnected with `closing_due_to_error`; such a drop after the handshake counts as a failed dial. Replaces be205e1's "nothing to give" reply. See p2p-vs-hived.md row 12. | 5fe3b49 | always |
| 7a52444f0 | A repeated id in one fetch_items is served once. | 9e4f9bd | always |
| c8fd68ff5 | Keep the duplicate check (and its records) for blocks younger than 25 h under `SkipDedupAndTaPOS`, so a node fresh from `gopherd replay` refuses duplicates. | ef97f8a | always |
| 86e15d046 | Refuse a fast-confirm for a block more than 3 above head. | c0e6741 | always (local) |
| 67a927815, 5caa8250d | Price feed more than 100× from the market median refused. | 6a8de14 | in control |
| 6e9ce8d7e | recurrent_transfer `pair_id` extensions in both JSON forms (gopherd rendered them as hex). | aee459b | API |
| 5af1b8a9c, 0afaf331f | A hello carrying our own connection key is refused as connected_to_self and the endpoint never dialled again. | d061d02 | always |
| 1cf2455d8, e2100feda, 18045a0ee, 450441091, e78bfabd1, a5edbbf29 | State stamps: header extension tag 3 (header extensions are a flat_set compared by tag only, first of a tag wins), the published-stamp list from genesis, HF29 validation and production. | ef966cf..f362d49 | stamps every node from HF20; extension HF29 |
| 7cc13d0af, 7baaad338, 1311a2389 | `delegate_rc_operation` at tag 37 (HF29); STRICT_RC_DELEGATION_RULES; the A15 block-time skips end at HF29; max-RC overflow refused. | c932812 | HF29 / in control |
| 7cc13d0af (REVERT_RC_USAGE) | Embedded delegate_rc RC charged as hived does: nothing before HF26 or on validate/authority failure, discount per op against the state left by earlier ops, usage only after success; from HF29 a failed op's usage is reverted. **This changes historical RC mana** for "rc" custom_jsons (see below). | c23833c | HF26 / HF29 |
| — | Mempool selection cleared the per-tx RC accumulator after the trial instead of before (pre-existing). | ec14828 | production |
| 64af0744d | Over-long producer name refused before the fork DAG. | b4e97cb | always |

## Before HF29 activates

- **Every existing gopherd state needs a replay from before HF20 (block
  26,256,743).** The first HF29 block commits the merkle root of every daily RC
  stamp published since HF20; states written before ef966cf never kept them. Such
  a state warns at the next RC report and refuses to validate or produce HF29
  blocks with an explicit error rather than diverging silently.
- **Compare gopherd's daily RC stamps with hived's** over a range of report
  blocks. Any RC mana difference since HF20, including the one c23833c fixes,
  makes gopherd reject the first HF29 block. Not yet measured.
- `TestHardforkReleaseConsistency` fails under `-tags testnet` (testnet reports
  1.29 with HF29's placeholder time); it fails without this round's changes too.

## No gopherd change needed

| hived | Why |
|---|---|
| 507216d83 | Embedded delegate_rc reported as separate op notifications: account history / HAF only. |
| 23d1bd349, c7e6be59a | delegate_rc max_rc above INT64_MAX: gopherd already rejects it; only the message differs. |
| 595d7eb69, d616f35be | Error messages / metadata only. |
| 5b0aa4b34, 57f45a932, 00d019e1e | forward_keyauths (HAF): no counterpart. |
| fb6bd7f83, feb4cf1bd | Authority-getter refactor; verify_signatures is not implemented in gopherd. |
| 169fd8249 | C++ data race on cached_lib. |
| 7fcadc28b, 70f8afe61, 45d7e1827 | Pruned-reader plumbing; gopherd's synopsis never starts below its ring. |
| bd71db6b7 | gopherd's block_log writer has no split parts. |
| 4e77eb83c, 3a07bc9ef | Duplicate-connection handling: gopherd's node id is per connection, so hived never sees duplicates or sends `already_connected`. Required if gopherd adopts a stable node id. |
| f76bde64b, 65f1aad59, 6f522aa2e, 31bba7076, 88979cc33 | Already behave as the new hived. |
| abfd619a5, ebd9dd933 | Block-source deprioritisation: gopherd keeps no peer after a body mismatch. |
| f5dedae63 | Deferred tx adverts: gopherd stays far below hived's per-peer limit as a sender. |
| 5e5a1631d, 63143202a | peers.json is not persisted by `gopherd node` (no `PeerDBPath`); make `peerdb.loadFromDisk` record-by-record before wiring it. |
