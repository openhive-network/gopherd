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
	// Created is the voter account's creation time, used as the tie-break
	// on equal Weight — hived's pay_curators cmp orders equal-weight votes
	// by voter account_id (creation order), NOT by voter name. See
	// distributeCuratorPool.
	Created uint32
	// Seq is the voter's account Seq (account_id + 1), the exact tie-break.
	Seq uint64
}

// cashoutSibling is a per-cashout sibling record used in the pre-HF17
// thread-cashout grouping loop. Pooled across cashouts via the
// blockHookScratch so the per-cashout siblings slice (~430 MB cum on
// an 8M run) reuses its backing storage.
type cashoutSibling struct {
	author      string
	permlink    string
	cashoutTime uint32
	id          uint64
}

// cashoutPaidKey is a struct key for the "already paid this cashout"
// dedup map — avoids the per-sibling `author + "/" + permlink` string
// concat (~286 MB cum on an 8M run).
type cashoutPaidKey struct {
	author   string
	permlink string
}

type blockHookScratch struct {
	conversionPending []blockHookConvertPending
	withdrawalPending []blockHookWithdrawalPending
	cashoutPending    []blockHookCashoutPending
	curatorPays       []CuratorPay
	siblings          []cashoutSibling
	paid              map[cashoutPaidKey]struct{}
}
