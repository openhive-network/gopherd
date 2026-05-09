package sync

import (
	"hived-go/internal/p2p/node"
	"hived-go/internal/p2p/proto"
	"hived-go/internal/protocol/block"
)

// OnPeerConnected is invoked once per peer when it reaches StateActive.
// Allocates per-peer dedup state and kicks off the catch-up handshake.
func (d *Delegate) OnPeerConnected(p *node.Peer) {
	st := newPeerState()
	d.mu.Lock()
	d.peers[p] = st
	d.mu.Unlock()

	// Catch-up handshake: send fetch_blockchain_item_ids_message with
	// our current head as the synopsis. The peer replies with the
	// forward block-id list. Even if the peer is BEHIND us this is
	// a no-op for the peer (it returns an empty list).
	headID, _ := d.chain.HeadBlockID()
	d.sendFetchBlockchainItemIDs(p, headID)
}

// OnPeerDisconnected is invoked once per peer when it transitions to
// StateClosed. Drops the per-peer state and refunds the dropped peer's
// outstanding budget so the global cap doesn't leak across reconnects.
func (d *Delegate) OnPeerDisconnected(p *node.Peer, _ error) {
	d.mu.Lock()
	st := d.peers[p]
	delete(d.peers, p)
	d.mu.Unlock()
	if st != nil {
		st.mu.Lock()
		n := len(st.outstanding)
		st.outstanding = make(map[block.ID]struct{})
		st.mu.Unlock()
		if n > 0 {
			d.outstandingTotal.Add(int32(-n))
		}
	}
}

// OnMessage routes inbound business messages to per-type handlers. We
// claim the sync-related types and silently ignore the rest (gossip and
// discovery delegates handle theirs).
func (d *Delegate) OnMessage(p *node.Peer, t proto.MessageType, body []byte) {
	switch t {
	case proto.ItemIDsInventoryMessageType:
		d.handleInv(p, body)
	case proto.FetchBlockchainItemIDsMessageType:
		d.handleFetchBlockchainItemIDs(p, body)
	case proto.BlockchainItemIDsInventoryMessageType:
		d.handleBlockchainItemIDsInventory(p, body)
	case proto.FetchItemsMessageType:
		d.handleFetchItems(p, body)
	case proto.ItemNotAvailableMessageType:
		d.handleItemNotAvailable(p, body)
	case proto.BlockMessageType:
		// Note: in the merged Phase 2.2 wiring, BlockMessage is also
		// dispatched via OnBlockReceived (gossip's typed hook). Both
		// paths feed the same applyLoop / dedup, so duplicate dispatch
		// here is a no-op after the first.
		d.handleBlock(p, body)
	case proto.CompressedBlockMessageType:
		// Compressed-block ingestion would need the zstd dictionary
		// set; deferred to a follow-up. Decompression on the live
		// path requires sharing the same dictionary catalogue
		// blocklog uses (internal/blocklog/zstd).
	}
}

// OnBlockReceived satisfies the node.Delegate typed hook for blocks.
// We forward to the same handler as OnMessage's BlockMessage case, so
// blocks landing via gossip's typed dispatch reach the sync layer's
// applyLoop + dedup the same way.
func (d *Delegate) OnBlockReceived(p *node.Peer, body []byte) {
	d.handleBlock(p, body)
}

// OnTransactionReceived satisfies the node.Delegate typed hook for txs.
// Sync currently only consumes blocks (the witness mempool handles txs
// via gossip's path); this is a no-op here.
func (d *Delegate) OnTransactionReceived(p *node.Peer, body []byte) {}

// OnAddressesReceived satisfies the node.Delegate typed hook for the
// discovery layer's address_message; the peerdb owns this path.
func (d *Delegate) OnAddressesReceived(p *node.Peer, addrs []proto.AddressInfo) {}

// BuildAddressReply satisfies the node.Delegate typed hook used to
// answer a peer's address_request; let the Node assemble its own reply.
func (d *Delegate) BuildAddressReply(p *node.Peer) []proto.AddressInfo { return nil }

// handleInv is the inbound side of item_ids_inventory_message: list of
// item hashes the peer has. We dedup against our local known set and
// queue any unknowns onto a fetch_items request.
func (d *Delegate) handleInv(p *node.Peer, body []byte) {
	var msg proto.ItemIDsInventoryMessage
	if err := msg.Unmarshal(body); err != nil {
		d.logf("inv decode: %v", err)
		return
	}
	if len(msg.ItemHashesAvailable) == 0 {
		return
	}
	st := d.peerStateFor(p)
	if st == nil {
		return
	}

	// Bin into "blocks I don't have" vs "txs I don't have".
	missing := make([]proto.ItemHash, 0, len(msg.ItemHashesAvailable))
	throttled := false
	st.mu.Lock()
	for _, h := range msg.ItemHashesAvailable {
		bid := block.ID(h)
		switch msg.ItemType {
		case proto.ItemTypeBlock, proto.ItemTypeBlockCompressed:
			// Skip if we already have this block locally.
			if d.chain.RecentBlockBytesByID(bid) != nil {
				st.known[bid] = struct{}{}
				continue
			}
			// Skip if its embedded block_num is at or below our head.
			if bid.Num() != 0 {
				head, _ := d.chain.HeadBlock()
				if bid.Num() <= head {
					st.known[bid] = struct{}{}
					continue
				}
			}
			// Already requested? skip (and remember as known).
			if _, ok := st.outstanding[bid]; ok {
				st.known[bid] = struct{}{}
				continue
			}
			// Budget gates — DON'T mark known when throttling so the
			// next INV from this peer (post-drain) re-triggers.
			if len(st.outstanding) >= d.cfg.MaxOutstandingPerPeer {
				throttled = true
				continue
			}
			if d.outstandingTotal.Load() >= int32(d.cfg.MaxOutstandingTotal) {
				throttled = true
				continue
			}
			st.outstanding[bid] = struct{}{}
			st.known[bid] = struct{}{}
			d.outstandingTotal.Add(1)
			missing = append(missing, h)
		case proto.ItemTypeTrx:
			// We don't run a tx mempool yet; ignore tx INV until that
			// lands. Recording it in known prevents repeated re-asks.
			st.known[bid] = struct{}{}
		}
	}
	st.mu.Unlock()
	if throttled {
		d.outstandingThrottled.Add(1)
	}

	if len(missing) == 0 {
		return
	}
	d.sendFetchItems(p, msg.ItemType, missing)
	d.poke()
}

// handleFetchBlockchainItemIDs is the catch-up RECEIVER side: peer is
// telling us "here's my chain head; tell me what comes next." We
// reply with blockchain_item_ids_inventory_message listing the block
// IDs immediately after the synopsis-implied head, up to MaxBlocksPerInv.
func (d *Delegate) handleFetchBlockchainItemIDs(p *node.Peer, body []byte) {
	var msg proto.FetchBlockchainItemIDsMessage
	if err := msg.Unmarshal(body); err != nil {
		d.logf("fetch_blockchain_item_ids decode: %v", err)
		return
	}
	// Synopsis: the LAST hash in the list is the peer's head_block_id.
	// Older entries are exponentially-older history (hived's chain
	// synopsis structure) — for now we only need the head, since both
	// nodes are on the same fork.
	var peerHeadID block.ID
	if n := len(msg.BlockchainSynopsis); n > 0 {
		peerHeadID = block.ID(msg.BlockchainSynopsis[n-1])
	}

	// Map peer's head_id → block_num (peer head_id has block_num in its
	// first 4 BE bytes).
	peerHeadNum := uint32(0)
	if peerHeadID != (block.ID{}) {
		peerHeadNum = peerHeadID.Num()
	}

	// Update peer state with their head.
	if st := d.peerStateFor(p); st != nil {
		st.mu.Lock()
		if peerHeadNum > st.peerHeadNum {
			st.peerHeadNum = peerHeadNum
		}
		st.mu.Unlock()
	}

	// Fetch our recent blocks AFTER peer's head.
	hashes := make([]proto.ItemHash, 0, d.cfg.MaxBlocksPerInv)
	myHead, _ := d.chain.HeadBlock()
	if myHead > peerHeadNum {
		// Walk forward from peerHeadNum+1 until we run out of cached
		// blocks or hit MaxBlocksPerInv.
		for n := peerHeadNum + 1; n <= myHead && len(hashes) < d.cfg.MaxBlocksPerInv; n++ {
			id, ok := d.chain.RecentBlockIDForNum(n)
			if !ok {
				break
			}
			hashes = append(hashes, proto.ItemHash(id))
		}
	}

	remaining := uint32(0)
	if myHead > peerHeadNum+uint32(len(hashes)) {
		remaining = myHead - peerHeadNum - uint32(len(hashes))
	}
	reply := proto.BlockchainItemIDsInventoryMessage{
		TotalRemainingItemCount: remaining,
		ItemType:                proto.ItemTypeBlock,
		ItemHashesAvailable:     hashes,
	}
	encoded, err := reply.Marshal()
	if err != nil {
		d.logf("blockchain_item_ids_inventory encode: %v", err)
		return
	}
	if err := p.Send(proto.BlockchainItemIDsInventoryMessageType, encoded); err != nil {
		d.logf("blockchain_item_ids_inventory send: %v", err)
	}
}

// handleBlockchainItemIDsInventory is the catch-up REQUESTER side: peer
// has answered our fetch_blockchain_item_ids with the forward block
// list. Queue fetch_items batches for those we don't already have,
// honoring both per-peer and total in-flight caps.
func (d *Delegate) handleBlockchainItemIDsInventory(p *node.Peer, body []byte) {
	var msg proto.BlockchainItemIDsInventoryMessage
	if err := msg.Unmarshal(body); err != nil {
		d.logf("blockchain_item_ids_inventory decode: %v", err)
		return
	}
	if len(msg.ItemHashesAvailable) == 0 {
		return
	}
	st := d.peerStateFor(p)
	if st == nil {
		return
	}
	myHead, _ := d.chain.HeadBlock()
	missing := make([]proto.ItemHash, 0, len(msg.ItemHashesAvailable))
	throttled := false
	st.mu.Lock()
	for _, h := range msg.ItemHashesAvailable {
		bid := block.ID(h)
		st.known[bid] = struct{}{}
		// Track peer's head-num from the highest ID we see.
		if n := bid.Num(); n > st.peerHeadNum {
			st.peerHeadNum = n
		}
		// Skip already-applied + already-outstanding.
		if bid.Num() != 0 && bid.Num() <= myHead {
			continue
		}
		if d.chain.RecentBlockBytesByID(bid) != nil {
			continue
		}
		if _, ok := st.outstanding[bid]; ok {
			continue
		}
		// Per-peer + total budget enforcement. If we hit either cap we
		// stop adding to outstanding for this round; the next INV after
		// drainage will re-trigger the rest.
		if len(st.outstanding) >= d.cfg.MaxOutstandingPerPeer {
			throttled = true
			break
		}
		if d.outstandingTotal.Load() >= int32(d.cfg.MaxOutstandingTotal) {
			throttled = true
			break
		}
		st.outstanding[bid] = struct{}{}
		d.outstandingTotal.Add(1)
		missing = append(missing, h)
	}
	st.mu.Unlock()
	if throttled {
		d.outstandingThrottled.Add(1)
	}

	// Issue fetch_items in adaptively-sized chunks. Fast peers
	// (high recent blocks/sec) get bigger batches so we keep their
	// pipeline full; slow peers stay small so a single 5000-block
	// round to a slow peer doesn't stall the catch-up. The size is
	// recomputed once per round based on a 30s sliding rate window.
	batch := st.adaptiveBatchSize(d.cfg.FetchBatch)
	if d.cfg.AdaptiveFetchBatch && d.cfg.LogAdaptiveBatch {
		// Cheap trace for offline tuning (off by default).
		d.logf("peer %s batch=%d (rate=%d bps, missing=%d)",
			p.Conn().RemoteAddr(), batch, st.LastRate(), len(missing))
	}
	if !d.cfg.AdaptiveFetchBatch {
		batch = d.cfg.FetchBatch
	}
	if batch <= 0 {
		batch = d.cfg.FetchBatch
	}
	for i := 0; i < len(missing); i += batch {
		end := i + batch
		if end > len(missing) {
			end = len(missing)
		}
		d.sendFetchItems(p, msg.ItemType, missing[i:end])
	}

	// If the peer signalled there's more remaining AND we didn't throttle
	// THIS round, walk forward. We DO NOT gate on len(missing)>0:
	// when N peers all hand back the same first chunk in a sync, the
	// later peers see all-deduped (we already have / already requested)
	// and len(missing)==0 — but if we don't walk them forward they go
	// silent until the 5s INV tick, killing throughput. Walking them
	// forward immediately gets each peer onto fresh ground.
	//
	// Furthermore, FAN OUT the walk-forward to every other connected
	// peer too. When peer A is slow to respond on a 1000-block chunk,
	// peers B..H deliver the same 1000-block chunk in parallel — the
	// first to arrive wins (subsequent dups are filtered by
	// RecentBlockBytesByID and HeadBlock checks). We trade some
	// upstream-bandwidth at the seeds for stall-free catch-up; the
	// per-peer outstanding cap keeps any single peer bounded.
	if msg.TotalRemainingItemCount > 0 && !throttled && !d.inhibitFetching.Load() {
		// Use the highest-num hash we received as the new synopsis
		// anchor — it will become our head once those blocks apply.
		anchor := block.ID(msg.ItemHashesAvailable[len(msg.ItemHashesAvailable)-1])
		d.sendFetchBlockchainItemIDs(p, anchor)
	}
	if len(missing) > 0 {
		d.poke()
	}
}


// handleFetchItems is the SERVER side: peer is asking for raw items by
// hash. Reply with one block_message per known block, or
// item_not_available per unknown.
func (d *Delegate) handleFetchItems(p *node.Peer, body []byte) {
	var msg proto.FetchItemsMessage
	if err := msg.Unmarshal(body); err != nil {
		d.logf("fetch_items decode: %v", err)
		return
	}
	for _, h := range msg.ItemsToFetch {
		switch msg.ItemType {
		case proto.ItemTypeBlock, proto.ItemTypeBlockCompressed:
			bid := block.ID(h)
			raw := d.chain.RecentBlockBytesByID(bid)
			if raw == nil {
				// Fall back to block-num lookup (the embedded num in
				// id[:4] is canonical) so peers re-asking by num can
				// still get an answer if the cache evicted by id.
				if n := bid.Num(); n != 0 {
					raw = d.chain.RecentBlockBytes(n)
				}
			}
			if raw == nil {
				notAvail := proto.ItemNotAvailableMessage{
					RequestedItem: proto.ItemID{
						ItemType: msg.ItemType,
						ItemHash: h,
					},
				}
				enc, _ := notAvail.Marshal()
				_ = p.Send(proto.ItemNotAvailableMessageType, enc)
				continue
			}
			out := proto.BlockMessage{
				SignedBlockBytes: raw,
				BlockID:          proto.ItemHash(bid),
			}
			enc, err := out.Marshal()
			if err != nil {
				d.logf("block_message encode: %v", err)
				continue
			}
			if err := p.Send(proto.BlockMessageType, enc); err != nil {
				d.logf("block_message send: %v", err)
				return
			}
		case proto.ItemTypeTrx:
			notAvail := proto.ItemNotAvailableMessage{
				RequestedItem: proto.ItemID{
					ItemType: msg.ItemType,
					ItemHash: h,
				},
			}
			enc, _ := notAvail.Marshal()
			_ = p.Send(proto.ItemNotAvailableMessageType, enc)
		}
	}
}

// handleItemNotAvailable lets us drop the corresponding outstanding
// fetch so a future INV from another peer can re-trigger the request.
func (d *Delegate) handleItemNotAvailable(p *node.Peer, body []byte) {
	var msg proto.ItemNotAvailableMessage
	if err := msg.Unmarshal(body); err != nil {
		return
	}
	if st := d.peerStateFor(p); st != nil {
		bid := block.ID(msg.RequestedItem.ItemHash)
		st.mu.Lock()
		_, was := st.outstanding[bid]
		delete(st.outstanding, bid)
		st.mu.Unlock()
		if was {
			d.outstandingTotal.Add(-1)
		}
	}
}

// handleBlock queues an inbound block onto the apply pipeline.
func (d *Delegate) handleBlock(p *node.Peer, body []byte) {
	var msg proto.BlockMessage
	if err := msg.Unmarshal(body); err != nil {
		d.logf("block_message decode: %v", err)
		return
	}
	bid := block.ID(msg.BlockID)
	// If the wire-trailing block_id was zero, derive it from the bytes
	// — agent-gossip's broadcast path may leave it zero when re-using
	// the block they just produced.
	if bid == (block.ID{}) {
		// Find the header end by parsing it. The cheaper alternative
		// is to derive id from bytes; we leave it to the applier so
		// the queue stays cheap.
	}

	// Mark known on this peer so we don't ask back for it.
	if st := d.peerStateFor(p); st != nil {
		st.mu.Lock()
		st.known[bid] = struct{}{}
		_, wasOut := st.outstanding[bid]
		delete(st.outstanding, bid)
		st.mu.Unlock()
		if wasOut {
			d.outstandingTotal.Add(-1)
		}
		// Atomic — outside st.mu — so per-peer rate accounting doesn't
		// contend with broadcastInv's known-map iteration.
		st.recordBlock()
	}

	// Skip duplicate-arrival recovery + apply work entirely. With the
	// catch-up kicker firing fetch_blockchain_item_ids to all peers
	// every second, the same block frequently arrives from 2-4 peers.
	// A block we've already applied (head >= bid.num) or already cached
	// (RecentBlockBytesByID hit) will get skipped by applyLoop anyway,
	// but only AFTER we've spent ~2 ms on sig recovery on this
	// goroutine. Drop early to claw that back.
	//
	// We use the lock-free atomic mirror d.headBlockHint instead of
	// d.chain.HeadBlock() — the latter takes the chain applyMu.RLock
	// and materializes a state.Session, which costs ~2ms per call and
	// becomes a measurable serialization point on the hot per-peer
	// receive path. The mirror is updated by applyLoop after each
	// successful apply; it can lag by one block but that's harmless
	// here (worst case we recover sig for a block applyLoop will
	// discard one step later — same outcome as before this skip).
	headHint := d.headBlockHint.Load()
	if bn := bid.Num(); bn != 0 && uint32(bn) <= headHint {
		return
	}
	if d.chain.RecentBlockBytesByID(bid) != nil {
		return
	}

	// Pre-recover the witness signing key on THIS goroutine. handleBlock
	// runs on the per-peer receive goroutine — with N peers we get N-way
	// parallel sig recovery for free, and applyLoop never blocks on the
	// expensive secp256k1 RecoverPublicKey. RecoverBlockSigner returns
	// the zero PublicKey on any parse / recovery error; in that case
	// chain.ApplyBlock falls back to its own (failing) recovery path
	// which generates the canonical error message.
	//
	// We pass the block ID so duplicate deliveries (the same block from
	// multiple peers, common during catch-up where peers redundantly
	// answer the same fetch_items) hit the cache and skip the recovery.
	signer := recoverBlockSignerCached(msg.SignedBlockBytes, d.signerCache, bid)

	job := applyJob{
		blockNum:          bid.Num(),
		rawBlock:          msg.SignedBlockBytes,
		id:                bid,
		srcPeer:           p,
		preVerifiedSigner: signer,
	}
	select {
	case d.applyQ <- job:
		d.onEnqueue()
	default:
		// Queue full — drop. Catch-up will re-trigger via the next
		// INV / blockchain_item_ids cycle. Also re-arm the per-peer
		// outstanding map so we'll re-ask once we drain.
		d.droppedQueueFull.Add(1)
		// (We already cleared outstanding above on the receipt path;
		// the bid was probably never in there for gossip-pushed blocks
		// either, so the Add(-1) is gated on "was actually present".)
		// Un-mark known so the next INV from the same peer re-triggers
		// a fetch once back-pressure releases.
		if st := d.peerStateFor(p); st != nil {
			st.mu.Lock()
			delete(st.known, bid)
			st.mu.Unlock()
		}
		d.logf("apply queue full (%d/%d), dropping block %d",
			d.QueueDepth(), d.QueueCapacity(), job.blockNum)
	}
}

// peerStateFor returns the per-peer state, or nil if the peer was
// dropped between the message arriving and us being scheduled.
func (d *Delegate) peerStateFor(p *node.Peer) *peerState {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.peers[p]
}

// sendFetchBlockchainItemIDs encodes and sends the catch-up handshake.
func (d *Delegate) sendFetchBlockchainItemIDs(p *node.Peer, headID block.ID) {
	var synopsis []proto.ItemHash
	if headID != (block.ID{}) {
		synopsis = []proto.ItemHash{proto.ItemHash(headID)}
	}
	msg := proto.FetchBlockchainItemIDsMessage{
		ItemType:           proto.ItemTypeBlock,
		BlockchainSynopsis: synopsis,
	}
	enc, err := msg.Marshal()
	if err != nil {
		d.logf("fetch_blockchain_item_ids encode: %v", err)
		return
	}
	if err := p.Send(proto.FetchBlockchainItemIDsMessageType, enc); err != nil {
		d.logf("fetch_blockchain_item_ids send: %v", err)
	}
}

// sendFetchItems encodes and sends one fetch_items batch.
//
// Honors apply-queue back-pressure: while we're inhibited we DROP the
// fetch and clear the matching per-peer outstanding entries so they get
// re-tried after the queue drains. This mirrors hived's
// _inhibit_fetching_sync_blocks_until per-peer pause: we keep the TCP
// session open + answering pings, we just stop pulling new work.
func (d *Delegate) sendFetchItems(p *node.Peer, itemType uint32, hashes []proto.ItemHash) {
	if len(hashes) == 0 {
		return
	}
	if d.inhibitFetching.Load() {
		d.inhibitedFetches.Add(uint64(len(hashes)))
		// Don't keep the IDs marked as outstanding — peer would never
		// hear our request and we'd never re-issue. Caller already
		// inserted them in st.outstanding; pull them back out so a
		// subsequent INV (post-resume) will re-queue them.
		if st := d.peerStateFor(p); st != nil {
			st.mu.Lock()
			released := 0
			for _, h := range hashes {
				if _, ok := st.outstanding[block.ID(h)]; ok {
					delete(st.outstanding, block.ID(h))
					released++
				}
				// Also un-mark known so the next INV re-triggers a fetch.
				delete(st.known, block.ID(h))
			}
			st.mu.Unlock()
			if released > 0 {
				d.outstandingTotal.Add(int32(-released))
			}
		}
		return
	}
	msg := proto.FetchItemsMessage{
		ItemType:     itemType,
		ItemsToFetch: hashes,
	}
	enc, err := msg.Marshal()
	if err != nil {
		d.logf("fetch_items encode: %v", err)
		return
	}
	if err := p.Send(proto.FetchItemsMessageType, enc); err != nil {
		d.logf("fetch_items send: %v", err)
	}
}
