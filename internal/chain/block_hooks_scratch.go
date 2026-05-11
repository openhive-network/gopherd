package chain

// blockHookScratch bundles the per-call buffers the per-block hooks
// (process_conversions, process_vesting_withdrawals, process_comment_
// cashouts) used to allocate fresh on each call. The hooks run
// strictly sequentially inside ApplyBlock under d.applyMu, so a shared
// scratch on *Database is safe.

type blockHookConvertPending struct {
	conversion uint32
	requestID  uint32
	owner      string
}

type blockHookWithdrawalPending struct {
	nextWithdrawal uint32
	account        string
}

type blockHookCashoutPending struct {
	cashoutTime uint32
	commentID   uint64
	author      string
	permlink    string
}

// CuratorPay is the per-curator share record used inside
// distributeCuratorPool's sort+pay loop. Exported so the hook-scratch
// slice can be shared across the cashout cascade — one cashout fires
// distributeCuratorPool which allocates this list per call (and per
// closure capture for RangeCommentVotesByComment). With ~500K
// cashouts in the post-HF7 window, the per-call slice growth +
// reflectlite-swapper sort were burning real CPU.
type CuratorPay struct {
	Voter  string
	Weight uint64
	Share  int64
}

type blockHookScratch struct {
	conversionPending []blockHookConvertPending
	withdrawalPending []blockHookWithdrawalPending
	cashoutPending    []blockHookCashoutPending
	curatorPays       []CuratorPay
}
