package op

import (
	"encoding/binary"
	"fmt"

	"hived-go/internal/fcraw"
	"hived-go/internal/protocol/asset"
	"hived-go/internal/protocol/auth"
)

// ---- 0 vote --------------------------------------------------------------

type Vote struct {
	Voter    auth.AccountName
	Author   auth.AccountName
	Permlink string
	Weight   int16
}

func (Vote) OpTag() Tag { return TagVote }
func (o Vote) AppendFCBody(dst []byte) []byte {
	dst = appendString(dst, o.Voter)
	dst = appendString(dst, o.Author)
	dst = appendString(dst, o.Permlink)
	return binary.LittleEndian.AppendUint16(dst, uint16(o.Weight))
}

// voteSlab is a per-Reader scratch pool for *Vote slots, stashed in
// fcraw.Reader.Aux. The replay/apply path consumes votes within the
// op loop; once ApplyBlock has dispatched a block's ops the previous
// block's votes are dead, so the slab can be reset (n=0) and the
// backing storage reused.
//
// Within a block: handouts come from buf[n], with n monotonically
// increasing. A grow appends a new entry and leaves prior pointers
// valid (Go's append keeps old backing alive via the Operation
// interface that referenced &buf[i] from earlier handouts).
//
// sync.Pool would also work but its atomic Get/Put cost exceeded
// new(Vote)'s 80-byte alloc cost in a prior attempt (see
// feedback memo). The non-atomic slab here is single-goroutine
// under chain.applyMu.
type voteSlab struct {
	buf []Vote
	n   int
}

// ResetVoteSlab resets the per-Reader Vote slab to n=0, reusing the
// existing backing storage. Called by the block-apply loop before
// parsing each block. Safe to call when Aux is unset or holds a
// non-voteSlab type (no-op).
func ResetVoteSlab(r *fcraw.Reader) {
	if s, ok := r.Aux.(*voteSlab); ok {
		s.n = 0
	}
}

func readVote(r *fcraw.Reader) (Operation, error) {
	var o *Vote
	if s, ok := r.Aux.(*voteSlab); ok {
		if s.n < len(s.buf) {
			o = &s.buf[s.n]
			*o = Vote{}
			s.n++
		} else {
			s.buf = append(s.buf, Vote{})
			s.n = len(s.buf)
			o = &s.buf[s.n-1]
		}
	} else {
		s := &voteSlab{buf: make([]Vote, 0, 128)}
		r.Aux = s
		s.buf = append(s.buf, Vote{})
		s.n = 1
		o = &s.buf[0]
	}
	var err error
	if o.Voter, err = r.ReadString(); err != nil {
		return nil, err
	}
	if o.Author, err = r.ReadString(); err != nil {
		return nil, err
	}
	if o.Permlink, err = r.ReadString(); err != nil {
		return nil, err
	}
	w, err := r.ReadUint16()
	if err != nil {
		return nil, err
	}
	o.Weight = int16(w)
	return o, nil
}

// ---- 2 transfer ---------------------------------------------------------

type Transfer struct {
	From   auth.AccountName
	To     auth.AccountName
	Amount asset.Asset
	Memo   string
}

func (Transfer) OpTag() Tag { return TagTransfer }
func (o Transfer) AppendFCBody(dst []byte) []byte {
	dst = appendString(dst, o.From)
	dst = appendString(dst, o.To)
	dst = asset.AppendAsset(dst, o.Amount)
	return appendString(dst, o.Memo)
}

func readTransfer(r *fcraw.Reader) (Operation, error) {
	var o Transfer
	var err error
	if o.From, err = r.ReadString(); err != nil {
		return nil, err
	}
	if o.To, err = r.ReadString(); err != nil {
		return nil, err
	}
	if o.Amount, err = asset.ReadAsset(r); err != nil {
		return nil, err
	}
	if o.Memo, err = r.ReadString(); err != nil {
		return nil, err
	}
	return o, nil
}

// ---- 3 transfer_to_vesting ----------------------------------------------

type TransferToVesting struct {
	From   auth.AccountName
	To     auth.AccountName
	Amount asset.Asset
}

func (TransferToVesting) OpTag() Tag { return TagTransferToVesting }
func (o TransferToVesting) AppendFCBody(dst []byte) []byte {
	dst = appendString(dst, o.From)
	dst = appendString(dst, o.To)
	return asset.AppendAsset(dst, o.Amount)
}

func readTransferToVesting(r *fcraw.Reader) (Operation, error) {
	var o TransferToVesting
	var err error
	if o.From, err = r.ReadString(); err != nil {
		return nil, err
	}
	if o.To, err = r.ReadString(); err != nil {
		return nil, err
	}
	if o.Amount, err = asset.ReadAsset(r); err != nil {
		return nil, err
	}
	return o, nil
}

// ---- 4 withdraw_vesting -------------------------------------------------

type WithdrawVesting struct {
	Account       auth.AccountName
	VestingShares asset.Asset
}

func (WithdrawVesting) OpTag() Tag { return TagWithdrawVesting }
func (o WithdrawVesting) AppendFCBody(dst []byte) []byte {
	dst = appendString(dst, o.Account)
	return asset.AppendAsset(dst, o.VestingShares)
}

func readWithdrawVesting(r *fcraw.Reader) (Operation, error) {
	var o WithdrawVesting
	var err error
	if o.Account, err = r.ReadString(); err != nil {
		return nil, err
	}
	if o.VestingShares, err = asset.ReadAsset(r); err != nil {
		return nil, err
	}
	return o, nil
}

// ---- 6 limit_order_cancel -----------------------------------------------

type LimitOrderCancel struct {
	Owner   auth.AccountName
	OrderID uint32
}

func (LimitOrderCancel) OpTag() Tag { return TagLimitOrderCancel }
func (o LimitOrderCancel) AppendFCBody(dst []byte) []byte {
	dst = appendString(dst, o.Owner)
	return binary.LittleEndian.AppendUint32(dst, o.OrderID)
}

func readLimitOrderCancel(r *fcraw.Reader) (Operation, error) {
	var o LimitOrderCancel
	var err error
	if o.Owner, err = r.ReadString(); err != nil {
		return nil, err
	}
	if o.OrderID, err = r.ReadUint32(); err != nil {
		return nil, err
	}
	return o, nil
}

// ---- 7 feed_publish -----------------------------------------------------

type FeedPublish struct {
	Publisher    auth.AccountName
	ExchangeRate asset.Price
}

func (FeedPublish) OpTag() Tag { return TagFeedPublish }
func (o FeedPublish) AppendFCBody(dst []byte) []byte {
	dst = appendString(dst, o.Publisher)
	return asset.AppendPrice(dst, o.ExchangeRate)
}

func readFeedPublish(r *fcraw.Reader) (Operation, error) {
	var o FeedPublish
	var err error
	if o.Publisher, err = r.ReadString(); err != nil {
		return nil, err
	}
	if o.ExchangeRate, err = asset.ReadPrice(r); err != nil {
		return nil, err
	}
	return o, nil
}

// ---- 8 convert ----------------------------------------------------------

type Convert struct {
	Owner     auth.AccountName
	RequestID uint32
	Amount    asset.Asset
}

func (Convert) OpTag() Tag { return TagConvert }
func (o Convert) AppendFCBody(dst []byte) []byte {
	dst = appendString(dst, o.Owner)
	dst = binary.LittleEndian.AppendUint32(dst, o.RequestID)
	return asset.AppendAsset(dst, o.Amount)
}

func readConvert(r *fcraw.Reader) (Operation, error) {
	var o Convert
	var err error
	if o.Owner, err = r.ReadString(); err != nil {
		return nil, err
	}
	if o.RequestID, err = r.ReadUint32(); err != nil {
		return nil, err
	}
	if o.Amount, err = asset.ReadAsset(r); err != nil {
		return nil, err
	}
	return o, nil
}

// ---- 12 account_witness_vote --------------------------------------------

type AccountWitnessVote struct {
	Account auth.AccountName
	Witness auth.AccountName
	Approve bool
}

func (AccountWitnessVote) OpTag() Tag { return TagAccountWitnessVote }
func (o AccountWitnessVote) AppendFCBody(dst []byte) []byte {
	dst = appendString(dst, o.Account)
	dst = appendString(dst, o.Witness)
	if o.Approve {
		return append(dst, 1)
	}
	return append(dst, 0)
}

func readAccountWitnessVote(r *fcraw.Reader) (Operation, error) {
	var o AccountWitnessVote
	var err error
	if o.Account, err = r.ReadString(); err != nil {
		return nil, err
	}
	if o.Witness, err = r.ReadString(); err != nil {
		return nil, err
	}
	if o.Approve, err = r.ReadBool(); err != nil {
		return nil, err
	}
	return o, nil
}

// ---- 13 account_witness_proxy -------------------------------------------

type AccountWitnessProxy struct {
	Account auth.AccountName
	Proxy   auth.AccountName
}

func (AccountWitnessProxy) OpTag() Tag { return TagAccountWitnessProxy }
func (o AccountWitnessProxy) AppendFCBody(dst []byte) []byte {
	dst = appendString(dst, o.Account)
	return appendString(dst, o.Proxy)
}

func readAccountWitnessProxy(r *fcraw.Reader) (Operation, error) {
	var o AccountWitnessProxy
	var err error
	if o.Account, err = r.ReadString(); err != nil {
		return nil, err
	}
	if o.Proxy, err = r.ReadString(); err != nil {
		return nil, err
	}
	return o, nil
}

// ---- 16 witness_block_approve -------------------------------------------

type WitnessBlockApprove struct {
	Witness auth.AccountName
	BlockID [20]byte
}

func (WitnessBlockApprove) OpTag() Tag { return TagWitnessBlockApprove }
func (o WitnessBlockApprove) AppendFCBody(dst []byte) []byte {
	dst = appendString(dst, o.Witness)
	return append(dst, o.BlockID[:]...)
}

func readWitnessBlockApprove(r *fcraw.Reader) (Operation, error) {
	var o WitnessBlockApprove
	var err error
	if o.Witness, err = r.ReadString(); err != nil {
		return nil, err
	}
	if err = r.ReadInto(o.BlockID[:]); err != nil {
		return nil, err
	}
	return o, nil
}

// ---- 17 delete_comment --------------------------------------------------

type DeleteComment struct {
	Author   auth.AccountName
	Permlink string
}

func (DeleteComment) OpTag() Tag { return TagDeleteComment }
func (o DeleteComment) AppendFCBody(dst []byte) []byte {
	dst = appendString(dst, o.Author)
	return appendString(dst, o.Permlink)
}

func readDeleteComment(r *fcraw.Reader) (Operation, error) {
	var o DeleteComment
	var err error
	if o.Author, err = r.ReadString(); err != nil {
		return nil, err
	}
	if o.Permlink, err = r.ReadString(); err != nil {
		return nil, err
	}
	return o, nil
}

// ---- 20 set_withdraw_vesting_route --------------------------------------

type SetWithdrawVestingRoute struct {
	FromAccount auth.AccountName
	ToAccount   auth.AccountName
	Percent     uint16
	AutoVest    bool
}

func (SetWithdrawVestingRoute) OpTag() Tag { return TagSetWithdrawVestingRoute }
func (o SetWithdrawVestingRoute) AppendFCBody(dst []byte) []byte {
	dst = appendString(dst, o.FromAccount)
	dst = appendString(dst, o.ToAccount)
	dst = binary.LittleEndian.AppendUint16(dst, o.Percent)
	if o.AutoVest {
		return append(dst, 1)
	}
	return append(dst, 0)
}

func readSetWithdrawVestingRoute(r *fcraw.Reader) (Operation, error) {
	var o SetWithdrawVestingRoute
	var err error
	if o.FromAccount, err = r.ReadString(); err != nil {
		return nil, err
	}
	if o.ToAccount, err = r.ReadString(); err != nil {
		return nil, err
	}
	if o.Percent, err = r.ReadUint16(); err != nil {
		return nil, err
	}
	if o.AutoVest, err = r.ReadBool(); err != nil {
		return nil, err
	}
	return o, nil
}

// ---- 22 claim_account ---------------------------------------------------

type ClaimAccount struct {
	Creator    auth.AccountName
	Fee        asset.Asset
	Extensions []byte
}

func (ClaimAccount) OpTag() Tag { return TagClaimAccount }
func (o ClaimAccount) AppendFCBody(dst []byte) []byte {
	dst = appendString(dst, o.Creator)
	dst = asset.AppendAsset(dst, o.Fee)
	return appendRawExtensions(dst, o.Extensions)
}

func readClaimAccount(r *fcraw.Reader) (Operation, error) {
	var o ClaimAccount
	var err error
	if o.Creator, err = r.ReadString(); err != nil {
		return nil, err
	}
	if o.Fee, err = asset.ReadAsset(r); err != nil {
		return nil, err
	}
	if o.Extensions, err = readExtensionsRaw(r, "extensions", futureExtensionsTags); err != nil {
		return nil, err
	}
	return o, nil
}

// ---- 28 escrow_dispute --------------------------------------------------

type EscrowDispute struct {
	From, To, Agent, Who auth.AccountName
	EscrowID             uint32
}

func (EscrowDispute) OpTag() Tag { return TagEscrowDispute }
func (o EscrowDispute) AppendFCBody(dst []byte) []byte {
	dst = appendString(dst, o.From)
	dst = appendString(dst, o.To)
	dst = appendString(dst, o.Agent)
	dst = appendString(dst, o.Who)
	return binary.LittleEndian.AppendUint32(dst, o.EscrowID)
}

func readEscrowDispute(r *fcraw.Reader) (Operation, error) {
	var o EscrowDispute
	o.From, _ = r.ReadString()
	o.To, _ = r.ReadString()
	o.Agent, _ = r.ReadString()
	if v, err := r.ReadString(); err == nil {
		o.Who = v
	} else {
		return nil, err
	}
	id, err := r.ReadUint32()
	if err != nil {
		return nil, err
	}
	o.EscrowID = id
	return o, nil
}

// ---- 31 escrow_approve --------------------------------------------------

type EscrowApprove struct {
	From, To, Agent, Who auth.AccountName
	EscrowID             uint32
	Approve              bool
}

func (EscrowApprove) OpTag() Tag { return TagEscrowApprove }
func (o EscrowApprove) AppendFCBody(dst []byte) []byte {
	dst = appendString(dst, o.From)
	dst = appendString(dst, o.To)
	dst = appendString(dst, o.Agent)
	dst = appendString(dst, o.Who)
	dst = binary.LittleEndian.AppendUint32(dst, o.EscrowID)
	if o.Approve {
		return append(dst, 1)
	}
	return append(dst, 0)
}

func readEscrowApprove(r *fcraw.Reader) (Operation, error) {
	var o EscrowApprove
	var err error
	for _, dst := range []*string{&o.From, &o.To, &o.Agent, &o.Who} {
		if *dst, err = r.ReadString(); err != nil {
			return nil, err
		}
	}
	if o.EscrowID, err = r.ReadUint32(); err != nil {
		return nil, err
	}
	if o.Approve, err = r.ReadBool(); err != nil {
		return nil, err
	}
	return o, nil
}

// ---- 32 transfer_to_savings ---------------------------------------------

type TransferToSavings struct {
	From   auth.AccountName
	To     auth.AccountName
	Amount asset.Asset
	Memo   string
}

func (TransferToSavings) OpTag() Tag { return TagTransferToSavings }
func (o TransferToSavings) AppendFCBody(dst []byte) []byte {
	dst = appendString(dst, o.From)
	dst = appendString(dst, o.To)
	dst = asset.AppendAsset(dst, o.Amount)
	return appendString(dst, o.Memo)
}

func readTransferToSavings(r *fcraw.Reader) (Operation, error) {
	var o TransferToSavings
	var err error
	if o.From, err = r.ReadString(); err != nil {
		return nil, err
	}
	if o.To, err = r.ReadString(); err != nil {
		return nil, err
	}
	if o.Amount, err = asset.ReadAsset(r); err != nil {
		return nil, err
	}
	if o.Memo, err = r.ReadString(); err != nil {
		return nil, err
	}
	return o, nil
}

// ---- 33 transfer_from_savings -------------------------------------------

type TransferFromSavings struct {
	From      auth.AccountName
	RequestID uint32
	To        auth.AccountName
	Amount    asset.Asset
	Memo      string
}

func (TransferFromSavings) OpTag() Tag { return TagTransferFromSavings }
func (o TransferFromSavings) AppendFCBody(dst []byte) []byte {
	dst = appendString(dst, o.From)
	dst = binary.LittleEndian.AppendUint32(dst, o.RequestID)
	dst = appendString(dst, o.To)
	dst = asset.AppendAsset(dst, o.Amount)
	return appendString(dst, o.Memo)
}

func readTransferFromSavings(r *fcraw.Reader) (Operation, error) {
	var o TransferFromSavings
	var err error
	if o.From, err = r.ReadString(); err != nil {
		return nil, err
	}
	if o.RequestID, err = r.ReadUint32(); err != nil {
		return nil, err
	}
	if o.To, err = r.ReadString(); err != nil {
		return nil, err
	}
	if o.Amount, err = asset.ReadAsset(r); err != nil {
		return nil, err
	}
	if o.Memo, err = r.ReadString(); err != nil {
		return nil, err
	}
	return o, nil
}

// ---- 34 cancel_transfer_from_savings ------------------------------------

type CancelTransferFromSavings struct {
	From      auth.AccountName
	RequestID uint32
}

func (CancelTransferFromSavings) OpTag() Tag { return TagCancelTransferFromSavings }
func (o CancelTransferFromSavings) AppendFCBody(dst []byte) []byte {
	dst = appendString(dst, o.From)
	return binary.LittleEndian.AppendUint32(dst, o.RequestID)
}

func readCancelTransferFromSavings(r *fcraw.Reader) (Operation, error) {
	var o CancelTransferFromSavings
	var err error
	if o.From, err = r.ReadString(); err != nil {
		return nil, err
	}
	if o.RequestID, err = r.ReadUint32(); err != nil {
		return nil, err
	}
	return o, nil
}

// ---- 36 decline_voting_rights -------------------------------------------

type DeclineVotingRights struct {
	Account auth.AccountName
	Decline bool
}

func (DeclineVotingRights) OpTag() Tag { return TagDeclineVotingRights }
func (o DeclineVotingRights) AppendFCBody(dst []byte) []byte {
	dst = appendString(dst, o.Account)
	if o.Decline {
		return append(dst, 1)
	}
	return append(dst, 0)
}

func readDeclineVotingRights(r *fcraw.Reader) (Operation, error) {
	var o DeclineVotingRights
	var err error
	if o.Account, err = r.ReadString(); err != nil {
		return nil, err
	}
	if o.Decline, err = r.ReadBool(); err != nil {
		return nil, err
	}
	return o, nil
}

// ---- 37 reset_account ---------------------------------------------------

type ResetAccount struct {
	ResetAccount      auth.AccountName
	AccountToReset    auth.AccountName
	NewOwnerAuthority auth.Authority
}

func (ResetAccount) OpTag() Tag { return TagResetAccount }
func (o ResetAccount) AppendFCBody(dst []byte) []byte {
	dst = appendString(dst, o.ResetAccount)
	dst = appendString(dst, o.AccountToReset)
	return auth.AppendAuthority(dst, o.NewOwnerAuthority)
}

func readResetAccount(r *fcraw.Reader) (Operation, error) {
	var o ResetAccount
	var err error
	if o.ResetAccount, err = r.ReadString(); err != nil {
		return nil, err
	}
	if o.AccountToReset, err = r.ReadString(); err != nil {
		return nil, err
	}
	if o.NewOwnerAuthority, err = auth.ReadAuthority(r); err != nil {
		return nil, err
	}
	return o, nil
}

// ---- 38 set_reset_account -----------------------------------------------

type SetResetAccount struct {
	Account             auth.AccountName
	CurrentResetAccount auth.AccountName
	ResetAccount        auth.AccountName
}

func (SetResetAccount) OpTag() Tag { return TagSetResetAccount }
func (o SetResetAccount) AppendFCBody(dst []byte) []byte {
	dst = appendString(dst, o.Account)
	dst = appendString(dst, o.CurrentResetAccount)
	return appendString(dst, o.ResetAccount)
}

func readSetResetAccount(r *fcraw.Reader) (Operation, error) {
	var o SetResetAccount
	var err error
	if o.Account, err = r.ReadString(); err != nil {
		return nil, err
	}
	if o.CurrentResetAccount, err = r.ReadString(); err != nil {
		return nil, err
	}
	if o.ResetAccount, err = r.ReadString(); err != nil {
		return nil, err
	}
	return o, nil
}

// ---- 39 claim_reward_balance --------------------------------------------

type ClaimRewardBalance struct {
	Account      auth.AccountName
	RewardHive   asset.Asset
	RewardHbd    asset.Asset
	RewardVests  asset.Asset
}

func (ClaimRewardBalance) OpTag() Tag { return TagClaimRewardBalance }
func (o ClaimRewardBalance) AppendFCBody(dst []byte) []byte {
	dst = appendString(dst, o.Account)
	dst = asset.AppendAsset(dst, o.RewardHive)
	dst = asset.AppendAsset(dst, o.RewardHbd)
	return asset.AppendAsset(dst, o.RewardVests)
}

func readClaimRewardBalance(r *fcraw.Reader) (Operation, error) {
	var o ClaimRewardBalance
	var err error
	if o.Account, err = r.ReadString(); err != nil {
		return nil, err
	}
	if o.RewardHive, err = asset.ReadAsset(r); err != nil {
		return nil, err
	}
	if o.RewardHbd, err = asset.ReadAsset(r); err != nil {
		return nil, err
	}
	if o.RewardVests, err = asset.ReadAsset(r); err != nil {
		return nil, err
	}
	return o, nil
}

// ---- 40 delegate_vesting_shares -----------------------------------------

type DelegateVestingShares struct {
	Delegator     auth.AccountName
	Delegatee     auth.AccountName
	VestingShares asset.Asset
}

func (DelegateVestingShares) OpTag() Tag { return TagDelegateVestingShares }
func (o DelegateVestingShares) AppendFCBody(dst []byte) []byte {
	dst = appendString(dst, o.Delegator)
	dst = appendString(dst, o.Delegatee)
	return asset.AppendAsset(dst, o.VestingShares)
}

func readDelegateVestingShares(r *fcraw.Reader) (Operation, error) {
	var o DelegateVestingShares
	var err error
	if o.Delegator, err = r.ReadString(); err != nil {
		return nil, err
	}
	if o.Delegatee, err = r.ReadString(); err != nil {
		return nil, err
	}
	if o.VestingShares, err = asset.ReadAsset(r); err != nil {
		return nil, err
	}
	return o, nil
}

// ---- 48 collateralized_convert ------------------------------------------

type CollateralizedConvert struct {
	Owner     auth.AccountName
	RequestID uint32
	Amount    asset.Asset
}

func (CollateralizedConvert) OpTag() Tag { return TagCollateralizedConvert }
func (o CollateralizedConvert) AppendFCBody(dst []byte) []byte {
	dst = appendString(dst, o.Owner)
	dst = binary.LittleEndian.AppendUint32(dst, o.RequestID)
	return asset.AppendAsset(dst, o.Amount)
}

func readCollateralizedConvert(r *fcraw.Reader) (Operation, error) {
	var o CollateralizedConvert
	var err error
	if o.Owner, err = r.ReadString(); err != nil {
		return nil, err
	}
	if o.RequestID, err = r.ReadUint32(); err != nil {
		return nil, err
	}
	if o.Amount, err = asset.ReadAsset(r); err != nil {
		return nil, err
	}
	return o, nil
}

// shared helper for length-prefixed strings with no allocation surprises.
func appendString(dst []byte, s string) []byte {
	dst = fcraw.AppendVarUint(dst, uint32(len(s)))
	return append(dst, s...)
}

// trivial "use the package" wedge so unused-import errors don't pop up if
// the codecs above are pruned in a future refactor.
var _ = fmt.Sprintf

func init() {
	register(TagVote, readVote)
	register(TagTransfer, readTransfer)
	register(TagTransferToVesting, readTransferToVesting)
	register(TagWithdrawVesting, readWithdrawVesting)
	register(TagLimitOrderCancel, readLimitOrderCancel)
	register(TagFeedPublish, readFeedPublish)
	register(TagConvert, readConvert)
	register(TagAccountWitnessVote, readAccountWitnessVote)
	register(TagAccountWitnessProxy, readAccountWitnessProxy)
	register(TagWitnessBlockApprove, readWitnessBlockApprove)
	register(TagDeleteComment, readDeleteComment)
	register(TagSetWithdrawVestingRoute, readSetWithdrawVestingRoute)
	register(TagClaimAccount, readClaimAccount)
	register(TagEscrowDispute, readEscrowDispute)
	register(TagEscrowApprove, readEscrowApprove)
	register(TagTransferToSavings, readTransferToSavings)
	register(TagTransferFromSavings, readTransferFromSavings)
	register(TagCancelTransferFromSavings, readCancelTransferFromSavings)
	register(TagDeclineVotingRights, readDeclineVotingRights)
	register(TagResetAccount, readResetAccount)
	register(TagSetResetAccount, readSetResetAccount)
	register(TagClaimRewardBalance, readClaimRewardBalance)
	register(TagDelegateVestingShares, readDelegateVestingShares)
	register(TagCollateralizedConvert, readCollateralizedConvert)
}
