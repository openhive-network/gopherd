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

**gopherd is state-derived.** `broadcastInv` reconstructs the advert set by
scanning the recent-block ring. Nothing about that scan is inherently fresh, so
every freshness property hived gets for free must be re-established explicitly.

That difference *is* the incident: the original code advertised the last 200
ring entries (~10 minutes of history) to any peer whose per-peer `known` set was
empty — i.e. every newly connected peer.

> **Recommended follow-up:** port the event-driven shape. Capture newly-applied
> blocks into an explicit pending-advert set in the apply path and drain it per
> round, instead of scanning the ring. The current guards emulate the outcome
> but re-derive it defensively every time.

---

## Divergence table

| # | Area | hived | gopherd | Status |
|---|---|---|---|---|
| 1 | Advert set | `_new_block_inventory`, drained per round | ring scan + watermark + age gate | **Fixed** (`0f486ae`, `6452bbd`, `2effa3c`) |
| 2 | Advert freshness | implicit (only just-accepted blocks) | explicit `advertiseMaxAgeSec = 3` | **Fixed**, ours stricter by construction |
| 3 | Block id vs legacy hash | per-peer, by `core_protocol_version` | always block id; peer version **not stored** | ⚠️ **OPEN — risk** |
| 4 | "May I advertise at all" | per-peer `peer_needs_sync_items_from_us` | global `isCatchingUp()` (120s) | ⚠️ **Divergent** |
| 5 | Advert dedup | two sets: advertised-to-peer + peer-advertised-to-us | one `st.known` set written by both paths | Equivalent |
| 6 | Dedup expiry | time-based, 1 minute (`GRAPHENE_NET_MAX_INVENTORY_SIZE_IN_MINUTES`) | prune by `num <= head` | Divergent, benign |
| 7 | Transaction inventory | advertises txs | never advertises txs (no mempool relay) | Divergent, safe |
| 8 | Serving blocks | from block database | from 2048-block in-memory ring | Divergent, bounded |
| 9 | Unknown item requested | `item_not_available` | `item_not_available`, **no** num-based fallback | Matches (ours explicitly safer) |

---

## 3. Block id vs legacy hash — OPEN, and a plausible incident contributor

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
(`internal/p2p/proto/types.go:40`) and **always** advertise block ids
(`loops.go`, `proto.ItemHash(id)`). We never store the *peer's*
`core_protocol_version` — `node.Peer` has no such field, and the value parsed in
`handshake.go` is discarded.

**Risk:** a peer below protocol 107 interprets our inventory entries as legacy
`block_message` hashes. It then either cannot match them, or requests an item we
cannot serve under that identity. hived's response to a peer that offered
something it then fails to provide is a disconnect — and if that peer is
considered in-sync, the disconnect reason is literally *"You are missing a sync
item you claim to have, your database is probably corrupted"*
(`node.cpp:~3158`). That is the same "old/bad peer" class of eviction implicated
in the incident, and it selectively affects **older** nodes, matching the
reported symptom that pre-1.28.7 nodes forked away.

**Not yet fixed.** The minimal safe fix is to record the peer's
`core_protocol_version` at handshake and simply **not advertise** to peers below
107 (rather than implementing legacy-hash advertisement we would then have to
serve consistently).

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
   not.
4. **Never serve a different block than the one requested**, by any fallback.

Regression tests: `internal/p2p/sync/advertise_age_test.go`. Three of them were
verified to fail against the pre-incident code.
