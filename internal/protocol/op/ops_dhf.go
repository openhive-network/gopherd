package op

import (
	"encoding/binary"
	"fmt"

	"hived-go/internal/fcraw"
	"hived-go/internal/protocol/asset"
	"hived-go/internal/protocol/auth"
)

// readSortedInt64Set reads a flat_set_ex<int64_t> on the wire: varint count
// + each int64 LE in sorted order.
func readSortedInt64Set(r *fcraw.Reader, label string) ([]int64, error) {
	count, err := r.ReadVarUint()
	if err != nil {
		return nil, fmt.Errorf("%s count: %w", label, err)
	}
	out := make([]int64, 0, count)
	for i := uint32(0); i < count; i++ {
		v, err := r.ReadInt64()
		if err != nil {
			return nil, fmt.Errorf("%s[%d]: %w", label, i, err)
		}
		out = append(out, v)
	}
	return out, nil
}

func appendSortedInt64Set(dst []byte, ids []int64) []byte {
	dst = fcraw.AppendVarUint(dst, uint32(len(ids)))
	for _, id := range ids {
		dst = binary.LittleEndian.AppendUint64(dst, uint64(id))
	}
	return dst
}

// ---- 44 create_proposal -------------------------------------------------

type CreateProposal struct {
	Creator    auth.AccountName
	Receiver   auth.AccountName
	StartDate  uint32
	EndDate    uint32
	DailyPay   asset.Asset
	Subject    string
	Permlink   string
	Extensions []byte
}

func (CreateProposal) OpTag() Tag { return TagCreateProposal }
func (o CreateProposal) AppendFCBody(dst []byte) []byte {
	dst = appendString(dst, o.Creator)
	dst = appendString(dst, o.Receiver)
	dst = binary.LittleEndian.AppendUint32(dst, o.StartDate)
	dst = binary.LittleEndian.AppendUint32(dst, o.EndDate)
	dst = asset.AppendAsset(dst, o.DailyPay)
	dst = appendString(dst, o.Subject)
	dst = appendString(dst, o.Permlink)
	return appendRawExtensions(dst, o.Extensions)
}

func readCreateProposal(r *fcraw.Reader) (Operation, error) {
	var o CreateProposal
	var err error
	if o.Creator, err = r.ReadString(); err != nil {
		return nil, err
	}
	if o.Receiver, err = r.ReadString(); err != nil {
		return nil, err
	}
	if o.StartDate, err = r.ReadUint32(); err != nil {
		return nil, err
	}
	if o.EndDate, err = r.ReadUint32(); err != nil {
		return nil, err
	}
	if o.DailyPay, err = asset.ReadAsset(r); err != nil {
		return nil, err
	}
	if o.Subject, err = r.ReadString(); err != nil {
		return nil, err
	}
	if o.Permlink, err = r.ReadString(); err != nil {
		return nil, err
	}
	if o.Extensions, err = readExtensionsRaw(r, "create_proposal.extensions", futureExtensionsTags); err != nil {
		return nil, err
	}
	return o, nil
}

// ---- 45 update_proposal_votes -------------------------------------------

type UpdateProposalVotes struct {
	Voter       auth.AccountName
	ProposalIDs []int64
	Approve     bool
	Extensions  []byte
}

func (UpdateProposalVotes) OpTag() Tag { return TagUpdateProposalVotes }
func (o UpdateProposalVotes) AppendFCBody(dst []byte) []byte {
	dst = appendString(dst, o.Voter)
	dst = appendSortedInt64Set(dst, o.ProposalIDs)
	if o.Approve {
		dst = append(dst, 1)
	} else {
		dst = append(dst, 0)
	}
	return appendRawExtensions(dst, o.Extensions)
}

func readUpdateProposalVotes(r *fcraw.Reader) (Operation, error) {
	var o UpdateProposalVotes
	var err error
	if o.Voter, err = r.ReadString(); err != nil {
		return nil, err
	}
	if o.ProposalIDs, err = readSortedInt64Set(r, "update_proposal_votes.proposal_ids"); err != nil {
		return nil, err
	}
	if o.Approve, err = r.ReadBool(); err != nil {
		return nil, err
	}
	if o.Extensions, err = readExtensionsRaw(r, "update_proposal_votes.extensions", futureExtensionsTags); err != nil {
		return nil, err
	}
	return o, nil
}

// ---- 46 remove_proposal -------------------------------------------------

type RemoveProposal struct {
	ProposalOwner auth.AccountName
	ProposalIDs   []int64
	Extensions    []byte
}

func (RemoveProposal) OpTag() Tag { return TagRemoveProposal }
func (o RemoveProposal) AppendFCBody(dst []byte) []byte {
	dst = appendString(dst, o.ProposalOwner)
	dst = appendSortedInt64Set(dst, o.ProposalIDs)
	return appendRawExtensions(dst, o.Extensions)
}

func readRemoveProposal(r *fcraw.Reader) (Operation, error) {
	var o RemoveProposal
	var err error
	if o.ProposalOwner, err = r.ReadString(); err != nil {
		return nil, err
	}
	if o.ProposalIDs, err = readSortedInt64Set(r, "remove_proposal.proposal_ids"); err != nil {
		return nil, err
	}
	if o.Extensions, err = readExtensionsRaw(r, "remove_proposal.extensions", futureExtensionsTags); err != nil {
		return nil, err
	}
	return o, nil
}

// ---- 47 update_proposal -------------------------------------------------
//
// extensions has variants:
//
//	tag 0: void_t                       — 0 bytes
//	tag 1: update_proposal_end_date     — time_point_sec (4 bytes)
var updateProposalExtTags = [][2]int{{0, 0}, {1, 4}}

type UpdateProposal struct {
	ProposalID int64
	Creator    auth.AccountName
	DailyPay   asset.Asset
	Subject    string
	Permlink   string
	Extensions []byte
}

func (UpdateProposal) OpTag() Tag { return TagUpdateProposal }
func (o UpdateProposal) AppendFCBody(dst []byte) []byte {
	dst = binary.LittleEndian.AppendUint64(dst, uint64(o.ProposalID))
	dst = appendString(dst, o.Creator)
	dst = asset.AppendAsset(dst, o.DailyPay)
	dst = appendString(dst, o.Subject)
	dst = appendString(dst, o.Permlink)
	return appendRawExtensions(dst, o.Extensions)
}

func readUpdateProposal(r *fcraw.Reader) (Operation, error) {
	var o UpdateProposal
	var err error
	if o.ProposalID, err = r.ReadInt64(); err != nil {
		return nil, err
	}
	if o.Creator, err = r.ReadString(); err != nil {
		return nil, err
	}
	if o.DailyPay, err = asset.ReadAsset(r); err != nil {
		return nil, err
	}
	if o.Subject, err = r.ReadString(); err != nil {
		return nil, err
	}
	if o.Permlink, err = r.ReadString(); err != nil {
		return nil, err
	}
	if o.Extensions, err = readExtensionsRaw(r, "update_proposal.extensions", updateProposalExtTags); err != nil {
		return nil, err
	}
	return o, nil
}

func init() {
	register(TagCreateProposal, readCreateProposal)
	register(TagUpdateProposalVotes, readUpdateProposalVotes)
	register(TagRemoveProposal, readRemoveProposal)
	register(TagUpdateProposal, readUpdateProposal)
}
