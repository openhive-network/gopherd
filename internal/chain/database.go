// Package chain orchestrates block application: per-block setup, walking
// transactions and operations, dispatching to evaluators, and committing
// the session at block end.
package chain

import (
	"encoding/binary"
	"fmt"
	"os"
	"strings"

	"hived-go/internal/chain/evaluator"
	"hived-go/internal/fcraw"
	"hived-go/internal/protocol/auth"
	"hived-go/internal/protocol/block"
	"hived-go/internal/protocol/hardfork"
	"hived-go/internal/protocol/network"
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
	// lcbCache mirrors each witness's LastConfirmedBlockNum so the per-
	// block updateLastIrreversibleBlock can avoid 21 GetWitness reads
	// (each a btree lookup + full Witness deserialize). Updated whenever
	// updateProducerLastConfirmed bumps a witness; cache misses are
	// resolved by reading state and populating the entry. Single-writer
	// (only updateProducerLastConfirmed mutates LCB), so consistency
	// holds as long as we update cache and state together.
	lcbCache map[string]uint32

	// recent caches the FC-binary bytes of the last N successfully-applied
	// blocks so the p2p sync layer can re-serve them to peers requesting
	// `fetch_items` during catch-up. nil = caching disabled (callers that
	// don't need to serve sync — replay tools, tests — can leave it nil).
	recent *RecentBlocks

	// onBlockApplied is fired once after each successful ApplyBlock,
	// AFTER the recent-block cache is updated. Used by the p2p sync
	// layer to wake its INV broadcaster from idle backoff so the
	// new block reaches peers without waiting a full tick. nil =
	// no hook (default for replay/test contexts).
	onBlockApplied func(blockNum uint32, id block.ID)
}

// New wraps an opened state.Database for block application.
func New(s *state.Database) *Database {
	return &Database{
		state:    s,
		session:  state.NewSession(s),
		lcbCache: make(map[string]uint32, 32),
	}
}

// State returns the underlying state.Database.
func (d *Database) State() *state.Database { return d.state }

// EnableRecentBlockCache turns on the in-memory ring of last-N-applied
// raw block bytes. Required for serving p2p sync responses; callers that
// only replay block_log can leave it disabled. Safe to call once at
// startup (idempotent on the same capacity).
func (d *Database) EnableRecentBlockCache(capacity int) {
	if d.recent != nil {
		return
	}
	d.recent = NewRecentBlocks(capacity)
}

// SetOnBlockApplied registers a callback fired after every successful
// ApplyBlock. Pass nil to disable. Single-slot — last writer wins.
// The callback runs synchronously on the goroutine that called
// ApplyBlock, so it should return quickly (just notify a channel).
func (d *Database) SetOnBlockApplied(cb func(blockNum uint32, id block.ID)) {
	d.onBlockApplied = cb
}

// RecentBlockBytes returns the FC-binary bytes of blockNum if the recent-
// block cache is enabled and contains it, else nil.
func (d *Database) RecentBlockBytes(blockNum uint32) []byte {
	if d.recent == nil {
		return nil
	}
	return d.recent.ByNum(blockNum)
}

// RecentBlockBytesByID returns the FC-binary bytes of the block with the
// given block_id if the recent-block cache is enabled and contains it.
func (d *Database) RecentBlockBytesByID(id block.ID) []byte {
	if d.recent == nil {
		return nil
	}
	return d.recent.ByID(id)
}

// RecentBlockNums returns the block_nums currently held in the recent-
// block cache (oldest-first).
func (d *Database) RecentBlockNums() []uint32 {
	if d.recent == nil {
		return nil
	}
	return d.recent.RecentNums()
}

// RecentBlockIDForNum returns the cached block ID for blockNum.
func (d *Database) RecentBlockIDForNum(blockNum uint32) (block.ID, bool) {
	if d.recent == nil {
		return block.ID{}, false
	}
	return d.recent.IDForNum(blockNum)
}

// HeadBlockID returns the current head block's 20-byte ID per DGPO.
// Returns the zero ID + nil error when no blocks have been applied yet.
func (d *Database) HeadBlockID() (block.ID, error) {
	s := state.NewSession(d.state)
	p, err := objects.GetDGPO(s)
	if err == state.ErrNotFound {
		return block.ID{}, nil
	}
	if err != nil {
		return block.ID{}, err
	}
	return p.HeadBlockID, nil
}

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
	prevCurrentAslot := uint64(0)
	if dgpo, err := objects.GetDGPO(session); err == nil {
		headBlockTime = dgpo.Time
		prevCurrentAslot = dgpo.CurrentAslot
	}

	// Witness-scheduling assert + block-level invariants (chain
	// continuity, block size, merkle root, running_version). All
	// skipped on the replay path where the canonical block_log is
	// already validated; live sync sets opts.SkipBlockSignature=false
	// to engage them. Mirrors database.cpp:2335-2403.
	if !opts.SkipBlockSignature {
		if err := validateBlock(session, &hdr, rawBlock, hdrLen, blockNum, hf); err != nil {
			return objects.DynamicGlobalProperty{}, err
		}
		// Witness signature check: recover the pubkey from the
		// 65-byte compact signature over SHA256(header_bytes), and
		// compare to witness_object.BlockSigningKey. Mirrors hived's
		// validate_block_header in database.cpp:2887.
		if err := verifyBlockSignature(session, &hdr, rawBlock, hdrLen, blockNum); err != nil {
			return objects.DynamicGlobalProperty{}, err
		}
		if blockNum > 1 {
			slot := slotNumForBlockTime(headBlockTime, hdr.Timestamp)
			if slot > 0 {
				if ws, wErr := objects.GetWitnessSchedule(session); wErr == nil {
					expected := ws.ScheduledWitnessForSlotAslot(prevCurrentAslot, slot)
					if expected != "" && expected != hdr.Witness {
						return objects.DynamicGlobalProperty{}, fmt.Errorf(
							"apply_block %d: witness mismatch: header=%q expected=%q (slot=%d)",
							blockNum, hdr.Witness, expected, slot)
					}
				}
			}
		}
	}
	ctx := &evaluator.Context{
		DB:            session,
		Hardforks:     hf,
		BlockNum:      blockNum,
		BlockTime:     hdr.Timestamp,
		HeadBlockTime: headBlockTime,
		Witness:       hdr.Witness,
	}

	// Mirror C++ database.cpp:2374-2376 — set dgp.current_witness BEFORE
	// the tx loop runs, so PoW evaluators (and any other op that reads
	// dgpo.CurrentWitness) see the current block's producer rather than
	// the previous block's. Go's evaluator.Context.Witness is also set
	// to hdr.Witness (line 210), so the in-Context reads are independent;
	// this only matters for code paths that read dgpo directly.
	{
		dgpoPre, derr := objects.GetDGPO(session)
		if derr == nil {
			dgpoPre.CurrentWitness = hdr.Witness
			objects.PutDGPO(session, &dgpoPre)
		}
	}

	// Mirror C++ database.cpp:2379 — process_header_extensions runs
	// BEFORE the tx loop so witness running_version + hardfork_version_vote
	// reflect the current block when downstream evaluators or the HF5+
	// witness-version assertion read them.
	if err := processHeaderExtensions(session, &hdr); err != nil {
		return objects.DynamicGlobalProperty{}, fmt.Errorf("apply_block %d header_extensions: %w", blockNum, err)
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

		// Compute the tx_id (first 20 bytes of sha256 over the unsigned
		// transaction body). Used by both TaPOS and dedup checks below.
		unsignedBytes := rawBlock[hdrLen+txRes.UnsignedStart : hdrLen+txRes.UnsignedEnd]
		txID := tx.IDFromDigest(tx.DigestFromBytes(unsignedBytes))

		// Pre-flight checks: expiration window + dedup + TaPOS slot
		// existence. Pass `headBlockTime` (= dgpo.Time at start of
		// apply_block, i.e. PREVIOUS block's timestamp) — this is what
		// hived's push_transaction sees, NOT the current block's
		// timestamp. Using hdr.Timestamp here would make the expiration
		// window 3 seconds tighter than C++ and reject the trailing
		// edge of legitimate txs.
		// Signature verification (live-sync only — replay sets
		// SkipTransactionSignatures since the canonical block_log was
		// already validated upstream). Pre-HF24 the sig digest is
		// over OldChainID || unsigned_tx; HF24+ uses ChainID.
		if !opts.SkipTransactionSignatures {
			chainID := network.OldChainID
			if hf.Has(hardfork.HF24) {
				chainID = network.ChainID
			}
			unsignedBytes := rawBlock[hdrLen+txRes.UnsignedStart : hdrLen+txRes.UnsignedEnd]
			if err := VerifyAuthority(session, &txRes.SignedTx, unsignedBytes, chainID, false); err != nil {
				txFailed = true
				failOp = "tx-sigverify"
				failErr = err
				session.Restore(txSnap)
				if stats != nil {
					stats.recordSkip(failOp, failErr)
				}
				skippedThisBlock++
				if opts.TolerateFailingOps {
					continue
				}
				return objects.DynamicGlobalProperty{}, fmt.Errorf("apply_block %d tx %d: %w", blockNum, i, failErr)
			}
		}
		if err := validateTxPreFlight(session, &txRes.SignedTx, blockNum, headBlockTime, txID, !opts.SkipBlockSignature); err != nil {
			txFailed = true
			failOp = "tx-preflight"
			failErr = err
			session.Restore(txSnap)
			if stats != nil {
				stats.recordSkip(failOp, failErr)
			}
			skippedThisBlock++
			if opts.TolerateFailingOps {
				continue
			}
			return objects.DynamicGlobalProperty{}, fmt.Errorf("apply_block %d tx %d: %w", blockNum, i, failErr)
		}
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
		// Tx applied cleanly: record it in the dedup window so a future
		// rebroadcast of the same trx_id is rejected.
		objects.PutTransactionRecord(session, &objects.TransactionRecord{
			TrxID:      txID,
			Expiration: txRes.SignedTx.Expiration,
		})
	}
	if stats != nil {
		stats.OpsSkipped += skippedThisBlock
		if skippedThisBlock == 0 {
			stats.BlocksFull++
		} else {
			stats.BlocksDirty++
		}
	}

	// Mirror C++ database.cpp:2412 — update_global_dynamic_data after
	// tx application and before per-block hooks. CurrentWitness was
	// already set pre-tx-loop (mirrors C++ line 2374); here we advance
	// HeadBlockNumber, Time, HeadBlockID. Per-block hooks see the new
	// head block.
	//
	// update_global_dynamic_data also calls update_witness_missed_blocks
	// for each missed slot (database.cpp:2926-2933), incrementing their
	// total_missed and HF14-HF20 zeroing the signing_key for witnesses
	// out of touch beyond HIVE_WITNESS_SHUTDOWN_THRESHOLD blocks. We
	// run that BEFORE the DGPO advance so missed-witness lookups still
	// see the previous head block number.
	// Compute slot number BEFORE the missed-block walk so we can advance
	// CurrentAslot and pass the per-block slot to update_witness_missed_blocks.
	//
	// At block 1 hived's get_slot_at_time runs the head_block_num()==0
	// branch of get_slot_time, which uses dpo.time directly (= genesis
	// time, no rounding). Because Go's genesis seeds dgpo.Time with
	// network.GenesisTimeUnix and that value is on a 3-second boundary,
	// slotNumForBlockTime returns the same value as hived's get_slot_at_time
	// without any block-1 special case. For mainnet block 1 (timestamp
	// 1458835500, genesis 1458835200) this is 100 — the off-by-99 source
	// of the iter15 CurrentAslot drift, which Go was clamping to 1.
	//
	// update_witness_missed_blocks is gated by hived's
	// `head_block_num() != 0` guard; mirror that with `blockNum > 1`.
	slotForBlock := slotNumForBlockTime(headBlockTime, hdr.Timestamp)
	if blockNum > 1 && slotForBlock > 1 {
		updateWitnessMissedBlocks(session, hdr.Witness, slotForBlock-1, hf, blockNum)
	}
	dgpo, err := objects.GetDGPO(session)
	if err != nil && err != state.ErrNotFound {
		return objects.DynamicGlobalProperty{}, fmt.Errorf("apply_block %d dgpo read: %w", blockNum, err)
	}
	dgpo.HeadBlockNumber = blockNum
	dgpo.Time = hdr.Timestamp
	dgpo.HeadBlockID = block.ConstructID(rawBlock[:hdrLen], blockNum)
	// Advance CurrentAslot by slotForBlock — mirrors hived's
	// update_global_dynamic_data (database.cpp:2912-2920) which adds
	// the block's slot index (1 + missed slots since previous block) to
	// dgpo.current_aslot. ScheduledWitnessForSlot reads CurrentAslot to
	// resolve which witness should produce slot N from the schedule
	// shuffle. Without advancing on missed slots, miss-counter
	// attribution looks at the WRONG witness for each missed slot.
	dgpo.CurrentAslot += uint64(slotForBlock)
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
	// Producer's last_confirmed_block_num is bumped immediately on
	// successful block apply (mirrors C++ update_signing_witness which
	// updates the producer's record before the LIB recompute below).
	d.updateProducerLastConfirmed(session, hdr.Witness, blockNum)

	clearExpiredOrders(session, hdr.Timestamp)
	clearExpiredDelegations(session, hdr.Timestamp)
	updateWitnessSchedule(session, blockNum)
	updateMedianFeed(session, blockNum, hdr.Timestamp)
	updateVirtualSupply(session, blockNum)
	clearNullAccountBalance(session, blockNum)
	processProducerReward(session, hdr.Witness, blockNum)
	if envGhivediff != "" {
		// Per-block dump in the HF7-cascade-divergence window. Trace
		// fires AFTER processProducerReward but BEFORE processFunds —
		// matches C++'s trace location inside process_funds (which is
		// after get_producer_reward but before adjust_supply).
	}
	// GHIVEDIFF_ALL dump matches C++'s placement inside process_funds:
	// AFTER get_producer_reward (= our processProducerReward) but
	// BEFORE the content/curate/vesting inflation slice modify.
	if envGhivediffAll != "" {
		d, _ := objects.GetDGPO(session)
		fh, _ := objects.GetFeedHistory(session)
		fmt.Fprintf(os.Stderr, "GHIVEDIFF GLOBAL block=%d fund=%d vfund=%d vshares=%d supply=%d vsupply=%d hbd_supply=%d median=%d/%d\n",
			blockNum, d.TotalRewardFundHive, d.TotalVestingFundHive, d.TotalVestingShares,
			d.CurrentSupply, d.VirtualSupply, d.CurrentHBDSupply,
			fh.CurrentMedianHistory.Base.Amount, fh.CurrentMedianHistory.Quote.Amount)
	}
	// (Per-witness VST dump moved into updateWitnessSchedule4 to align
	// with hived's GHIVEDIFF_VSTS dump location.)
	// GHIVEDIFF_ACCOUNTS=acc1,acc2 — per-block balance dump for each
	// listed account. Mirrors hived's diagnostic block at
	// database.cpp:1832-1853.
	if accEnv := envGhivediffAccounts; accEnv != "" {
		for _, name := range strings.Split(accEnv, ",") {
			if name == "" {
				continue
			}
			a, err := objects.GetAccount(session, name)
			if err != nil {
				continue
			}
			fmt.Fprintf(os.Stderr, "GHIVEDIFF ACCT block=%d name=%s hive=%d hbd=%d vests=%d\n",
				blockNum, name, a.Balance, a.HBDBalance, a.VestingShares)
		}
	}
	processFunds(session, blockNum, hdr.Witness)
	if envGhivediff != "" {
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
		// One-shot witness vote total dump at block 2,920,000 (after
		// processFunds but before cashouts) for parity comparison.
		if blockNum == 2920000 {
			for _, name := range []string{"blocktrades", "steempty", "smooth.witness", "abit", "wackou", "riverhead", "witness.svk", "silversteem", "complexring", "kushed", "arhag", "clayop", "xeldal", "bitcube", "good-karma", "pfunk", "datasecuritynode"} {
				if w, e := objects.GetWitness(session, name); e == nil {
					fmt.Fprintf(os.Stderr, "GHIVEDIFF WITNESS block=%d name=%s votes=%d\n",
						blockNum, name, w.TotalVotesVests)
				}
			}
			d, _ := objects.GetDGPO(session)
			fh, _ := objects.GetFeedHistory(session)
			fmt.Fprintf(os.Stderr, "GHIVEDIFF GLOBAL block=%d fund=%d vfund=%d vshares=%d supply=%d vsupply=%d hbd_supply=%d median=%d/%d\n",
				blockNum, d.TotalRewardFundHive, d.TotalVestingFundHive, d.TotalVestingShares,
				d.CurrentSupply, d.VirtualSupply, d.CurrentHBDSupply,
				fh.CurrentMedianHistory.Base.Amount, fh.CurrentMedianHistory.Quote.Amount)
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

	objects.PutBlockSummary(session, blockNum, dgpo.HeadBlockID)
	objects.PruneExpiredTransactions(session, hdr.Timestamp)

	// RC subsystem finalize_block hook (HF20+). Runs the per-block
	// pool decay + expired-delegation refund. Pre-HF20 only the
	// expiration sweep fires (safe — table is empty).
	rcFinalizeBlock(session, blockNum, hdr.Timestamp)

	// LIB recompute runs after the new schedule + producer-confirm are
	// in place. Mirrors database.cpp:2870 update_last_irreversible_block.
	d.updateLastIrreversibleBlock(session)

	// Re-read dgpo so the returned value reflects per-block hook
	// updates (supply, vesting fund, etc.).
	if updated, err := objects.GetDGPO(session); err == nil {
		dgpo = updated
	}

	if err := session.Commit(opts.Sync); err != nil {
		return dgpo, fmt.Errorf("apply_block %d commit: %w", blockNum, err)
	}
	// Cache the raw block bytes so the p2p sync layer can re-serve them
	// to peers requesting `fetch_items`. Done after commit so callers
	// only see entries for blocks that successfully landed.
	if d.recent != nil {
		raw := append([]byte(nil), rawBlock...)
		d.recent.Put(blockNum, dgpo.HeadBlockID, raw)
	}
	if d.onBlockApplied != nil {
		d.onBlockApplied(blockNum, dgpo.HeadBlockID)
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
	return string(ws.ScheduledWitnessForSlotAslot(dgpo.CurrentAslot, slotNum)), nil
}

// HIVE_BLOCK_INTERVAL: seconds between blocks (libraries/protocol/include/
// hive/protocol/config.hpp).
const blockIntervalSec uint32 = 3

// slotNumForBlockTime computes the slot number of a block whose timestamp
// is `blockTime`, given that the chain head is at `headTime`. slot 1 = the
// next contiguous block; slot 2 = one missed; etc. Returns 0 if the block
// is at or before head_time (which would be a clock-rewind error upstream).
func slotNumForBlockTime(headTime, blockTime uint32) uint32 {
	// Mirror hived's get_slot_at_time (database.cpp:1015-1020) which
	// uses get_slot_time(1) = head_slot_time + 1*interval, where
	// head_slot_time = (head_block_time / interval) * interval —
	// ROUNDED DOWN to the previous interval boundary. Without the
	// alignment, blocks where head_block_time isn't a multiple of
	// interval would be miscounted (Go returned 0, hived returned 1).
	// Source of the 99-slot CurrentAslot gap observed at block
	// 5,148,656 vs hived.
	headSlotTime := (headTime / blockIntervalSec) * blockIntervalSec
	firstSlotTime := headSlotTime + blockIntervalSec
	if blockTime < firstSlotTime {
		return 0
	}
	return (blockTime-firstSlotTime)/blockIntervalSec + 1
}

// hiveWitnessShutdownThresholdBlocks = HIVE_BLOCKS_PER_DAY = 28800.
// Pre-HF14 / post-HF20 the shutdown branch is a no-op (HF14 introduced it,
// HF20 disabled it via SP190). Within HF14-HF20, a witness whose
// last_confirmed_block_num is more than this many blocks behind head_block
// has its signing_key zeroed (effectively dropping them from the schedule).
const hiveWitnessShutdownThresholdBlocks uint32 = 28800

// updateWitnessMissedBlocks mirrors database::update_witness_missed_blocks
// (database_witness.cpp:284-306). For each missed slot 1..missedBlocks, it
// finds the witness who SHOULD have produced that slot, increments their
// total_missed counter, and HF14-HF20 zeroes their BlockSigningKey if
// their last_confirmed is too far behind head — which causes the next
// witness-schedule rebuild to drop them from the active set entirely.
func updateWitnessMissedBlocks(s *state.Session, blockWitness auth.AccountName, missedBlocks uint32, hf hardfork.Set, blockNum uint32) {
	if missedBlocks == 0 {
		return
	}
	ws, err := objects.GetWitnessSchedule(s)
	if err != nil {
		return
	}
	// hived: per-slot witness lookup uses dgpo.current_aslot, NOT
	// head_block_num. iter16 closed the upstream CurrentAslot drift
	// (slotNumForBlockTime now rounds head_time DOWN to interval
	// boundary, matching hived's get_slot_at_time).
	dgpoForAslot, _ := objects.GetDGPO(s)
	currentAslot := dgpoForAslot.CurrentAslot
	for i := uint32(0); i < missedBlocks; i++ {
		missedName := ws.ScheduledWitnessForSlotAslot(currentAslot, i+1)
		if missedName == "" || missedName == blockWitness {
			continue
		}
		w, err := objects.GetWitness(s, missedName)
		if err != nil {
			continue
		}
		w.TotalMissedBlocks++
		if hf.Has(hardfork.HF14) && !hf.Has(hardfork.HF20) {
			if blockNum-w.LastConfirmedBlockNum > hiveWitnessShutdownThresholdBlocks {
				w.BlockSigningKey = [33]byte{}
			}
		}
		objects.PutWitness(s, &w)
	}
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
