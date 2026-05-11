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

type blockHookScratch struct {
	conversionPending []blockHookConvertPending
	withdrawalPending []blockHookWithdrawalPending
	cashoutPending    []blockHookCashoutPending
}
