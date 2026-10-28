# gopherd P2P vs hived: gossip/advert path comparison

Written after the 2026-08 forking incident, which the hived team traced to this
node. The advert path is the highest-blast-radius code in the project: a single
stale advert is relayed onward by peers *before* they work out it is bad, so the
resulting kick cascades outward. Observed consequences included widespread
forking, witness participation drops, and a latent HAF forking bug that poisoned
HAF databases.

This document compares what we do against what hived actually does, so
divergences are deliberate and visible rather than discovered by incident.

Sources compared:

| | |
|---|---|
| hived | `libraries/net/node.cpp`, `libraries/net/peer_connection.cpp`, `libraries/net/include/graphene/net/config.hpp` |
| gopherd | `internal/p2p/sync/loops.go`, `internal/p2p/sync/handlers.go`, `internal/p2p/sync/delegate.go` |

---

## The root architectural difference

**hived is event-driven.** `_new_block_inventory` is populated *only* in
`broadcast()` when a block is accepted (`node.cpp:5766`), and
`advertise_inventory_loop` **swaps the whole set out** each iteration
(`node.cpp:1400`). The set therefore contains exactly "blocks accepted since the
last round" — normally one. **Staleness is structurally impossible**, so hived
needs no age check anywhere in the advert path.

**gopherd WAS state-derived.** `broadcastInv` reconstructed the advert set by
scanning the recent-block ring. Nothing about that scan is inherently fresh, so
every freshness property hived gets for free had to be re-established explicitly.
It now uses hived's shape (see below).

That difference *is* the incident: the original code advertised the last 200
ring entries (~10 minutes of history) to any peer whose per-peer `known` set was
empty — i.e. every newly connected peer.

> **DONE (`8ee1394`).** The event-driven shape is now ported: `queueAdvert` is
> called from the apply path (hived's `broadcast()`) and `broadcastInv` drains
> the set per round (hived's `swap`). The ring is no longer consulted by the
> advert path, so freshness is structural. The watermark and age gate remain as
> backstops for a starved advert loop rather than as the primary mechanism.

---

## Divergence table

| # | Area | hived | gopherd | Status |
|---|---|---|---|---|
| 1 | Advert set | `_new_block_inventory`, drained per round | **same shape** — `queueAdvert` from apply path, drained per round | **Closed** (`8ee1394`) |
| 2 | Advert freshness | implicit (only just-accepted blocks) | explicit `advertiseMaxAgeSec = 3` | **Fixed**, ours stricter by construction |
| 3 | Block id vs legacy hash | per-peer, by `core_protocol_version` | version captured; **withhold** from peers < 107 | **Closed** (`8ee1394`) |
| 4 | "May I advertise at all" | per-peer `peer_needs_sync_items_from_us` | global `isCatchingUp()` (120s) | ⚠️ **Divergent** |
| 5 | Advert dedup | two sets: advertised-to-peer + peer-advertised-to-us | one `st.known` set written by both paths | Equivalent |
| 6 | Dedup expiry | time-based, 1 minute (`GRAPHENE_NET_MAX_INVENTORY_SIZE_IN_MINUTES`) | prune by `num <= head` | Divergent, benign |
| 7 | Transaction inventory | advertises txs | never advertises txs (no mempool relay) | Divergent, safe |
| 8 | Serving blocks | from block database | from 2048-block in-memory ring | Divergent, bounded |
| 9 | Unknown item requested | `item_not_available` | `item_not_available`, **no** num-based fallback | Matches (ours explicitly safer) |
| 10 | `fetch_blockchain_item_ids` we cannot serve | always answered; nothing to give = **empty** inventory (`on_fetch_blockchain_item_ids_message`, unreachable-fork branch) | was **silence** when passive or behind; hived drops an unanswered request after 6 s (`active_ignored_request_timeout`), so every peer churned ~10 s after handshake (334 drops in 6 min, 2026-10-28). Now an empty inventory, as hived | **Closed** (`replyNoSyncItems`) |

---

## 3. Block id vs legacy hash — CLOSED; was a plausible incident contributor

hived picks the inventory hash **per peer**:

```cpp
// node.cpp:1421
fc::ripemd160 item_hash_for_block = peer->advertise_blocks_by_block_id()
    ? full_block_to_advertise->get_block_id()
    : full_block_to_advertise->get_legacy_block_message_hash();

// peer_connection.cpp:547
bool peer_connection::advertise_blocks_by_block_id() const {
  return core_protocol_version >= GRAPHENE_NET_PROTOCOL_ADVERTISE_BLOCKS_BY_BLOCK_ID_VERSION; // 107
}
```

We advertise `CoreProtocolVersion = 107` in our own hello
(`internal/p2p/proto/types.go`) and only ever speak block ids. Before `8ee1394`
we never stored the *peer's* `core_protocol_version` — `node.Peer` had no such
field and the parsed hello value was discarded, so we advertised block ids to
everyone regardless of whether they could interpret them.

**Risk:** a peer below protocol 107 interprets our inventory entries as legacy
`block_message` hashes. It then either cannot match them, or requests an item we
cannot serve under that identity. hived's response to a peer that offered
something it then fails to provide is a disconnect — and if that peer is
considered in-sync, the disconnect reason is literally *"You are missing a sync
item you claim to have, your database is probably corrupted"*
(`node.cpp:~3158`). That is the same "old/bad peer" class of eviction implicated
in the incident, and it selectively affects **older** nodes, matching the
reported symptom that pre-1.28.7 nodes forked away.

**Fixed (`8ee1394`)** with exactly that minimal approach: `Peer` captures
`core_protocol_version` from either hello form, and `broadcastInv` withholds
from peers below 107. We do not implement legacy-hash advertisement, since we
would then have to serve consistently under that identity. Version 0 (hello not
parsed) is withheld as well — unknown is not permission.

## 4. In-sync determination — divergent by design, worth knowing

hived decides per peer, from the sync negotiation, and only advertises to peers
that are in sync with it (`node.cpp:1412`, flag set at `node.cpp:~2566`).

We use a **global** `isCatchingUp()` — our head's timestamp within
`liveAdvertiseWindowSec` (120s). Two consequences:

- It is not per-peer: we advertise to peers we may be far out of step with.
- 120s is ~40 blocks. It permits advertising while well behind, which is why the
  per-block `advertiseMaxAgeSec` gate — not this one — is what actually keeps
  staleness off the wire.

The comment above that gate previously claimed it means "only advertise LIVE
blocks". It does not: it tests the **head only**, and its window is far wider
than the tip. That wording is why the defect survived two readings of
`broadcastInv` during the incident session.

## 6-8. Benign divergences

- **Dedup expiry:** hived expires inventory sets after 1 minute; we prune
  entries at or below head. Both bound memory; neither affects what is offered.
- **Transactions:** hived gossips tx inventory. We have no mempool relay and
  never advertise txs — strictly less traffic, no correctness impact.
- **Serving:** hived serves from its block database; we serve only from the
  2048-block recent ring (`RecentBlockBytesByID`) and answer
  `item_not_available` otherwise. Since the advert watermark + 3s age gate mean
  we only ever offer blocks we just applied, everything we advertise is in the
  ring. We deliberately do **not** fall back to serving by block number, because
  returning a different block at the requested height is exactly what earns
  "you offered me a block deemed invalid".

---

## Invariants to preserve

1. **Never offer a block older than one block interval.** Under one-block
   irreversibility a peer's LIB is `head-1`, so a node two blocks behind is
   already offering below its peers' LIB — "too old", unacceptable, kickable.
2. **Never offer the same block twice.** Otherwise a late-joining peer receives
   a replayed backlog.
3. **Fail silent.** If we cannot keep up, nothing qualifies and we advertise
   nothing until we are back at the tip. Silence is always safe; staleness is
   not. The one exception is a direct `fetch_blockchain_item_ids`: that must
   be *answered*, with an empty inventory (row 10), because hived treats an
   unanswered request as a dead peer. An empty answer claims nothing, so it is
   as safe as silence and keeps the connection.
4. **Never serve a different block than the one requested**, by any fallback.

Regression tests: `internal/p2p/sync/advertise_age_test.go` (7 cases). Three were
verified to fail against the pre-incident code, with its own signature
(`advertised 200 blocks, want <= 11`). They drive the event path — `queueAdvert`
then drain — so they exercise how blocks actually enter the advert set.

---

## Publishing a block we produced (2026-08-10, block 108887429)

The advert work above covers blocks we *received*. Blocks we *produce* took a
completely different path, and it was wrong in three ways at once. howo's first
real block was built correctly — right slot, on time, signed with the key
registered on chain — and still lost.

### What hived does

`node_impl::broadcast(const std::shared_ptr<full_block_type>&)` — node.cpp:5758:

```cpp
_most_recent_blocks_accepted.push_back(block_id);   // don't re-request our own
_block_message_cache.cache_message(full_block, ..); // so we can SERVE it
_new_block_inventory.insert(full_block);            // advertise the ID
trigger_advertise_inventory_loop();
```

It never pushes a block body. Publication is: cache it, advertise the **ID**,
serve it when a peer asks. Which is the same advert path a received block takes
— hived has exactly one publication mechanism, not two.

### What we did

| # | Defect | Consequence |
|---|---|---|
| 1 | `gossip.BroadcastBlock` pushed an unsolicited `block_message` body | hived accepts a `block_message` **only** if it requested it; otherwise "received a block I didn't ask for from peer, disconnecting from peer" (node.cpp:4039). All 8 peers dropped us in the same instant. Block never propagated; slot missed. |
| 2 | Witness called `chain.ApplyBlock` directly, no `Reversible` | Our own block became the **committed** head immediately. `PushLiveBlock` then filed the network's real block for that height as `PushStale` (`num <= committedNum()`), and its child buffered forever. `--fork-switching` was ON and structurally unable to help. |
| 3 | Both inventory paths skipped offered blocks with `height <= head` | We never fetched the competing block, so fork choice could not see the rival branch even once defect 2 was fixed. |

### Rules this fixes

1. **The witness applies nothing and broadcasts nothing.** It hands the block to
   `sync.Delegate.PublishBlock` and the normal apply path does both — one
   publication mechanism, like hived.
2. **A block we produce must stay reversible.** Any produced block may lose its
   slot race; if it is committed on creation, losing it wedges the node
   permanently. `--produce` therefore requires `--fork-switching`.
3. **A rival at our own head height must stay fetchable** (`skipByHeight`).
   Fork choice cannot switch to a branch it was never allowed to download.
4. **Local apply is not network acceptance.** `witness: APPLIED block N` was
   printed while every peer was rejecting it. The on-chain verdict is
   `get_block(N).witness` and the witness's `last_confirmed_block_num`.

Regression tests: `TestProductionDoesNotApplyLocally`,
`TestProductionRefusesWithoutPublisher` (`internal/plugins/witness/`),
`TestSkipByHeightForkAware` (`internal/p2p/sync/`). Fix: `31cb0f7`.
