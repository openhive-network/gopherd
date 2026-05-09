package sync

import (
	"time"

	"hived-go/internal/chain"
	"hived-go/internal/p2p/node"
	"hived-go/internal/p2p/proto"
	"hived-go/internal/protocol/block"
)

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
	nums := d.chain.RecentBlockNums()
	if len(nums) == 0 {
		// Nothing to advertise. Bump the idle counter.
		d.idleMu.Lock()
		d.idleRounds++
		d.idleMu.Unlock()
		return
	}

	// Truncate to the most-recent MaxBlocksPerInv.
	if len(nums) > d.cfg.MaxBlocksPerInv {
		nums = nums[len(nums)-d.cfg.MaxBlocksPerInv:]
	}

	// Pre-resolve nums → IDs once per round.
	allIDs := make([]block.ID, 0, len(nums))
	for _, n := range nums {
		if id, ok := d.chain.RecentBlockIDForNum(n); ok {
			allIDs = append(allIDs, id)
		}
	}
	if len(allIDs) == 0 {
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

// applyLoop is the SINGLE consumer of d.applyQ. Pulls inbound blocks
// off the queue and feeds them to chain.ApplyBlock in arrival order.
// Mirrors the witness production constraint: chain mutation must happen
// on a single goroutine.
//
// Blocks that arrive out of order (i.e. before the immediate next
// block) are stashed in pending and re-considered after each apply.
func (d *Delegate) applyLoop() {
	defer d.wg.Done()
	defer close(d.applyDone)

	pending := make(map[uint32]applyJob)

	tryApply := func() {
		for {
			head, _ := d.chain.HeadBlock()
			next := head + 1
			job, ok := pending[next]
			if !ok {
				return
			}
			delete(pending, next)
			if _, err := d.chain.ApplyBlock(job.rawBlock, job.blockNum, chain.ApplyOptions{
				Sync:                      false,
				SkipTransactionSignatures: false,
				SkipBlockSignature:        false,
			}, nil); err != nil {
				d.logf("apply_block %d failed: %v", job.blockNum, err)
				// Drop and continue — agent-gossip / next INV will
				// re-deliver if the chain is on a recoverable fork.
				continue
			}
			d.poke()
		}
	}

	for {
		select {
		case <-d.stopCh:
			return
		case job := <-d.applyQ:
			head, _ := d.chain.HeadBlock()
			if job.blockNum <= head {
				// Already applied.
				continue
			}
			pending[job.blockNum] = job
			tryApply()
		}
	}
}
