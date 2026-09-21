package objects

import (
	"errors"

	"gopherd/internal/fcraw"
	"gopherd/internal/protocol/auth"
	"gopherd/internal/state"
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
	// Removed mirrors proposal_object::removed. A remove_proposal_operation
	// that runs out of its per-operation removal budget cannot erase every
	// proposal it names; the ones left over are FLAGGED instead, with
	// end_date pulled back so they stop being funded, and the regular
	// per-block sweep (RemoveOldProposals) finishes the job later.
	// dhf_evaluator.cpp:255 — the flag is what HF29's
	// VALIDATE_ALL_PROPOSAL_IDS treats as "doesn't exist".
	Removed bool
}

// ProposalMaintenanceCleanupSec is HIVE_PROPOSAL_MAINTENANCE_CLEANUP — one
// day. proposal_object::get_end_date_with_delay() adds it to end_date, and
// that DELAYED date is what the by_end_date index orders on: a proposal stays
// "alive" for a day past its end so the corner cases around expiry have room.
const ProposalMaintenanceCleanupSec uint32 = 60 * 60 * 24

// EndDateWithDelay mirrors proposal_object::get_end_date_with_delay().
func (p *Proposal) EndDateWithDelay() uint32 {
	return p.EndDate + ProposalMaintenanceCleanupSec
}

// ProposalVote (proposalID, voter) edge.
type ProposalVote struct {
	ProposalID int64
	Voter      auth.AccountName
}

const (
	ProposalTable     state.TableID = 16
	ProposalVoteTable state.TableID = 17
	// ProposalByEndDateIndex orders proposals by end_date. The
	// per-block process_proposals hook iterates this index to retire
	// any proposal whose end_date has passed.
	ProposalByEndDateIndex state.IndexID = 1
	// ProposalRemovedIndex holds exactly the proposals whose Removed flag is
	// set, keyed the same way as ProposalByEndDateIndex so iteration order is
	// identical. hived has no such index: dhf_processor::remove_proposals
	// walks by_end_date and skips every proposal that is not flagged. That
	// walk is O(all expired proposals) on EVERY block, which hived can afford
	// with an in-memory multi_index and gopherd cannot afford against the
	// state layer. Restricting the scan to the flagged rows visits the same
	// proposals in the same order and skips only the no-ops.
	ProposalRemovedIndex state.IndexID = 2
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
	// Appended LAST, and read back only if there are bytes left, so rows
	// written before the field existed still decode (as Removed=false, which
	// is what they were: nothing could flag a proposal before this).
	if p.Removed {
		dst = append(dst, 1)
	} else {
		dst = append(dst, 0)
	}
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
		var flag uint8
		if flag, err = r.ReadUint8(); err != nil {
			return p, err
		}
		p.Removed = flag != 0
	}
	if !r.AtEnd() {
		return p, errors.New("proposal: trailing bytes")
	}
	return p, nil
}

// proposalByEndDateKey keys on the RAW end_date, where hived's by_end_date
// composite key uses get_end_date_with_delay() (end_date + one day). Adding a
// constant to every key would reorder nothing, and it would change an
// encoding that snapshots on disk already use — so the delay is applied where
// it actually matters, at the boundary comparison in the sweep, not here.
func proposalByEndDateKey(endDate uint32, id int64) []byte {
	out := state.AppendUint32BE(nil, endDate)
	return state.AppendInt64BE(out, id)
}

// ProposalIDFromEndDateKey decodes a key produced by proposalByEndDateKey:
// 4 bytes of big-endian end_date followed by the sign-flipped big-endian id.
func ProposalIDFromEndDateKey(key []byte) (endDate uint32, id int64, ok bool) {
	if len(key) < 12 {
		return 0, 0, false
	}
	for i := 0; i < 4; i++ {
		endDate = endDate<<8 | uint32(key[i])
	}
	var u uint64
	for i := 4; i < 12; i++ {
		u = u<<8 | uint64(key[i])
	}
	return endDate, int64(u ^ (1 << 63)), true
}

// PutProposal stores p AND maintains the by-end-date secondary index.
// Callers updating an existing proposal's EndDate (uncommon) must
// DeleteProposalByEndDate at the OLD endDate first.
func PutProposal(s *state.Session, p *Proposal) {
	s.Put(ProposalTable, state.PrimaryIndex, proposalKey(p.ID), AppendProposal(nil, p))
	s.Put(ProposalTable, ProposalByEndDateIndex, proposalByEndDateKey(p.EndDate, p.ID), nil)
	if p.Removed {
		s.Put(ProposalTable, ProposalRemovedIndex, proposalByEndDateKey(p.EndDate, p.ID), nil)
	}
}

// ReplaceProposal stores next after dropping the secondary-index entries that
// belong to prev. Use it for any update that moves EndDate or sets Removed;
// PutProposal alone would leave the old index entry behind, pointing at a
// proposal that no longer sorts there.
func ReplaceProposal(s *state.Session, prev, next *Proposal) {
	s.Delete(ProposalTable, ProposalByEndDateIndex, proposalByEndDateKey(prev.EndDate, prev.ID))
	if prev.Removed {
		s.Delete(ProposalTable, ProposalRemovedIndex, proposalByEndDateKey(prev.EndDate, prev.ID))
	}
	PutProposal(s, next)
}

// DeleteProposalFull removes both the primary row and the by-end-date
// index entry. Use this when a proposal is removed or expired-out.
func DeleteProposalFull(s *state.Session, p *Proposal) {
	s.Delete(ProposalTable, state.PrimaryIndex, proposalKey(p.ID))
	s.Delete(ProposalTable, ProposalByEndDateIndex, proposalByEndDateKey(p.EndDate, p.ID))
	s.Delete(ProposalTable, ProposalRemovedIndex, proposalByEndDateKey(p.EndDate, p.ID))
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

// DeleteAllProposalVotesForProposal removes every ProposalVote row attached
// to `id`, with no removal budget. Kept as the unbudgeted form of
// RemoveProposalVotesForProposal for callers that are not subject to
// HIVE_GLOBAL_REMOVE_THRESHOLD.
func DeleteAllProposalVotesForProposal(s *state.Session, id int64) {
	RemoveProposalVotesForProposal(s, id, NewRemoveGuard(-1))
}
