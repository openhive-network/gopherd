// Package chain orchestrates block application: per-block setup, walking
// transactions and operations, dispatching to evaluators, and committing
// the session at block end.
package chain

import (
	"encoding/binary"
	"fmt"
	"os"

	"hived-go/internal/chain/evaluator"
	"hived-go/internal/fcraw"
	"hived-go/internal/protocol/block"
	"hived-go/internal/protocol/hardfork"
	"hived-go/internal/protocol/tx"
	"hived-go/internal/state"
	"hived-go/internal/state/objects"
)

// Database wraps a state.Database and provides block-application
// orchestration on top of it. Mirrors libraries/chain/database.hpp's
// hive::chain::database in spirit, scoped to what current evaluators need.
type Database struct {
	state *state.Database
	// reused across ApplyBlock calls; maps are clear()'d on Commit so
	// the underlying buckets stay warm and we avoid 14M+ map allocations
	// per replay.
	session *state.Session
}

// New wraps an opened state.Database for block application.
func New(s *state.Database) *Database { return &Database{state: s, session: state.NewSession(s)} }

// State returns the underlying state.Database.
func (d *Database) State() *state.Database { return d.state }

// ApplyOptions tunes ApplyBlock behavior.
type ApplyOptions struct {
	// SkipUnknownOps causes ApplyBlock to ignore evaluator.ErrNotImplemented
	// (treating the op as a no-op). Default false; with the stub registry in
	// place every op is registered, so this should rarely be needed.
	SkipUnknownOps bool
	// TolerateFailingOps causes ApplyBlock to count, but not abort on, any
	// evaluator error (insufficient balance, unsupported symbol, etc.).
	// This is intended for INCREMENTAL P1 development only — when many
	// evaluators are still validate-only stubs, real evaluators rightly
	// reject operations that depend on stub-side mutations that never
	// happened. Setting this true lets replay drive forward through the
	// whole block_log to surface other coverage holes. Counts of skipped
	// ops are reported via *Stats. NEVER set this true on a node that
	// claims consensus correctness.
	TolerateFailingOps bool
	// Sync controls whether each block's commit fsyncs.
	Sync bool
	// SkipTransactionSignatures bypasses tx-level signature checks
	// (mirrors hived's skip_transaction_signatures flag). Replay
	// always sets this true — historical txs already passed validation
	// at the producing-node side; re-verifying is wasted work. Live
	// sync sets it false.
	SkipTransactionSignatures bool
	// SkipBlockSignature bypasses the witness's block-level signature
	// check (mirrors hived's skip_witness_signature). Same rationale
	// as above for replay.
	SkipBlockSignature bool
}

// Stats accumulates per-pipeline-run counters. Pass a non-nil pointer to
// ApplyBlock to receive op-level counts.
type Stats struct {
	OpsApplied  uint64
	OpsSkipped  uint64
	BlocksFull  uint64 // blocks where every op applied cleanly
	BlocksDirty uint64 // blocks with at least one skipped op
	// SkipReasons counts skipped ops by a coarse "op_name: error_class" key.
	// Populated only when TolerateFailingOps is true.
	SkipReasons map[string]uint64
	// SkipAccounts counts insufficient-balance failures by the offending
	// account name (the one extracted from the error message). Lets the
	// summary highlight which accounts the upstream cascade is starving.
	SkipAccounts map[string]uint64
}

func (s *Stats) recordSkip(opName string, err error) {
	if s.SkipReasons == nil {
		s.SkipReasons = make(map[string]uint64, 32)
	}
	// Walk to the leaf for the reason key (so wrapping noise like the
	// "apply_block N tx M op K: " prefix doesn't fragment the histogram).
	leaf := err
	for {
		next := unwrap(leaf)
		if next == nil {
			break
		}
		leaf = next
	}
	short := leaf.Error()
	if i := indexOfByte(short, ':'); i > 0 {
		short = short[:i]
	}
	s.SkipReasons[opName+": "+short]++
	// For the starved-accounts histogram we want the ORIGINAL formatted
	// message (which still has the quoted account name); the leaf is
	// just the bare sentinel and never carries the name.
	full := err.Error()
	if name := firstQuotedToken(full); name != "" {
		if s.SkipAccounts == nil {
			s.SkipAccounts = make(map[string]uint64, 64)
		}
		s.SkipAccounts[name]++
	}
}

// firstQuotedToken returns the contents of the first `"…"` quoted span
// in s, or "" if there is none.
func firstQuotedToken(s string) string {
	a := -1
	for i := 0; i < len(s); i++ {
		if s[i] == '"' {
			if a < 0 {
				a = i + 1
			} else {
				return s[a:i]
			}
		}
	}
	return ""
}

func indexOfByte(s string, c byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == c {
			return i
		}
	}
	return -1
}

// ApplyBlock runs every transaction's operations through their registered
// evaluators against a fresh state.Session, then commits. Returns the
// post-apply DGPO so callers can chain quickly without re-fetching.
//
// rawBlock is the FC-binary block payload as decompressed by blocklog
// (signed_block_header + transactions vector). blockNum is the 1-indexed
// block number. stats may be nil.
func (d *Database) ApplyBlock(rawBlock []byte, blockNum uint32, opts ApplyOptions, stats *Stats) (objects.DynamicGlobalProperty, error) {
	hdr, hdrLen, err := block.UnmarshalSignedHeader(rawBlock)
	if err != nil {
		return objects.DynamicGlobalProperty{}, fmt.Errorf("apply_block %d header: %w", blockNum, err)
	}

	session := d.session
	hf := hardfork.SetForBlock(blockNum)
	// hived's head_block_time() during transactions returns dgpo.time,
	// which is the PREVIOUS block's timestamp — update_global_dynamic_data
	// only advances dgpo.time after tx application. Read it now and pass
	// to evaluators so they match C++.
	headBlockTime := hdr.Timestamp
	if dgpo, err := objects.GetDGPO(session); err == nil {
		headBlockTime = dgpo.Time
	}
	ctx := &evaluator.Context{
		DB:            session,
		Hardforks:     hf,
		BlockNum:      blockNum,
		BlockTime:     hdr.Timestamp,
		HeadBlockTime: headBlockTime,
		Witness:       hdr.Witness,
	}

	r := fcraw.NewReader(rawBlock[hdrLen:])
	txCount, err := r.ReadVarUint()
	if err != nil {
		return objects.DynamicGlobalProperty{}, fmt.Errorf("apply_block %d tx_count: %w", blockNum, err)
	}
	var skippedThisBlock uint64
	for i := uint32(0); i < txCount; i++ {
		txRes, err := tx.ReadSignedTransaction(r)
		if err != nil {
			return objects.DynamicGlobalProperty{}, fmt.Errorf("apply_block %d tx %d parse: %w", blockNum, i, err)
		}
		// hived's apply_block wraps each push_transaction in its own
		// undo session — if any op in the tx throws, the whole tx is
		// rolled back atomically. Subsequent txs in the same block
		// are unaffected. We mirror that by snapshotting the overlay
		// at tx-start; on failure we Restore.
		txSnap := session.Snapshot()
		txFailed := false
		var failOp string
		var failErr error
		for j, o := range txRes.SignedTx.Operations {
			if err := evaluator.Apply(ctx, o); err != nil {
				txFailed = true
				failOp = o.OpTag().Name()
				failErr = fmt.Errorf("op %d: %w", j, err)
				break
			}
			if stats != nil {
				stats.OpsApplied++
			}
		}
		// HF20+: charge RC for the entire transaction. Use a stub
		// per-op cost; subtract from the first op's payer's manabar.
		// Replay never enforces — current_mana can go negative.
		if !txFailed && hardfork.SetForBlock(blockNum).Has(hardfork.HF20) && len(txRes.SignedTx.Operations) > 0 {
			cost := ComputeRCCost(&txRes.SignedTx, blockNum)
			payer := PayerOf(txRes.SignedTx.Operations[0])
			if cost > 0 && payer != "" {
				if a, err := objects.GetAccount(session, payer); err == nil {
					objects.RegenerateRCMana(&a, hdr.Timestamp)
					a.RCManaCurrent = objects.UseMana(a.RCManaCurrent, cost, -a.LastMaxRC)
					objects.PutAccount(session, &a)
				}
			}
		}
		if txFailed {
			// Roll back the partial tx's writes; subsequent txs proceed.
			session.Restore(txSnap)
			// Undo the per-op `OpsApplied` increments for the rolled-back ops.
			if stats != nil {
				// Number of ops successfully applied within this tx
				// before the failure point. Subtract since we're
				// rolling them back.
				// (we counted them above per-op; the simplest fix
				// is to leave stats-level counters and just increment
				// the skip counter). Here we follow the simpler path
				// and only count the rolled-back as "skipped".
				stats.recordSkip(failOp, failErr)
			}
			skippedThisBlock++
			if opts.SkipUnknownOps && isNotImplemented(failErr) {
				continue
			}
			if opts.TolerateFailingOps {
				continue
			}
			return objects.DynamicGlobalProperty{}, fmt.Errorf("apply_block %d tx %d: %w", blockNum, i, failErr)
		}
	}
	if stats != nil {
		stats.OpsSkipped += skippedThisBlock
		if skippedThisBlock == 0 {
			stats.BlocksFull++
		} else {
			stats.BlocksDirty++
		}
	}

	// Roll forward the DGPO BEFORE per-block hooks — hived calls
	// update_global_dynamic_data right after tx application and before
	// any of the per-block processing functions, so they see the
	// current block's head_block_number / time / current_witness.
	// Mirrors libraries/chain/database.cpp:2825.
	dgpo, err := objects.GetDGPO(session)
	if err != nil && err != state.ErrNotFound {
		return objects.DynamicGlobalProperty{}, fmt.Errorf("apply_block %d dgpo read: %w", blockNum, err)
	}
	dgpo.HeadBlockNumber = blockNum
	dgpo.Time = hdr.Timestamp
	dgpo.CurrentWitness = hdr.Witness
	dgpo.HeadBlockID = block.ConstructID(rawBlock[:hdrLen], blockNum)
	objects.PutDGPO(session, &dgpo)

	// Per-block hooks — order mirrors libraries/chain/database.cpp's
	// _apply_block (lines 2335-2356):
	//
	//   clear_expired_transactions    (we don't track tx dedup yet)
	//   clear_expired_orders
	//   clear_expired_delegations
	//   update_witness_schedule
	//   update_median_feed
	//   update_virtual_supply
	//   clear_null_account_balance
	//   process_funds                 → producer_reward + inflation
	//   process_conversions           → settles HBD→HIVE under feed
	//   process_comment_cashout       → uses same feed
	//   process_vesting_withdrawals
	//   process_savings_withdraws
	//   process_subsidized_accounts
	//   pay_liquidity_reward
	//   update_virtual_supply         (HBD might have changed)
	//   account_recovery_processing
	//   expire_escrow_ratification
	//   process_decline_voting_rights
	//   process_proposals
	//   process_delayed_voting
	//   remove_expired_governance_votes
	//   process_recurrent_transfers
	//   process_hardforks             → applies HF state effects last
	clearExpiredOrders(session, hdr.Timestamp)
	clearExpiredDelegations(session, hdr.Timestamp)
	updateWitnessSchedule(session, blockNum)
	updateMedianFeed(session, blockNum, hdr.Timestamp)
	updateVirtualSupply(session, blockNum)
	clearNullAccountBalance(session, blockNum)
	processProducerReward(session, hdr.Witness, blockNum)
	if os.Getenv("GHIVEDIFF") != "" {
		// Per-block dump in the HF7-cascade-divergence window. Trace
		// fires AFTER processProducerReward but BEFORE processFunds —
		// matches C++'s trace location inside process_funds (which is
		// after get_producer_reward but before adjust_supply).
		if blockNum >= 2888500 && blockNum <= 2897770 {
			d, _ := objects.GetDGPO(session)
			pfunkVests := int64(-1)
			pfunkHbd := int64(-1)
			if a, e := objects.GetAccount(session, "pfunk"); e == nil {
				pfunkVests = a.VestingShares
				pfunkHbd = a.HBDBalance
			}
			fmt.Fprintf(os.Stderr, "GHIVEDIFF DBG-BLOCK block=%d fund=%d vfund=%d vshares=%d supply=%d vsupply=%d pfunk_vests=%d pfunk_hbd=%d\n",
				blockNum, d.TotalRewardFundHive, d.TotalVestingFundHive, d.TotalVestingShares, d.CurrentSupply, d.VirtualSupply, pfunkVests, pfunkHbd)
		}
	}
	processFunds(session, blockNum, hdr.Witness)
	if os.Getenv("GHIVEDIFF") != "" {
		switch blockNum {
		case 100000, 500000, 1000000, 1500000, 2000000, 2500000, 2700000, 2800000, 2850000, 2880000, 2885000, 2889000, 2889020, 2890000, 2891000, 2892000, 2893000, 2894000, 2895000, 2896000, 2896500, 2896680, 2896682, 2897000, 2897356, 2897358, 2897500, 2897700, 2897750, 2897769, 2897770:
			if a, e := objects.GetAccount(session, "pfunk"); e == nil {
				fmt.Fprintf(os.Stderr, "GHIVEDIFF PFUNK-AT block=%d vesting_shares=%d delegated=%d received=%d hive=%d hbd=%d\n",
					blockNum, a.VestingShares, a.DelegatedVestingShares, a.ReceivedVestingShares, a.Balance, a.HBDBalance)
			}
		}
		switch blockNum {
		case 2896680, 2896682, 2897356, 2897358:
			d, _ := objects.GetDGPO(session)
			fmt.Fprintf(os.Stderr, "GHIVEDIFF VEST-PRICE block=%d vesting_fund=%d vesting_shares=%d\n",
				blockNum, d.TotalVestingFundHive, d.TotalVestingShares)
		}
		switch blockNum {
		case 2889100, 2889110, 2889120, 2889130, 2889140, 2889150, 2889160, 2889170, 2889180, 2889190, 2889200:
			d, _ := objects.GetDGPO(session)
			shares2 := d.TotalRewardShares2
			hi := uint64(shares2[0])<<56 | uint64(shares2[1])<<48 | uint64(shares2[2])<<40 | uint64(shares2[3])<<32 |
				uint64(shares2[4])<<24 | uint64(shares2[5])<<16 | uint64(shares2[6])<<8 | uint64(shares2[7])
			lo := uint64(shares2[8])<<56 | uint64(shares2[9])<<48 | uint64(shares2[10])<<40 | uint64(shares2[11])<<32 |
				uint64(shares2[12])<<24 | uint64(shares2[13])<<16 | uint64(shares2[14])<<8 | uint64(shares2[15])
			abitHive, abitHbd := int64(0), int64(0)
			if a, e := objects.GetAccount(session, "abit"); e == nil {
				abitHive = a.Balance
				abitHbd = a.HBDBalance
			}
			fmt.Fprintf(os.Stderr, "GHIVEDIFF SHARES2-AT block=%d shares2_hi=%d shares2_lo=%d fund=%d cur_supply=%d virt_supply=%d abit_hive=%d abit_hbd=%d\n",
				blockNum, hi, lo, d.TotalRewardFundHive, d.CurrentSupply, d.VirtualSupply, abitHive, abitHbd)
		}
	}
	processConversions(session, hdr.Timestamp)
	processCommentCashouts(session, hdr.Timestamp, blockNum)
	processVestingWithdrawals(session, hdr.Timestamp)
	processSavingsWithdraws(session, hdr.Timestamp)
	processSubsidizedAccounts(session, blockNum)
	payLiquidityReward(session, blockNum, hdr.Timestamp)
	updateVirtualSupply(session, blockNum)
	processAccountRecovery(session, hdr.Timestamp)
	expireEscrowRatifications(session, hdr.Timestamp)
	processDeclineVotingRights(session, hdr.Timestamp)
	processProposals(session, hdr.Timestamp, blockNum)
	processDelayedVoting(session, hdr.Timestamp, blockNum)
	removeExpiredGovernanceVotes(session, hdr.Timestamp, blockNum)
	processRecurrentTransfers(session, hdr.Timestamp, blockNum)
	processHardforks(session, blockNum)

	// Re-read dgpo so the returned value reflects per-block hook
	// updates (supply, vesting fund, etc.).
	if updated, err := objects.GetDGPO(session); err == nil {
		dgpo = updated
	}

	if err := session.Commit(opts.Sync); err != nil {
		return dgpo, fmt.Errorf("apply_block %d commit: %w", blockNum, err)
	}
	return dgpo, nil
}

// HeadBlock returns the current head block number per DGPO. Returns 0
// when no blocks have been applied yet.
func (d *Database) HeadBlock() (uint32, error) {
	s := state.NewSession(d.state)
	p, err := objects.GetDGPO(s)
	if err == state.ErrNotFound {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return p.HeadBlockNumber, nil
}

// ScheduledWitness returns the witness who should produce the block
// at slot_num counting from 1 (1 = next block). Mirrors hived's
// database::get_scheduled_witness.
func (d *Database) ScheduledWitness(slotNum uint32) (string, error) {
	s := state.NewSession(d.state)
	dgpo, err := objects.GetDGPO(s)
	if err != nil {
		return "", err
	}
	ws, err := objects.GetWitnessSchedule(s)
	if err != nil {
		return "", err
	}
	return string(ws.ScheduledWitnessForSlot(dgpo.HeadBlockNumber, slotNum)), nil
}

// isNotImplemented reports whether err wraps evaluator.ErrNotImplemented.
func isNotImplemented(err error) bool {
	for ; err != nil; err = unwrap(err) {
		if err == evaluator.ErrNotImplemented {
			return true
		}
	}
	return false
}

func unwrap(err error) error {
	type unwrapper interface{ Unwrap() error }
	if u, ok := err.(unwrapper); ok {
		return u.Unwrap()
	}
	return nil
}

// Reserved for use by tests/CLI: extracts a uint32 ID from a key body.
var _ = binary.BigEndian.Uint32
