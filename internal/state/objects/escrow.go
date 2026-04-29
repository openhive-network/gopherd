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
	PendingFee           int64 // HIVE only
	RatificationDeadline uint32
	EscrowExpiration     uint32
	ToApproved           bool
	AgentApproved        bool
	Disputed             bool
}

const EscrowTable state.TableID = 10

func escrowKey(from auth.AccountName, escrowID uint32) []byte {
	out := state.AppendAccountName(nil, from)
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
	if !r.AtEnd() {
		return e, errors.New("escrow: trailing bytes")
	}
	return e, nil
}

func PutEscrow(s *state.Session, e *Escrow) {
	s.Put(EscrowTable, state.PrimaryIndex, escrowKey(e.From, e.EscrowID), AppendEscrow(nil, e))
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
