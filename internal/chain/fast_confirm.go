package chain

import (
	"fmt"

	"gopherd/internal/protocol/auth"
	"gopherd/internal/protocol/block"
	"gopherd/internal/protocol/hardfork"
	"gopherd/internal/protocol/network"
	"gopherd/internal/protocol/op"
	"gopherd/internal/protocol/tx"
	"gopherd/internal/state"
	"gopherd/internal/state/objects"
)

// fcRingSize is how many recently applied main-branch blocks the fast-confirm
// LIB search can see. hived walks its fork database, which holds the
// reversible window; with fast confirmation that window is a few blocks, so
// 128 (over six minutes of chain) only matters if confirmations stall.
const fcRingSize = 128

// fcBlock is one applied main-branch block: what the LIB search walks.
type fcBlock struct {
	num     uint32
	id      block.ID
	witness string
}

// IsFastConfirmTx mirrors database::is_fast_confirm_transaction
// (database.cpp:549): exactly one operation, and it is witness_block_approve.
func IsFastConfirmTx(t *tx.SignedTransaction) bool {
	return len(t.Operations) == 1 && t.Operations[0].Tag == op.TagWitnessBlockApprove
}

// noteAppliedForFastConfirm records an applied block for the LIB search.
// Called by ApplyBlock under the applyMu write lock. A block undone from the tip leaves its
// slot behind, but the search ignores anything above the head and a re-applied
// height overwrites it.
func (d *Database) noteAppliedForFastConfirm(num uint32, id block.ID, witness string) {
	d.fcRing[num%fcRingSize] = fcBlock{num: num, id: id, witness: witness}
}

// FastLIB is the last irreversible block as fast confirmations have moved it
// since the last applied block, which folds it into the DGPO. 0 if none.
func (d *Database) FastLIB() uint32 { return d.fastLIB.Load() }

// FastConfirmsAccepted counts approvals that replaced a witness's previous one.
func (d *Database) FastConfirmsAccepted() uint64 { return d.fcAccepted.Load() }

// ProcessFastConfirm ports database::process_fast_confirm_transaction
// (database.cpp:2941). A fast-confirm is never queued for a block: it records
// the witness's approval in memory and may advance the last irreversible
// block. Valid approvals that change nothing (witness not scheduled, or an
// approval no newer than the one we hold) return nil, as hived's do, so they
// are relayed like any accepted transaction.
//
// Differences from hived: a LIB found on another fork does not trigger a fork
// switch (the fork driver only reorgs at the tip), and the new LIB reaches the
// DGPO at the next applied block rather than immediately; until then FastLIB
// carries it.
func (d *Database) ProcessFastConfirm(raw []byte) error {
	res, err := readSignedTxBytes(raw)
	if err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	if !IsFastConfirmTx(&res.SignedTx) {
		return fmt.Errorf("not a fast-confirm transaction")
	}
	// Validation only reads state, so it shares the lock with admission; the
	// approvals map has fcMu of its own. ApplyBlock holds the write lock, which
	// excludes us entirely while it folds approvals into the LIB.
	d.applyMu.RLock()
	defer d.applyMu.RUnlock()

	session := state.NewSession(d.state)
	dgpo, err := objects.GetDGPO(session)
	if err != nil {
		return fmt.Errorf("dgpo: %w", err)
	}
	hf := hardfork.SetForBlock(dgpo.HeadBlockNumber + 1)
	if !hf.Has(hardfork.HF26) {
		return fmt.Errorf("fast confirmation transactions not valid until HF26")
	}
	// validate_transaction with skip_tapos_check: the approving witness may be
	// on another fork, so its reference block need not be in our chain.
	exp := res.SignedTx.Expiration
	maxDelta := maxTimeUntilExpirationSec
	if hf.Has(hardfork.HF28) {
		maxDelta = maxTimeUntilSignatureExpirationSec
	}
	if exp <= dgpo.Time || exp > dgpo.Time+maxDelta {
		return fmt.Errorf("fast-confirm expiration %d outside (%d, %d]", exp, dgpo.Time, dgpo.Time+maxDelta)
	}
	approve := res.SignedTx.Operations[0].AsWitnessBlockApprove()
	if v, ok := res.SignedTx.Operations[0].Operation().(op.Validator); ok {
		if verr := v.Validate(); verr != nil {
			return fmt.Errorf("op 0 invalid: %w", verr)
		}
	}
	unsigned := raw[res.UnsignedStart:res.UnsignedEnd]
	if err := VerifyAuthority(session, &res.SignedTx, unsigned, network.ChainID, false); err != nil {
		return fmt.Errorf("authority: %w", err)
	}

	witness := string(approve.Witness)
	sched, ok := irreversibilitySchedule(session, dgpo.HeadBlockNumber)
	if !ok || !containsName(sched, witness) {
		return nil // not on the schedule: ignored
	}
	id := block.ID(approve.BlockID)
	d.fcMu.Lock()
	defer d.fcMu.Unlock()
	if prev, ok := d.fcApproved[witness]; ok && id.Num() <= prev.Num() {
		return nil // already hold an approval at least as new
	}
	if d.fcApproved == nil {
		d.fcApproved = make(map[string]block.ID, 32)
	}
	d.fcApproved[witness] = id
	d.fcAccepted.Add(1)

	old := dgpo.LastIrreversibleBlockNum
	if f := d.fastLIB.Load(); f > old {
		old = f
	}
	if lib := d.fastConfirmLIB(sched, dgpo.HeadBlockNumber, old); lib > old {
		d.fastLIB.Store(lib)
		d.dropSettledApprovals(lib)
	}
	return nil
}

// irreversibilitySchedule returns the witnesses whose opinions decide
// irreversibility: get_witness_schedule_object_for_irreversibility
// (database.cpp:2928), the FUTURE schedule from HF26.
func irreversibilitySchedule(s *state.Session, headNum uint32) ([]auth.AccountName, bool) {
	wsRef, ok := objects.GetWitnessScheduleRef(s)
	if !ok {
		return nil, false
	}
	if hardfork.SetForBlock(headNum).Has(hardfork.HF26) {
		if fws, ok := objects.EnsureFutureWitnessScheduleStore(s).GetRef(); ok {
			wsRef = fws
		}
	}
	if wsRef.NumScheduledWitnesses == 0 {
		return nil, false
	}
	return wsRef.CurrentShuffledWitnesses, true
}

// IrreversibilitySchedule is irreversibilitySchedule for callers outside the
// chain package that hold a read session (the witness plugin, deciding whom
// to fast-confirm for). nil when there is no schedule yet.
func IrreversibilitySchedule(s *state.Session, headNum uint32) []auth.AccountName {
	sched, _ := irreversibilitySchedule(s, headNum)
	return sched
}

func containsName(names []auth.AccountName, name string) bool {
	for _, n := range names {
		if string(n) == name {
			return true
		}
	}
	return false
}

// fastConfirmLIB ports sync_block_writer::find_new_last_irreversible_block
// (sync_block_writer.cpp:267) over the main branch. Each scheduled witness
// votes once, for the newer of the block it last fast-approved and the block
// it last produced; a block is irreversible once enough witnesses voted for it
// or for a descendant. Returns 0 when nothing above oldLIB qualifies.
func (d *Database) fastConfirmLIB(sched []auth.AccountName, headNum, oldLIB uint32) uint32 {
	if headNum <= oldLIB || len(sched) == 0 {
		return 0
	}
	offset := int((hundredPercentBP - irreversibleThresholdBP) * uint32(len(sched)) / hundredPercentBP)
	required := len(sched) - offset

	lowest := oldLIB + 1
	if headNum >= fcRingSize && headNum-fcRingSize+1 > lowest {
		lowest = headNum - fcRingSize + 1
	}
	// mainAt reports the main-branch block at num, if the ring still holds it.
	mainAt := func(num uint32) (fcBlock, bool) {
		b := d.fcRing[num%fcRingSize]
		return b, b.num == num && num <= headNum
	}

	var votes [fcRingSize]uint8
	for _, name := range sched {
		w := string(name)
		var best uint32
		// The newest block this witness produced on our branch.
		for n := headNum; n >= lowest; n-- {
			if b, ok := mainAt(n); ok && b.witness == w {
				best = n
				break
			}
		}
		// A fast approval counts only for a block on our branch.
		if id, ok := d.fcApproved[w]; ok && id.Num() > best {
			if b, ok := mainAt(id.Num()); ok && b.id == id {
				best = id.Num()
			}
		}
		if best >= lowest {
			votes[best%fcRingSize]++
		}
	}
	approving := 0
	for n := headNum; n >= lowest; n-- {
		approving += int(votes[n%fcRingSize])
		if approving >= required {
			return n
		}
	}
	return 0
}

// dropSettledApprovals erases approvals of blocks at or below lib, as hived
// does once a new LIB is set (database.cpp:3093).
func (d *Database) dropSettledApprovals(lib uint32) {
	for w, id := range d.fcApproved {
		if id.Num() <= lib {
			delete(d.fcApproved, w)
		}
	}
}
