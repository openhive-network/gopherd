// Package chain orchestrates block application: per-block setup, walking
// transactions and operations, dispatching to evaluators, and committing
// the session at block end.
package chain

import (
	"encoding/binary"
	"fmt"

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
}

// New wraps an opened state.Database for block application.
func New(s *state.Database) *Database { return &Database{state: s} }

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

	session := state.NewSession(d.state)
	hf := hardfork.SetForBlock(blockNum)
	ctx := &evaluator.Context{
		DB:        session,
		Hardforks: hf,
		BlockNum:  blockNum,
		BlockTime: hdr.Timestamp,
		Witness:   hdr.Witness,
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
		for j, o := range txRes.SignedTx.Operations {
			if err := evaluator.Apply(ctx, o); err != nil {
				if opts.SkipUnknownOps && isNotImplemented(err) {
					skippedThisBlock++
					if stats != nil {
						stats.recordSkip(o.OpTag().Name(), err)
					}
					continue
				}
				if opts.TolerateFailingOps {
					skippedThisBlock++
					if stats != nil {
						stats.recordSkip(o.OpTag().Name(), err)
					}
					continue
				}
				return objects.DynamicGlobalProperty{}, fmt.Errorf("apply_block %d tx %d op %d: %w", blockNum, i, j, err)
			}
			if stats != nil {
				stats.OpsApplied++
			}
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

	// Per-block hooks (mirror hived's database::process_funds and
	// adjacent chain maintenance, in order):
	//   1. pay the producing witness their per-block reward
	//   2. apply chain inflation (content + vesting reward) to dgpo
	//   3. fire any vesting withdrawals whose interval elapsed
	//   4. cash out any comments whose payout window closed
	// Hook order mirrors libraries/chain/database.cpp:2335 in apply_block:
	// process_funds → process_conversions → process_comment_cashout →
	// process_vesting_withdrawals → process_savings_withdraws → ...
	// process_funds in C++ also dispatches the producer-reward (we do
	// that explicitly upstream so the witness's vesting state is set
	// before vesting-share inflation runs).
	processProducerReward(session, hdr.Witness, blockNum)
	processFunds(session, blockNum)
	processCommentCashouts(session, hdr.Timestamp, blockNum)
	processVestingWithdrawals(session, hdr.Timestamp)
	processSavingsWithdraws(session, hdr.Timestamp)
	clearExpiredOrders(session, hdr.Timestamp)

	// Roll forward the DGPO. We always have enough info here even if no
	// evaluator touched it.
	dgpo, err := objects.GetDGPO(session)
	if err != nil && err != state.ErrNotFound {
		return objects.DynamicGlobalProperty{}, fmt.Errorf("apply_block %d dgpo read: %w", blockNum, err)
	}
	dgpo.HeadBlockNumber = blockNum
	dgpo.Time = hdr.Timestamp
	dgpo.CurrentWitness = hdr.Witness
	dgpo.HeadBlockID = block.ConstructID(rawBlock[:hdrLen], blockNum)
	objects.PutDGPO(session, &dgpo)

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
