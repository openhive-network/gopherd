package objects

import (
	"errors"

	"hived-go/internal/fcraw"
	"hived-go/internal/protocol/auth"
	"hived-go/internal/state"
)

// Proposal mirrors the DHF proposal_object (HF24+).
type Proposal struct {
	ID         int64
	Creator    auth.AccountName
	Receiver   auth.AccountName
	StartDate  uint32
	EndDate    uint32
	DailyPay   int64 // HBD satoshi
	Subject    string
	Permlink   string
	TotalVotes int64 // sum of voter vesting_shares
}

// ProposalVote (proposalID, voter) edge.
type ProposalVote struct {
	ProposalID int64
	Voter      auth.AccountName
}

const (
	ProposalTable     state.TableID = 16
	ProposalVoteTable state.TableID = 17
)

func proposalKey(id int64) []byte {
	return state.AppendInt64BE(nil, id)
}

func proposalVoteKey(id int64, voter auth.AccountName) []byte {
	out := state.AppendInt64BE(nil, id)
	return state.AppendAccountName(out, voter)
}

func AppendProposal(dst []byte, p *Proposal) []byte {
	dst = appendI64LE(dst, p.ID)
	dst = fcraw.AppendVarUint(dst, uint32(len(p.Creator)))
	dst = append(dst, p.Creator...)
	dst = fcraw.AppendVarUint(dst, uint32(len(p.Receiver)))
	dst = append(dst, p.Receiver...)
	dst = appendU32LE(dst, p.StartDate)
	dst = appendU32LE(dst, p.EndDate)
	dst = appendI64LE(dst, p.DailyPay)
	dst = fcraw.AppendVarUint(dst, uint32(len(p.Subject)))
	dst = append(dst, p.Subject...)
	dst = fcraw.AppendVarUint(dst, uint32(len(p.Permlink)))
	dst = append(dst, p.Permlink...)
	dst = appendI64LE(dst, p.TotalVotes)
	return dst
}

func ReadProposal(src []byte) (Proposal, error) {
	r := fcraw.NewReader(src)
	var p Proposal
	var err error
	if p.ID, err = r.ReadInt64(); err != nil {
		return p, err
	}
	if p.Creator, err = r.ReadString(); err != nil {
		return p, err
	}
	if p.Receiver, err = r.ReadString(); err != nil {
		return p, err
	}
	if p.StartDate, err = r.ReadUint32(); err != nil {
		return p, err
	}
	if p.EndDate, err = r.ReadUint32(); err != nil {
		return p, err
	}
	if p.DailyPay, err = r.ReadInt64(); err != nil {
		return p, err
	}
	if p.Subject, err = r.ReadString(); err != nil {
		return p, err
	}
	if p.Permlink, err = r.ReadString(); err != nil {
		return p, err
	}
	if p.TotalVotes, err = r.ReadInt64(); err != nil {
		return p, err
	}
	if !r.AtEnd() {
		return p, errors.New("proposal: trailing bytes")
	}
	return p, nil
}

func PutProposal(s *state.Session, p *Proposal) {
	s.Put(ProposalTable, state.PrimaryIndex, proposalKey(p.ID), AppendProposal(nil, p))
}

func GetProposal(s *state.Session, id int64) (Proposal, error) {
	v, err := s.Get(ProposalTable, state.PrimaryIndex, proposalKey(id))
	if err != nil {
		return Proposal{}, err
	}
	return ReadProposal(v)
}

func HasProposal(s *state.Session, id int64) (bool, error) {
	return s.Has(ProposalTable, state.PrimaryIndex, proposalKey(id))
}

func DeleteProposal(s *state.Session, id int64) {
	s.Delete(ProposalTable, state.PrimaryIndex, proposalKey(id))
}

func PutProposalVote(s *state.Session, id int64, voter auth.AccountName) {
	s.Put(ProposalVoteTable, state.PrimaryIndex, proposalVoteKey(id, voter), nil)
}

func DeleteProposalVote(s *state.Session, id int64, voter auth.AccountName) {
	s.Delete(ProposalVoteTable, state.PrimaryIndex, proposalVoteKey(id, voter))
}

func HasProposalVote(s *state.Session, id int64, voter auth.AccountName) (bool, error) {
	return s.Has(ProposalVoteTable, state.PrimaryIndex, proposalVoteKey(id, voter))
}
