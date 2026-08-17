package sync

import (
	"sort"
	"time"

	"gopherd/internal/chain"
	"gopherd/internal/p2p/node"
	"gopherd/internal/p2p/proto"
	"gopherd/internal/protocol/block"
)

// percentileMs returns the p-th percentile (0-100) of the nanosecond samples,
// expressed in milliseconds. Sorts a copy so the caller's ring is untouched.
func percentileMs(samples []int64, p int) float64 {
	if len(samples) == 0 {
		return 0
	}
	cp := append([]int64(nil), samples...)
	sort.Slice(cp, func(i, j int) bool { return cp[i] < cp[j] })
	idx := (p * (len(cp) - 1)) / 100
	return float64(cp[idx]) / 1e6
}

// invLoop periodically broadcasts an item_ids_inventory_message to every
// connected peer listing the IDs of recently-applied blocks. Backs off
// when nothing has changed for IdleINVRounds consecutive ticks; wakes
// on activityCh when the head advances or new INV is received.
func (d *Delegate) invLoop() {
	defer d.wg.Done()

	t := time.NewTicker(d.cfg.INVInterval)
	defer t.Stop()

	for {
		select {
		case <-d.stopCh:
			return
		case <-t.C:
			d.broadcastInv()
		case <-d.activityCh:
			// Activity detected. Reset the ticker so we broadcast
			// promptly without waiting a full interval, then loop.
			t.Reset(d.cfg.INVInterval)
			d.broadcastInv()
		}

		// Idle backoff: if we've had no activity for IdleINVRounds in
		// a row, sleep on activityCh until something pokes us. We
		// check this AFTER the broadcast so the very first idle round
		// still sends out one final advert.
		d.idleMu.Lock()
		idle := d.idleRounds
		d.idleMu.Unlock()
		if d.cfg.IdleINVRounds > 0 && idle >= d.cfg.IdleINVRounds {
			t.Stop()
			select {
			case <-d.stopCh:
				return
			case <-d.activityCh:
				t = time.NewTicker(d.cfg.INVInterval)
				continue
			}
		}
	}
}

// broadcastInv sends one round of item_ids_inventory to every connected
// peer. Each peer's known-set acts as a per-peer dedup so we don't keep
// re-advertising blocks the peer already has.
func (d *Delegate) broadcastInv() {
	// Passive (fetch-only) node: never advertise our inventory. Advertising an
	// item we can't then serve promptly (GC-stalled / wedged) is what gets a
	// node dropped by peers and can churn the network — a passive node is
	// structurally incapable of that. See Config.Serve.
	if !d.cfg.Serve {
		return
	}
	// Only advertise while our HEAD is live. NOTE this is necessary but NOT
	// sufficient: it says nothing about the age of the other blocks in the
	// batch, and its 120s window tolerates being ~40 blocks behind. The
	// per-block advertiseMaxAgeSec gate in selectAdvertIDs is what actually
	// keeps stale blocks off the wire. During initial catch-up every block we
	// hold is historical (timestamps hours-to-years old); a peer at the
	// live head rejects a node offering such an ancient block ("you
	// offered me a block that I have deemed to be invalid") and drops the
	// connection — churning through the peer set and eventually stalling
	// the sync once the good peers are exhausted. hived advertises
	// inventory only when in sync. We gate on the head block's timestamp
	// recency — the same liveness signal the gossip relay uses
	// (gossip.liveBroadcastWindowSec) via isCatchingUp() — rather than
	// the old isFarBehind(): the timestamp test needs no per-peer head
	// tracking, so it can't be fooled by a freshly-connected peer whose
	// head we haven't learned yet, and it naturally resumes advertising
	// the instant we reach the live tip. Catch-up propagation is
	// unaffected: it runs over the fetch_blockchain_item_ids / fetch_items
	// path, not this advert path.
	if d.isCatchingUp() {
		d.idleMu.Lock()
		d.idleRounds++
		d.idleMu.Unlock()
		return
	}
	// Drain the event-driven advert set (queueAdvert, fed from the apply path)
	// — hived's _new_block_inventory swap. Blocks in it were applied by us since
	// the last round, so the set is fresh by construction rather than by
	// filtering; the age gate inside is a backstop for when a starved advert
	// loop lets an entry go stale before it ships.
	allIDs := d.selectAdvertIDs(time.Now().Unix())
	txIDs := d.drainTxAdvert()
	if len(allIDs) == 0 && len(txIDs) == 0 {
		d.idleMu.Lock()
		d.idleRounds++
		d.idleMu.Unlock()
		return
	}

	d.mu.Lock()
	peers := make([]*node.Peer, 0, len(d.peers))
	states := make([]*peerState, 0, len(d.peers))
	for p, st := range d.peers {
		peers = append(peers, p)
		states = append(states, st)
	}
	d.mu.Unlock()

	if len(peers) == 0 {
		d.idleMu.Lock()
		d.idleRounds++
		d.idleMu.Unlock()
		return
	}

	advertisedAny := false
	for i, p := range peers {
		st := states[i]
		// Only advertise to peers that identify blocks the same way we do.
		// hived picks per peer: block ids at core_protocol_version >= 107,
		// legacy block_message hashes below that
		// (peer_connection.cpp::advertise_blocks_by_block_id). We only speak
		// block ids, so offering one to an older peer gives it an identity it
		// cannot match — and a fetch we cannot satisfy, which hived answers by
		// disconnecting. Older nodes are precisely the population that forked
		// away in the 2026-08 incident. Version 0 = hello not parsed yet, also
		// withheld: unknown is not permission.
		if p.CoreProtocolVersion() < proto.AdvertiseBlocksByBlockIDVersion {
			continue
		}
		// Filter to entries the peer hasn't been told about yet.
		hashes := make([]proto.ItemHash, 0, len(allIDs))
		st.mu.Lock()
		for _, id := range allIDs {
			if _, ok := st.known[id]; ok {
				continue
			}
			st.known[id] = struct{}{}
			hashes = append(hashes, proto.ItemHash(id))
		}
		st.mu.Unlock()
		// Transactions go in their own inventory message: one message carries a
		// single item type. Unlike blocks these are NOT withheld from older
		// peers — transaction ids are identified the same way at every protocol
		// version, so the >=107 gate that applies to block ids does not apply
		// here.
		if len(txIDs) > 0 {
			txHashes := make([]proto.ItemHash, 0, len(txIDs))
			st.mu.Lock()
			for _, id := range txIDs {
				if _, ok := st.known[id]; ok {
					continue // this peer already told us about it, or we it
				}
				st.known[id] = struct{}{}
				txHashes = append(txHashes, proto.ItemHash(id))
			}
			st.mu.Unlock()
			if len(txHashes) > 0 {
				tmsg := proto.ItemIDsInventoryMessage{
					ItemType:            proto.ItemTypeTrx,
					ItemHashesAvailable: txHashes,
				}
				if tenc, terr := tmsg.Marshal(); terr == nil {
					if serr := p.Send(proto.ItemIDsInventoryMessageType, tenc); serr == nil {
						d.txRelayed.Add(uint64(len(txHashes)))
						advertisedAny = true
					}
				}
			}
		}
		if len(hashes) == 0 {
			continue
		}
		msg := proto.ItemIDsInventoryMessage{
			ItemType:            proto.ItemTypeBlock,
			ItemHashesAvailable: hashes,
		}
		enc, err := msg.Marshal()
		if err != nil {
			d.logf("inv encode: %v", err)
			continue
		}
		if err := p.Send(proto.ItemIDsInventoryMessageType, enc); err != nil {
			d.logf("inv send to %s: %v", p.Conn().RemoteAddr(), err)
			continue
		}
		advertisedAny = true
	}

	d.idleMu.Lock()
	if advertisedAny {
		d.idleRounds = 0
	} else {
		d.idleRounds++
	}
	d.idleMu.Unlock()
}

// queueAdvert records a freshly-applied block for advertisement and wakes the
// advert loop. Mirrors hived's broadcast(): _new_block_inventory.insert(block)
// followed by trigger_advertise_inventory_loop() (node.cpp:5766).
//
// Called from the apply path, so the set only ever holds blocks we just
// accepted. Cheap and lock-brief — the apply loop is the single writer and must
// not be held up.
func (d *Delegate) queueAdvert(rawBlock []byte, blockNum uint32) {
	if !d.cfg.Serve {
		return // passive: never advertise, so never accumulate
	}
	id, ok := d.chain.RecentBlockIDForNum(blockNum)
	if !ok {
		return
	}
	e := advertEntry{id: id, num: blockNum, ts: blockTimestamp(rawBlock)}
	d.pendingAdvertMu.Lock()
	// Bound the queue. If the advert loop is starved we would rather drop the
	// oldest entries than grow without limit — and anything we drop here would
	// have failed the age gate on the way out regardless.
	if len(d.pendingAdvert) >= d.cfg.MaxBlocksPerInv {
		d.pendingAdvert = d.pendingAdvert[1:]
	}
	d.pendingAdvert = append(d.pendingAdvert, e)
	d.pendingAdvertMu.Unlock()
	d.poke()
}

// drainAdvertQueue takes the whole pending set, mirroring hived's
// `block_inventory_to_advertise.swap(_new_block_inventory)` (node.cpp:1400).
func (d *Delegate) drainAdvertQueue() []advertEntry {
	d.pendingAdvertMu.Lock()
	out := d.pendingAdvert
	d.pendingAdvert = nil
	d.pendingAdvertMu.Unlock()
	return out
}

// selectAdvertIDs picks the blocks broadcastInv may offer this round, and
// advances the advertise watermark. Split out from broadcastInv so the
// selection rules are directly testable — this is the logic whose earlier
// version caused a live mainnet incident (see broadcastInv).
//
// A block is offered only if it is BOTH above the watermark (never re-offered,
// so a new peer gets the new-block stream rather than a backlog) AND younger
// than advertiseMaxAgeSec (so history is never gossiped, including on the first
// round after a warm start when the watermark is still unset).
func (d *Delegate) selectAdvertIDs(nowUnix int64) []block.ID {
	entries := d.drainAdvertQueue()
	lastAdv := d.lastAdvertisedNum.Load()
	out := make([]block.ID, 0, len(entries))
	maxNum := lastAdv
	for _, e := range entries {
		if e.num <= lastAdv {
			continue // already offered
		}
		if e.ts == 0 || nowUnix-int64(e.ts) > advertiseMaxAgeSec {
			continue // aged out while we were starved — never gossip staleness
		}
		out = append(out, e.id)
		if e.num > maxNum {
			maxNum = e.num
		}
	}
	if maxNum > lastAdv {
		d.lastAdvertisedNum.Store(maxNum)
	}
	return out
}

// pruneLoop periodically drops per-peer dedup state (known / outstanding)
// and global in-flight reservations for blocks at or below our head. Those
// entries are dead weight: a block we've already applied is never
// re-requested (handleBlockchainItemIDsInventory skips num<=head) and never
// needs re-dedup. Without this the maps grow unbounded during a long
// catch-up — at ~9k bps each walking peer adds millions of IDs — which the
// old churn masked by resetting peer state on every disconnect. Measured
// holding 4.5 GB at head 6.9M before this loop existed.
func (d *Delegate) pruneLoop() {
	defer d.wg.Done()
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-d.stopCh:
			return
		case <-t.C:
			head := d.headBlockHint.Load()
			if head == 0 {
				h, _ := d.chain.HeadBlock()
				head = h
			}
			if head == 0 {
				continue
			}
			d.mu.Lock()
			sts := make([]*peerState, 0, len(d.peers))
			for _, st := range d.peers {
				sts = append(sts, st)
			}
			d.mu.Unlock()
			freed := 0
			for _, st := range sts {
				st.mu.Lock()
				for bid := range st.known {
					n := bid.Num()
					// Block ids embed their height, so anything at or below
					// head is already ours and can be forgotten.
					//
					// TRANSACTION advert ids also live in this map (since
					// --relay-transactions), and they are ripemd160 hashes:
					// Num() reads their first 4 bytes, which are effectively
					// random and therefore almost always FAR beyond head. The
					// old `n <= head` test never matched them, so every
					// transaction ever advertised to or by a peer stayed here
					// forever — measured at ~2.5 GiB/day of live-heap growth,
					// with handleInv/broadcastInv the top growers in a heap
					// diff. Drop anything too far ahead to be a real block;
					// keep a slack window so genuinely-next blocks survive.
					if n != 0 && (n <= head || n > head+knownFutureSlack) {
						delete(st.known, bid)
						continue
					}
					// A hash whose first bytes happen to look like a plausible
					// height (or zero) slips through the height test, so bound
					// the map by size as a backstop. Losing dedup only costs a
					// re-fetch; unbounded growth costs the process.
					if len(st.known) > maxKnownPerPeer {
						delete(st.known, bid)
					}
				}
				for bid := range st.outstanding {
					if n := bid.Num(); n != 0 && n <= head {
						delete(st.outstanding, bid)
						freed++
					}
				}
				st.mu.Unlock()
			}
			if freed > 0 {
				d.outstandingTotal.Add(int32(-freed))
			}
			d.inflightMu.Lock()
			for bid := range d.inflight {
				if n := bid.Num(); n != 0 && n <= head {
					delete(d.inflight, bid)
				}
			}
			d.inflightMu.Unlock()
		}
	}
}

// catchupKickerLoop fires a periodic fetch_blockchain_item_ids to every
// connected peer while we're catching up. Keeps deep pipelining even
// when individual peers are slow to walk-forward on their own — the
// passive walk-forward (in handleBlockchainItemIDsInventory) only fires
// AFTER the peer answers the prior request, so a slow peer (or a broken
// walk-forward chain) leaves a gap. This loop fills the gap by
// re-requesting every CatchupKickInterval while isCatchingUp() is true.
//
// Gating on isCatchingUp() (head-timestamp recency) rather than the old
// isFarBehind() is essential: isFarBehind() derived "behind" from the
// per-peer peerHeadNum, which only advances ~one inventory batch ahead of
// our head, so it flipped to false mid-catch-up and silenced the kicker —
// stalling the sync once the self-sustaining walk-forward broke. (Before
// the INV-advertise suppression landed, peer churn masked this: every
// "invalid block" disconnect forced a reconnect, and OnPeerConnected
// re-seeded the walk. With churn gone, the kicker is the only re-seeder.)
// tipStallKickTicks is how many consecutive idle kicker ticks (head not
// advancing, nothing in flight, nothing outstanding) mean we are STALLED at the
// tip rather than just waiting for the next block.
//
// It must clear the normal inter-block gap or the kicker would re-seed every
// peer every second while sitting healthily at the tip — which is the very
// fetch storm the INV suppression exists to prevent. Blocks are 3s apart and a
// missed witness slot doubles that, so 10 ticks (10s ≈ 3 missed slots) is
// unambiguously abnormal while still recovering ~12x faster than the 120s
// isCatchingUp() window did on its own.
// advertiseMaxAgeSec is the hard ceiling on how old a block may be for us to
// advertise it: ONE block interval. Anything we offer must effectively be the
// block we just applied.
//
// This bound is set by hived's definition of "too old", not by our own comfort.
// Under ONE-BLOCK IRREVERSIBILITY the last irreversible block is head-1, so a
// node just TWO blocks behind (6s) is already offering something below its
// peers' LIB — which hived treats as irreversibly-on-the-wrong-fork, i.e. a
// block it can never accept. The hived team confirmed this was the trigger for
// the 2026-08 forking incident caused by this node.
//
// Emitting even ONE stale advert is far more costly than "we get disconnected
// and reconnect": peers relay the offered block onward BEFORE they work out it
// is bad, so they get kicked too, and the kick cascades outward. Observed
// consequences included widespread forking, witness participation drops, and a
// latent HAF forking bug that poisoned HAF databases and needed recovery
// scripts. Treat the advert path as the highest-blast-radius code here.
//
// Earlier attempts, both too loose: 30s (~10 blocks behind) and 8s (~2-3
// blocks, still inside the sub-LIB window above).
//
// 3s costs nothing in normal operation: invLoop is poked on apply
// (activityCh), so a freshly applied block is advertised while ~1s old. When we
// cannot keep up the block ages out, nothing qualifies, and we go SILENT —
// which is the correct behaviour and the safe direction to fail.
//
// This bound is deliberately tight because it is also the ONLY thing that stops
// a CPU-starved node from gossiping staleness. isCatchingUp() — the gate that
// decides whether we advertise at all — allows a head up to
// liveAdvertiseWindowSec (120s) old, i.e. we can be ~40 BLOCKS BEHIND and still
// believe we are "live" enough to advertise. On a contended box (other
// workloads taking most of the CPU) that is exactly what happens: we fall
// behind, keep advertising a head that is now a minute old, and peers older
// than hived 1.28.7 act on those stale offers and fork away.
//
// With this bound the failure mode becomes self-correcting and safe: when we
// cannot keep up, our newest block ages past 8s, NOTHING passes the gate, and
// we simply go silent until we are back at the tip. That is precisely the
// "only advertise what you can serve promptly" contract — enforced by
// measurement rather than by assuming the node is keeping up.
//
// 30s was the first attempt and was too loose: it still permitted advertising
// while ~10 blocks behind.
const advertiseMaxAgeSec = 3

const tipStallKickTicks = 10

// Tip in-sync handshake. hived advertises inventory ONLY to peers it considers
// in sync: advertise_inventory_loop skips any peer with
// `peer_needs_sync_items_from_us` set (libraries/net/node.cpp:~1412). That flag
// is decided from OUR fetch_blockchain_item_ids (node.cpp:~2566): it clears to
// false only when the reply is empty, or when our synopsis is non-empty AND the
// reply is exactly ONE item contained in that synopsis — i.e. only when we are
// EXACTLY LEVEL with the peer.
//
// The kicker alone can never satisfy that: it fires when STALLED, which means
// behind, so the reply carries new items and the flag re-arms to true. We never
// asked while level, so peers never started advertising — observed live as
// invRx=0 with 8 peers while the head advanced solely by polling.
//
// So: while sitting level at the tip, periodically re-issue the request. The
// peer answers with the single matching item, clears the flag, and starts
// pushing inventory. Cheap (one small message per peer per interval) and it
// must only fire when genuinely level — issuing it while even one block behind
// yields a 2-item reply and re-arms the flag.
const (
	tipSyncHandshakeTicks = 30 // ≥30 kicker ticks (~30s) between handshakes
	// tipSyncHandshakeMaxAgeSec must be STRICTLY under the 3s block interval.
	// At 4s the window still admitted a head one block behind (a block 3-6s
	// old); a handshake sent while behind draws a multi-item reply, which
	// RE-ARMS peer_needs_sync_items_from_us and suppresses adverts until the
	// next handshake 30s later. Measured at 4s: tipStallKicks kept ticking
	// ~1-2/min even with push working. 2s can only be satisfied by the newest
	// block, so the reply is the single matching item and the flag clears.
	tipSyncHandshakeMaxAgeSec = 2
)

func (d *Delegate) catchupKickerLoop() {
	defer d.wg.Done()
	t := time.NewTicker(d.cfg.CatchupKickInterval)
	defer t.Stop()
	var lastHead uint32
	var idleTicks int
	var sinceHandshake int
	for {
		select {
		case <-d.stopCh:
			return
		case <-t.C:
			// Tip-stall escape hatch. At the tip isCatchingUp() is false, so the
			// gate below silences the kicker — and the kicker is the ONLY
			// re-seeder (see the comment above). A block dropped at head+1 then
			// has nothing to re-request it: future blocks pile into the reorder
			// buffer while head sits still, until head_age finally exceeds
			// liveAdvertiseWindowSec (120s) and isCatchingUp() flips back true.
			// Observed live 2026-08-09 at head 108,858,700 as a ~120s sawtooth,
			// 0..60 blocks behind, with 8 peers and ZERO apply errors
			// (qdepth=0 outstanding=0 inflight=0 — we simply stopped asking).
			// A serving node must not sit in that state, so treat a persistent
			// no-progress-nothing-in-flight window as reason to re-seed.
			head := d.headBlockHint.Load()
			if head == 0 {
				h, _ := d.chain.HeadBlock()
				head = h
			}
			d.inflightMu.Lock()
			nInflight := len(d.inflight)
			d.inflightMu.Unlock()
			if head != 0 && head == lastHead && nInflight == 0 && d.outstandingTotal.Load() == 0 {
				idleTicks++
			} else {
				idleTicks = 0
			}
			lastHead = head
			tipStalled := idleTicks >= tipStallKickTicks
			if tipStalled {
				// Reset so we re-seed at most once per tipStallKickTicks window
				// rather than every tick for as long as the stall lasts.
				idleTicks = 0
				d.tipStallKicks.Add(1)
				// NB: d.peers is guarded by d.mu and is read below under that
				// lock — don't touch it here.
				d.logf("tip stall: head=%d unchanged for %ds with nothing in flight — re-seeding peers",
					head, tipStallKickTicks)
			}
			// Tip in-sync handshake (see tipSyncHandshake* above): while level
			// at the tip, re-issue the request so peers clear
			// peer_needs_sync_items_from_us and start advertising to us.
			handshake := false
			if !d.isCatchingUp() && !tipStalled {
				sinceHandshake++
				if sinceHandshake >= tipSyncHandshakeTicks && d.headAgeSec() <= tipSyncHandshakeMaxAgeSec {
					sinceHandshake = 0
					handshake = true
					d.tipSyncHandshakes.Add(1)
				}
			}
			if !d.isCatchingUp() && !tipStalled && !handshake {
				continue
			}
			if d.inhibitFetching.Load() {
				continue
			}
			headID, _ := d.chain.HeadBlockID()
			d.mu.Lock()
			peers := make([]*node.Peer, 0, len(d.peers))
			for p := range d.peers {
				peers = append(peers, p)
			}
			d.mu.Unlock()
			for _, p := range peers {
				d.sendFetchBlockchainItemIDs(p, headID)
			}
		}
	}
}

// applyLoop is the SINGLE consumer of d.applyQ. Pulls inbound blocks
// off the queue and feeds them to chain.ApplyBlock in arrival order.
// Mirrors the witness production constraint: chain mutation must happen
// on a single goroutine.
//
// Blocks that arrive out of order (i.e. before the immediate next
// block) are stashed in pending and re-considered after each apply.
// The pending map is bounded by cfg.MaxPendingReorder — if a faulty
// peer streams far-future blocks faster than the gap closes we drop
// the highest-num entries and let the standard catch-up re-fetch them
// once the missing predecessor lands.
func (d *Delegate) applyLoop() {
	defer d.wg.Done()
	defer close(d.applyDone)

	pending := make(map[uint32]applyJob)

	// Per-block apply "work" timing accumulators (Config.LogWorkTiming). This
	// loop is the single ApplyBlock caller, so plain locals are race-free.
	// Summarized + reset on each diag tick.
	var workCount int
	var workSumNs, workMaxNs int64
	workSamples := make([]int64, 0, 8192)

	// Drop the highest-num pending entry to keep the reorder buffer
	// bounded. Returns the dropped block_num (0 if the map was empty).
	evictHighest := func() uint32 {
		var maxN uint32
		for n := range pending {
			if n > maxN {
				maxN = n
			}
		}
		if maxN > 0 {
			delete(pending, maxN)
			d.droppedReorder.Add(1)
		}
		return maxN
	}

	tryApply := func() {
		for {
			// Use the atomic hint when populated; the cold-start path
			// (head=0) reads the chain canonically.
			head := d.headBlockHint.Load()
			if head == 0 {
				h, _ := d.chain.HeadBlock()
				head = h
			}
			next := head + 1
			job, ok := pending[next]
			if !ok {
				return
			}
			delete(pending, next)
			// Historical catch-up matches hived's default replay behavior
			// (chain_plugin.cpp:1232-1239): skip transaction signatures
			// and authority checks for blocks below the head's checkpoint
			// horizon. Mainnet has txs (e.g., block 5,435,426 tx 2 on
			// `jesus2`) that historically failed sig verification — they
			// only entered the canonical block_log because witnesses
			// accepted them, and hived itself never re-validates them on
			// replay. We approximate the "old block" check by wall-clock
			// staleness: any block whose timestamp is >5 minutes behind
			// real time is treated as historical. Once we catch up to
			// the network tip, sigs are validated again.
			//
			// Historical catch-up: also skip block-signature verification.
			// In our locally-derived state a small number of witnesses
			// can end up flagged "disabled" (signing_key == zero) due to
			// state divergence from gaps we tolerated earlier in the
			// sync. When a real mainnet block at that height is produced
			// by such a witness, hived's replay path simply trusts the
			// header (chain_plugin.cpp:1232-1239) and applies the block,
			// but our strict verify_block_signature returns "witness X
			// has no signing_key (disabled)" and the block is dropped.
			// Concrete example: block 7,730,321 produced by witness
			// `snailmen09`. Tying block-sig validation to the same
			// 5-minute staleness gate matches hived's behavior; once at
			// the live tip the gate clears and apply is strict again.
			historical := false
			if ts := blockTimestamp(job.rawBlock); ts != 0 && uint32(time.Now().Unix())-ts > 5*60 {
				historical = true
			}
			// Historical catch-up also needs to tolerate per-op apply
			// failures: the canonical mainnet block_log carries txs
			// that hived's replay path skips because
			// chain_plugin.cpp:1232-1239 sets `skip_undo_history_check`
			// (and the entire apply runs inside a no-validate session).
			// Concrete example: block 5,147,607 tx 2 op 0 is a
			// transfer_from_savings on `abit` with request_id 101 that
			// duplicates a prior pending withdrawal; pure consensus
			// replay carries the block forward unchanged, so we have to
			// as well. TolerateFailingOps reproduces hived's behavior
			// at the per-op granularity.
			var applyStart time.Time
			if d.cfg.LogWorkTiming {
				applyStart = time.Now()
			}
			if _, err := d.chain.ApplyBlock(job.rawBlock, job.blockNum, chain.ApplyOptions{
				Sync:                      false,
				SkipTransactionSignatures: historical,
				SkipBlockSignature:        historical,
				TolerateFailingOps:        historical,
				PreVerifiedBlockSigner:    job.preVerifiedSigner,
				PreRecoveredTxKeys:        job.preRecoveredTxKeys,
			}, nil); err != nil {
				d.logf("apply_block %d failed: %v", job.blockNum, err)
				// Drop and continue — gossip / next INV will re-deliver
				// if the chain is on a recoverable fork.
				continue
			}
			if d.cfg.LogWorkTiming {
				// Per-block apply "work" time (comparable to hived's block-
				// processing timing). Accumulated here and summarized on the
				// diag tick; large p99/max vs a ~few-ms norm flags the node
				// falling behind (GC pressure, disk, etc.).
				ns := time.Since(applyStart).Nanoseconds()
				workCount++
				workSumNs += ns
				if ns > workMaxNs {
					workMaxNs = ns
				}
				if len(workSamples) < cap(workSamples) {
					workSamples = append(workSamples, ns)
				}
			}
			// Update the head hint that handleBlock reads to early-skip
			// duplicate deliveries. Slight lag (vs HeadBlock()) is OK —
			// the worst case is one extra sig-recovery on a stale block.
			d.headBlockHint.Store(job.blockNum)
			// Per-block "applied block N" line was the dominant
			// stderr-overhead source on a fast catch-up (15K writes/s on
			// a 12K bps stream). The chain-level per-N-block / per-second
			// progress line in the node's afterApply replaces it
			// for normal operation; debug ladders can re-enable via the
			// optional Config.LogEveryBlock flag.
			if d.cfg.LogEveryBlock {
				d.logf("applied block %d", job.blockNum)
			}
			// Fan the freshly-applied block out to peers (other than
			// the source we got it from). The hook is what replaced
			// gossip's now-deleted apply loop: gossip.Service wires its
			// BroadcastAppliedBlock here so re-broadcast stays on the
			// same goroutine that owns the chain mutation.
			if d.cfg.OnBlockApplied != nil {
				d.cfg.OnBlockApplied(job.rawBlock, job.blockNum, job.srcPeer)
			}
			// Event source for the advert set — hived calls broadcast() at
			// exactly this point (block accepted). No-op when passive.
			d.queueAdvert(job.rawBlock, job.blockNum)
			d.poke()
		}
	}

	// forkApply routes one inbound block through the fork-choice engine
	// (chain.PushLiveBlock): extend / buffer / depth-1 tip reorg. Used instead
	// of tryApply when Config.ForkAware. Out-of-order + competing blocks are
	// buffered by the chain's fork DB, so no reorder buffer is needed here. A
	// tip block stays reversible until its child confirms it (block_log-append
	// + eviction fire via SetOnBlockFinalized, not here), so nothing is
	// re-broadcast from this path.
	forkApply := func(job applyJob) {
		var applyStart time.Time
		if d.cfg.LogWorkTiming {
			applyStart = time.Now()
		}
		historical := false
		if ts := blockTimestamp(job.rawBlock); ts != 0 && uint32(time.Now().Unix())-ts > 5*60 {
			historical = true
		}
		outcome, err := d.chain.PushLiveBlock(job.rawBlock, job.blockNum, chain.ApplyOptions{
			Sync:                      false,
			SkipTransactionSignatures: historical,
			// A block we just produced carries our own signature, made
			// microseconds ago; recovering it again buys nothing and spends
			// slot budget we are racing. Tx signatures are still checked.
			SkipBlockSignature: historical || job.local,
			// Our own block skips only the signature recovery; every other
			// block-level check still runs, so an invalid block fails HERE with
			// a specific reason instead of being rejected by peers.
			ValidateOwnBlock:       job.local,
			TolerateFailingOps:     historical,
			PreVerifiedBlockSigner: job.preVerifiedSigner,
			PreRecoveredTxKeys:     job.preRecoveredTxKeys,
		})
		if err != nil {
			d.logf("push_live_block %d failed: %v", job.blockNum, err)
			return
		}
		if d.cfg.LogWorkTiming {
			ns := time.Since(applyStart).Nanoseconds()
			workCount++
			workSumNs += ns
			if ns > workMaxNs {
				workMaxNs = ns
			}
			if len(workSamples) < cap(workSamples) {
				workSamples = append(workSamples, ns)
			}
		}
		h, _ := d.chain.HeadBlock()
		d.headBlockHint.Store(h)
		if outcome == chain.PushReorged {
			d.logf("fork: reorged to head %d", h)
		}
		// Same event source as the linear path. Only advertise a block that
		// actually became our head: PushBuffered means it is parked awaiting a
		// parent and is not ours to offer, and after a reorg the head may be a
		// different block than the one we just pushed — queue by head number,
		// not by job.blockNum.
		if outcome != chain.PushBuffered && h == job.blockNum {
			d.queueAdvert(job.rawBlock, h)
		}
		// Same hook the linear path fires. It was wired ONLY there, so with
		// --fork-switching (the production configuration) it never ran at all —
		// silently disabling anything built on it.
		if d.cfg.OnBlockApplied != nil && outcome != chain.PushBuffered {
			d.cfg.OnBlockApplied(job.rawBlock, job.blockNum, job.srcPeer)
		}
		d.poke()
	}

	diag := time.NewTicker(10 * time.Second)
	defer diag.Stop()

	for {
		select {
		case <-d.stopCh:
			return
		case <-diag.C:
			// Per-block apply "work" timing summary (Config.LogWorkTiming).
			// avg/p50/p99/max over the window — comparable to hived's block-
			// processing time; a p99/max blowing past the ~3s block interval
			// means the node can't keep up (GC pressure, disk, cold-tier
			// thrash) and would advertise-but-not-serve if Serve were on.
			if d.cfg.LogWorkTiming && workCount > 0 {
				avgMs := float64(workSumNs) / float64(workCount) / 1e6
				p50, p99 := percentileMs(workSamples, 50), percentileMs(workSamples, 99)
				d.logf("WORK apply n=%d avg=%.2fms p50=%.2fms p99=%.2fms max=%.2fms",
					workCount, avgMs, p50, p99, float64(workMaxNs)/1e6)
				workCount, workSumNs, workMaxNs = 0, 0, 0
				workSamples = workSamples[:0]
			}
			// Stall diagnostics: if we're sitting on a non-empty reorder
			// buffer but the immediate next block isn't in it, log what
			// we're waiting for and the buffered range so a catch-up that
			// gets wedged on one missing block is visible.
			if len(pending) > 0 {
				head := d.headBlockHint.Load()
				if head == 0 {
					h, _ := d.chain.HeadBlock()
					head = h
				}
				var mn, mx uint32
				first := true
				for n := range pending {
					if first || n < mn {
						mn = n
					}
					if first || n > mx {
						mx = n
					}
					first = false
				}
				_, haveNext := pending[head+1]
				diag := ""
				if !haveNext {
					diag = " " + d.diagnoseAwaited(head+1)
				}
				d.logf("STALLCHK head=%d awaiting=%d haveNext=%v pending=%d range=[%d..%d]%s",
					head, head+1, haveNext, len(pending), mn, mx, diag)
			}
		case job := <-d.applyQ:
			d.onDequeue()
			if d.cfg.ForkAware {
				// Fork-choice engine owns ordering + forks; feed every block
				// (including same-height competitors the linear path would
				// drop) straight to it.
				forkApply(job)
				continue
			}
			head := d.headBlockHint.Load()
			if head == 0 {
				h, _ := d.chain.HeadBlock()
				head = h
			}
			if job.blockNum <= head {
				// Already applied.
				continue
			}
			// Bound the reorder buffer. New job goes in either way;
			// we evict the FURTHEST-FUTURE entry first so we keep the
			// blocks closest to head — those are the ones most likely
			// to fill the gap.
			if d.cfg.MaxPendingReorder > 0 && len(pending) >= d.cfg.MaxPendingReorder {
				evicted := evictHighest()
				if evicted != 0 && evicted == job.blockNum {
					// Whole buffer was further from head than this
					// job — keep it instead of the just-evicted one.
				}
				d.logf("reorder buffer full (%d), evicted block %d",
					d.cfg.MaxPendingReorder, evicted)
			}
			pending[job.blockNum] = job
			tryApply()
		}
	}
}

// liveAdvertiseWindowSec bounds how far behind wall-clock our head block
// may be and still be worth advertising via item_ids_inventory. Blocks
// are 3s apart, so a node receiving live blocks is at most a few seconds
// behind; catch-up (initial-sync) blocks are hours-to-years old. Mirrors
// gossip.liveBroadcastWindowSec (120s ~ 40 blocks) so the advert and
// relay paths agree on what "live" means.
const liveAdvertiseWindowSec = 120

// isCatchingUp reports whether we are still doing initial-sync catch-up,
// i.e. our head block's timestamp is more than liveAdvertiseWindowSec
// behind wall-clock (or we're at genesis). It is the single liveness
// signal driving both halves of the catch-up machinery:
//
//   - broadcastInv suppresses item_ids_inventory adverts while catching
//     up, so we never offer a historical block to a live-head peer (which
//     would disconnect us with "you offered me a block deemed invalid").
//   - catchupKickerLoop keeps re-issuing fetch_blockchain_item_ids to
//     every peer while catching up, so the inventory walk stays seeded
//     even if a walk-forward chain breaks (throttle / item_not_available
//     / dropped reorder block). Previously this gated on isFarBehind(),
//     which derived "behind" from per-peer peerHeadNum — but that only
//     advances ~one inventory batch (≈2000) ahead of our head, so once we
//     applied through the batch the gate went false and the kicker fell
//     silent, stalling the sync. The wall-clock timestamp can't be fooled
//     that way: it stays "catching up" until we genuinely reach the tip.
//
// On the rare path where the head bytes aren't cached (or are unparsable)
// we err toward "catching up" so the kicker keeps us moving rather than
// going silent.
// headAgeSec returns how many seconds old our head block's timestamp is, or a
// large sentinel when the head (or its cached bytes) can't be read. Used to
// tell "exactly level with the tip" from merely "near the tip" —
// isCatchingUp()'s 120s window is far too coarse for that distinction.
func (d *Delegate) headAgeSec() int64 {
	head := d.headBlockHint.Load()
	if head == 0 {
		h, _ := d.chain.HeadBlock()
		head = h
	}
	if head == 0 {
		return 1 << 40
	}
	raw := d.chain.RecentBlockBytes(head)
	if raw == nil {
		return 1 << 40
	}
	ts := blockTimestamp(raw)
	if ts == 0 {
		return 1 << 40
	}
	return time.Now().Unix() - int64(ts)
}

func (d *Delegate) isCatchingUp() bool {
	head := d.headBlockHint.Load()
	if head == 0 {
		head, _ = d.chain.HeadBlock()
	}
	if head == 0 {
		return true // at genesis — maximally behind
	}
	raw := d.chain.RecentBlockBytes(head)
	if raw == nil {
		return true
	}
	ts := blockTimestamp(raw)
	if ts == 0 {
		return true
	}
	return int64(ts) < time.Now().Unix()-liveAdvertiseWindowSec
}

// blockTimestamp peeks at the timestamp field of a raw block payload
// (signed_block_header). Returns 0 if the buffer is too short to hold
// the fixed-size header prefix where Timestamp lives. The signed header
// layout is: prev_id(20) + timestamp(4) + witness(varuint+ascii) +
// transaction_merkle_root(20) + extensions(...) + sig(65). Timestamp
// sits at byte offset 20.
func blockTimestamp(rawBlock []byte) uint32 {
	if len(rawBlock) < 24 {
		return 0
	}
	return uint32(rawBlock[20]) |
		uint32(rawBlock[21])<<8 |
		uint32(rawBlock[22])<<16 |
		uint32(rawBlock[23])<<24
}

const (
	// knownFutureSlack is how far beyond head an id may claim to be and still
	// be treated as a plausible block rather than a transaction hash. A few
	// hundred blocks covers any real reorg/catch-up window.
	knownFutureSlack = 1000

	// maxKnownPerPeer bounds the per-peer known set. Dedup is an optimisation,
	// not a correctness requirement: dropping an entry costs at most one
	// redundant fetch.
	maxKnownPerPeer = 200_000
)
