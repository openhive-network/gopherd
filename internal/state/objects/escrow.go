package objects

import (
	"errors"

	"hived-go/internal/fcraw"
	"hived-go/internal/protocol/auth"
	"hived-go/internal/state"
)

// Escrow is the chain-level escrow_object — a three-party conditional
// transfer pending agent + counterparty approval and possibly dispute.
type Escrow struct {
	From, To, Agent      auth.AccountName
	EscrowID             uint32
	HBDAmount            int64
	HiveAmount           int64
	PendingFee           int64 // amount; symbol selected by PendingFeeIsHBD
	RatificationDeadline uint32
	EscrowExpiration     uint32
	ToApproved           bool
	AgentApproved        bool
	Disputed             bool
	// PendingFeeIsHBD distinguishes the fee's asset symbol. xtar's
	// escrow at block 9,543,257 had a 1-sat HBD fee, previously
	// miscredited to the agent's HIVE balance.
	PendingFeeIsHBD bool
}

const EscrowTable state.TableID = 10

// EscrowByRatificationIndex orders entries by ratification_deadline so
// the per-block expire_escrow_ratification hook can iterate stale,
// not-yet-approved escrows in time order.
const EscrowByRatificationIndex state.IndexID = 1

func escrowKey(from auth.AccountName, escrowID uint32) []byte {
	out := state.AppendAccountName(nil, from)
	return state.AppendUint32BE(out, escrowID)
}

func escrowByRatificationKey(deadline uint32, from auth.AccountName, escrowID uint32) []byte {
	out := state.AppendUint32BE(nil, deadline)
	out = state.AppendAccountName(out, from)
	return state.AppendUint32BE(out, escrowID)
}

func AppendEscrow(dst []byte, e *Escrow) []byte {
	dst = fcraw.AppendVarUint(dst, uint32(len(e.From)))
	dst = append(dst, e.From...)
	dst = fcraw.AppendVarUint(dst, uint32(len(e.To)))
	dst = append(dst, e.To...)
	dst = fcraw.AppendVarUint(dst, uint32(len(e.Agent)))
	dst = append(dst, e.Agent...)
	dst = appendU32LE(dst, e.EscrowID)
	dst = appendI64LE(dst, e.HBDAmount)
	dst = appendI64LE(dst, e.HiveAmount)
	dst = appendI64LE(dst, e.PendingFee)
	dst = appendU32LE(dst, e.RatificationDeadline)
	dst = appendU32LE(dst, e.EscrowExpiration)
	if e.ToApproved {
		dst = append(dst, 1)
	} else {
		dst = append(dst, 0)
	}
	if e.AgentApproved {
		dst = append(dst, 1)
	} else {
		dst = append(dst, 0)
	}
	if e.Disputed {
		dst = append(dst, 1)
	} else {
		dst = append(dst, 0)
	}
	if e.PendingFeeIsHBD {
		dst = append(dst, 1)
	} else {
		dst = append(dst, 0)
	}
	return dst
}

func ReadEscrow(src []byte) (Escrow, error) {
	r := fcraw.NewReader(src)
	var e Escrow
	var err error
	if e.From, err = r.ReadString(); err != nil {
		return e, err
	}
	if e.To, err = r.ReadString(); err != nil {
		return e, err
	}
	if e.Agent, err = r.ReadString(); err != nil {
		return e, err
	}
	if e.EscrowID, err = r.ReadUint32(); err != nil {
		return e, err
	}
	if e.HBDAmount, err = r.ReadInt64(); err != nil {
		return e, err
	}
	if e.HiveAmount, err = r.ReadInt64(); err != nil {
		return e, err
	}
	if e.PendingFee, err = r.ReadInt64(); err != nil {
		return e, err
	}
	if e.RatificationDeadline, err = r.ReadUint32(); err != nil {
		return e, err
	}
	if e.EscrowExpiration, err = r.ReadUint32(); err != nil {
		return e, err
	}
	if e.ToApproved, err = r.ReadBool(); err != nil {
		return e, err
	}
	if e.AgentApproved, err = r.ReadBool(); err != nil {
		return e, err
	}
	if e.Disputed, err = r.ReadBool(); err != nil {
		return e, err
	}
	// PendingFeeIsHBD: optional trailing byte for backwards-compat with
	// older serialized escrows that predate the asset-symbol tracking.
	// Missing byte → defaults to false (HIVE), matching old behavior.
	if !r.AtEnd() {
		if e.PendingFeeIsHBD, err = r.ReadBool(); err != nil {
			return e, err
		}
	}
	if !r.AtEnd() {
		return e, errors.New("escrow: trailing bytes")
	}
	return e, nil
}

func PutEscrow(s *state.Session, e *Escrow) {
	s.Put(EscrowTable, state.PrimaryIndex, escrowKey(e.From, e.EscrowID), AppendEscrow(nil, e))
	s.Put(EscrowTable, EscrowByRatificationIndex, escrowByRatificationKey(e.RatificationDeadline, e.From, e.EscrowID), nil)
	if e.RatificationDeadline != 0 {
		s.NudgeExpHint(EscrowTable, EscrowByRatificationIndex, e.RatificationDeadline)
	}
}

func GetEscrow(s *state.Session, from auth.AccountName, escrowID uint32) (Escrow, error) {
	v, err := s.Get(EscrowTable, state.PrimaryIndex, escrowKey(from, escrowID))
	if err != nil {
		return Escrow{}, err
	}
	return ReadEscrow(v)
}

func HasEscrow(s *state.Session, from auth.AccountName, escrowID uint32) (bool, error) {
	return s.Has(EscrowTable, state.PrimaryIndex, escrowKey(from, escrowID))
}

func DeleteEscrow(s *state.Session, from auth.AccountName, escrowID uint32) {
	s.Delete(EscrowTable, state.PrimaryIndex, escrowKey(from, escrowID))
}

// DeleteEscrowFull removes both primary and by-ratification entries.
func DeleteEscrowFull(s *state.Session, e *Escrow) {
	s.Delete(EscrowTable, state.PrimaryIndex, escrowKey(e.From, e.EscrowID))
	s.Delete(EscrowTable, EscrowByRatificationIndex, escrowByRatificationKey(e.RatificationDeadline, e.From, e.EscrowID))
}
