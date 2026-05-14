package chain

import (
	"hived-go/internal/protocol/asset"
	"hived-go/internal/protocol/auth"
)

// BlockVOp captures the small set of virtual operations the Go chain
// currently records during ApplyBlock so tests can assert against them
// (e.g. the recurrent-transfer parity test that inspects pair_id on the
// fill_recurrent_transfer / failed_recurrent_transfer vops).
//
// Hived's full virtual-op pipeline (push_virtual_operation +
// post_push_virtual_operation + account_history_plugin) is not yet
// ported. This struct is intentionally lightweight — a tagged union
// over the few vops the test suite needs to observe. Buffer is cleared
// at the top of every ApplyBlock and exposed via LastBlockVOps().
type BlockVOpKind uint8

const (
	BlockVOpFillRecurrentTransfer BlockVOpKind = iota + 1
	BlockVOpFailedRecurrentTransfer
)

// BlockVOp is the union record. Only the fields relevant to the kind
// are populated. For *RecurrentTransfer vops:
//
//	From, To, Memo, Amount, RemainingExecutions, PairID are always set.
//	ConsecutiveFailures + Deleted are set on Failed*.
//
// Mirrors hived's:
//
//	fill_recurrent_transfer_operation(from, to, amount, memo, remaining_executions)
//	failed_recurrent_transfer_operation(from, to, amount, consecutive_failures, memo, remaining_executions, deleted)
type BlockVOp struct {
	Kind                BlockVOpKind
	From                auth.AccountName
	To                  auth.AccountName
	Amount              asset.Asset
	Memo                string
	RemainingExecutions uint16
	ConsecutiveFailures uint8
	Deleted             bool
	PairID              uint8
}

// LastBlockVOps returns a copy of the virtual ops emitted during the
// most recent successful ApplyBlock. Safe to call concurrently with
// other reads — takes the same RLock as the other read-side accessors.
func (d *Database) LastBlockVOps() []BlockVOp {
	d.applyMu.RLock()
	defer d.applyMu.RUnlock()
	if len(d.vops) == 0 {
		return nil
	}
	out := make([]BlockVOp, len(d.vops))
	copy(out, d.vops)
	return out
}

// pushVOp appends a virtual op to the per-block buffer. Caller must
// hold the write lock (ApplyBlock holds it for the entire block).
func (d *Database) pushVOp(v BlockVOp) {
	d.vops = append(d.vops, v)
}

// resetVOps truncates the per-block vop buffer at the top of ApplyBlock.
func (d *Database) resetVOps() {
	d.vops = d.vops[:0]
}
