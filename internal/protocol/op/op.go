// Package op implements the 50 regular hive::protocol operations as Go
// structs with FC binary codecs. The operation static_variant tag ordering
// in libraries/protocol/include/hive/protocol/operations.hpp is the source
// of truth — these tags are consensus-frozen and must never change.
//
// See per-op-file comments for field-level details. Cross-cutting wire
// notes:
//
//   - Each operation is encoded as `varint(tag) || fields...` inside a
//     transaction's operations vector.
//   - Field order matches the FC_REFLECT macro in the corresponding C++
//     header. Adding a field is a hardfork-level change.
//   - extensions_type fields (flat_set<future_extensions>) are typically a
//     single 0x00 byte (empty set). We model them as raw bytes captured
//     from the wire so re-marshalling is bit-identical.
package op

import (
	"fmt"

	"hived-go/internal/fcraw"
)

// Tag is the static_variant tag of a regular operation. Values 0..49 are
// regular ops; 50..92 are virtual ops which never appear in transactions
// (block-only). We define names for both ranges so logs and debug tools
// can pretty-print them, but only regular ops have decoders.
type Tag uint8

// Regular operation tags — order matches the static_variant declaration in
// libraries/protocol/include/hive/protocol/operations.hpp.
const (
	TagVote                          Tag = 0
	TagComment                       Tag = 1
	TagTransfer                      Tag = 2
	TagTransferToVesting             Tag = 3
	TagWithdrawVesting               Tag = 4
	TagLimitOrderCreate              Tag = 5
	TagLimitOrderCancel              Tag = 6
	TagFeedPublish                   Tag = 7
	TagConvert                       Tag = 8
	TagAccountCreate                 Tag = 9
	TagAccountUpdate                 Tag = 10
	TagWitnessUpdate                 Tag = 11
	TagAccountWitnessVote            Tag = 12
	TagAccountWitnessProxy           Tag = 13
	TagPow                           Tag = 14
	TagCustom                        Tag = 15
	TagWitnessBlockApprove           Tag = 16
	TagDeleteComment                 Tag = 17
	TagCustomJSON                    Tag = 18
	TagCommentOptions                Tag = 19
	TagSetWithdrawVestingRoute       Tag = 20
	TagLimitOrderCreate2             Tag = 21
	TagClaimAccount                  Tag = 22
	TagCreateClaimedAccount          Tag = 23
	TagRequestAccountRecovery        Tag = 24
	TagRecoverAccount                Tag = 25
	TagChangeRecoveryAccount         Tag = 26
	TagEscrowTransfer                Tag = 27
	TagEscrowDispute                 Tag = 28
	TagEscrowRelease                 Tag = 29
	TagPow2                          Tag = 30
	TagEscrowApprove                 Tag = 31
	TagTransferToSavings             Tag = 32
	TagTransferFromSavings           Tag = 33
	TagCancelTransferFromSavings     Tag = 34
	TagCustomBinary                  Tag = 35
	TagDeclineVotingRights           Tag = 36
	TagResetAccount                  Tag = 37
	TagSetResetAccount               Tag = 38
	TagClaimRewardBalance            Tag = 39
	TagDelegateVestingShares         Tag = 40
	TagAccountCreateWithDelegation   Tag = 41
	TagWitnessSetProperties          Tag = 42
	TagAccountUpdate2                Tag = 43
	TagCreateProposal                Tag = 44
	TagUpdateProposalVotes           Tag = 45
	TagRemoveProposal                Tag = 46
	TagUpdateProposal                Tag = 47
	TagCollateralizedConvert         Tag = 48
	TagRecurrentTransfer             Tag = 49

	// First virtual-op tag. Tags >= TagFirstVirtual must never appear inside
	// a transaction's operations vector; they are constructed by evaluators
	// and emitted into the block's virtual-op stream.
	TagFirstVirtual Tag = 50
)

// Operation is implemented by every regular operation Go struct.
type Operation interface {
	OpTag() Tag
	// AppendFCBody encodes the operation body (everything after the tag varint)
	// to dst.
	AppendFCBody(dst []byte) []byte
}

// AppendOperation writes a tagged operation: varint(tag) || body.
func AppendOperation(dst []byte, op Operation) []byte {
	dst = fcraw.AppendVarUint(dst, uint32(op.OpTag()))
	return op.AppendFCBody(dst)
}

// readerFn decodes the body of one operation (the bytes following the tag
// varint). It returns the constructed Op or an error.
type readerFn func(*fcraw.Reader) (Op, error)

// readers maps Tag → body decoder. Populated by init() in ops_*.go.
var readers [TagFirstVirtual]readerFn

// register associates a body decoder with a tag. Called from init() in
// per-op files.
func register(tag Tag, fn readerFn) {
	if int(tag) >= len(readers) {
		panic(fmt.Sprintf("op.register: tag %d out of range", tag))
	}
	if readers[tag] != nil {
		panic(fmt.Sprintf("op.register: tag %d already registered", tag))
	}
	readers[tag] = fn
}

// readOpBody parses the body of one operation (after the tag varint).
// Shared by ReadOp and ReadOperation.
func readOpBody(r *fcraw.Reader) (Op, error) {
	tagVal, err := r.ReadVarUint()
	if err != nil {
		return Op{}, fmt.Errorf("op.ReadOperation tag: %w", err)
	}
	if tagVal >= uint32(TagFirstVirtual) {
		return Op{}, fmt.Errorf("op.ReadOperation: virtual op tag %d cannot appear in a transaction", tagVal)
	}
	tag := Tag(tagVal)
	fn := readers[tag]
	if fn == nil {
		return Op{}, fmt.Errorf("op.ReadOperation: tag %d (%s) not yet implemented", tag, tag.Name())
	}
	o, err := fn(r)
	if err != nil {
		return Op{}, fmt.Errorf("op.ReadOperation tag %d (%s): %w", tag, tag.Name(), err)
	}
	return o, nil
}

// ReadOperation parses a single tagged operation from r: varint(tag) || body.
// Returns the boxed Operation iface for the legacy API. Unknown tags
// produce a clear error so callers can identify gaps in coverage.
func ReadOperation(r *fcraw.Reader) (Operation, error) {
	o, err := readOpBody(r)
	if err != nil {
		return nil, err
	}
	return o.Operation(), nil
}

// Name returns the canonical name of t, or "unknown(N)".
func (t Tag) Name() string {
	if int(t) < len(tagNames) && tagNames[t] != "" {
		return tagNames[t]
	}
	return fmt.Sprintf("unknown(%d)", t)
}

var tagNames = [TagFirstVirtual]string{
	TagVote:                        "vote",
	TagComment:                     "comment",
	TagTransfer:                    "transfer",
	TagTransferToVesting:           "transfer_to_vesting",
	TagWithdrawVesting:             "withdraw_vesting",
	TagLimitOrderCreate:            "limit_order_create",
	TagLimitOrderCancel:            "limit_order_cancel",
	TagFeedPublish:                 "feed_publish",
	TagConvert:                     "convert",
	TagAccountCreate:               "account_create",
	TagAccountUpdate:               "account_update",
	TagWitnessUpdate:               "witness_update",
	TagAccountWitnessVote:          "account_witness_vote",
	TagAccountWitnessProxy:         "account_witness_proxy",
	TagPow:                         "pow",
	TagCustom:                      "custom",
	TagWitnessBlockApprove:         "witness_block_approve",
	TagDeleteComment:               "delete_comment",
	TagCustomJSON:                  "custom_json",
	TagCommentOptions:              "comment_options",
	TagSetWithdrawVestingRoute:     "set_withdraw_vesting_route",
	TagLimitOrderCreate2:           "limit_order_create2",
	TagClaimAccount:                "claim_account",
	TagCreateClaimedAccount:        "create_claimed_account",
	TagRequestAccountRecovery:      "request_account_recovery",
	TagRecoverAccount:              "recover_account",
	TagChangeRecoveryAccount:       "change_recovery_account",
	TagEscrowTransfer:              "escrow_transfer",
	TagEscrowDispute:               "escrow_dispute",
	TagEscrowRelease:               "escrow_release",
	TagPow2:                        "pow2",
	TagEscrowApprove:               "escrow_approve",
	TagTransferToSavings:           "transfer_to_savings",
	TagTransferFromSavings:         "transfer_from_savings",
	TagCancelTransferFromSavings:   "cancel_transfer_from_savings",
	TagCustomBinary:                "custom_binary",
	TagDeclineVotingRights:         "decline_voting_rights",
	TagResetAccount:                "reset_account",
	TagSetResetAccount:             "set_reset_account",
	TagClaimRewardBalance:          "claim_reward_balance",
	TagDelegateVestingShares:       "delegate_vesting_shares",
	TagAccountCreateWithDelegation: "account_create_with_delegation",
	TagWitnessSetProperties:        "witness_set_properties",
	TagAccountUpdate2:              "account_update2",
	TagCreateProposal:              "create_proposal",
	TagUpdateProposalVotes:         "update_proposal_votes",
	TagRemoveProposal:              "remove_proposal",
	TagUpdateProposal:              "update_proposal",
	TagCollateralizedConvert:       "collateralized_convert",
	TagRecurrentTransfer:           "recurrent_transfer",
}
