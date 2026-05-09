// Package sync implements catch-up block + transaction synchronisation
// for ghive's p2p layer. Mirrors the hived flow:
//
//  1. Periodically every node broadcasts an item_ids_inventory_message
//     listing the IDs of recently produced blocks (and recently received
//     txs). Peers dedup against their own known set; missing items go
//     onto a per-peer fetch queue.
//
//  2. When a peer joins (or reconnects after being away) and its
//     advertised head_block_id is at-or-ahead of ours, we send
//     fetch_blockchain_item_ids_message containing a "synopsis" of our
//     chain head. The peer replies with blockchain_item_ids_inventory_message
//     listing the block IDs forward from there.
//
//  3. We then issue fetch_items_message in batches for missing IDs.
//     The peer responds with one block_message per requested ID
//     (or item_not_available_message for IDs it can't serve).
//
//  4. Received blocks are queued in arrival order and applied via
//     chain.ApplyBlock on a single goroutine — same single-writer
//     constraint as the witness production path.
//
// The sync delegate plugs into p2p/node via the Delegate interface and
// composes with the gossip and discovery delegates through
// node.MultiDelegate.
package sync
