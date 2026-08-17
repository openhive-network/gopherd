package objects

import (
	"errors"

	"gopherd/internal/fcraw"
	"gopherd/internal/protocol/auth"
	"gopherd/internal/state"
)

// RecurrentTransfer is the recurrent_transfer_object — schedules
// repeated automated transfers from `From` to `To`.
type RecurrentTransfer struct {
	From                auth.AccountName
	To                  auth.AccountName
	PairID              uint8
	Amount              int64
	Symbol              uint32
	Memo                string
	Recurrence          uint16 // hours
	Executions          uint16 // remaining count
	Trigger             uint32 // next firing time_point_sec
	ConsecutiveFailures uint8
}

const RecurrentTransferTable state.TableID = 13

func recurrentKey(from, to auth.AccountName, pairID uint8) []byte {
	out := state.AppendAccountName(nil, from)
	out = state.AppendAccountName(out, to)
	return append(out, pairID)
}

func AppendRecurrentTransfer(dst []byte, r *RecurrentTransfer) []byte {
	dst = fcraw.AppendVarUint(dst, uint32(len(r.From)))
	dst = append(dst, r.From...)
	dst = fcraw.AppendVarUint(dst, uint32(len(r.To)))
	dst = append(dst, r.To...)
	dst = append(dst, r.PairID)
	dst = appendI64LE(dst, r.Amount)
	dst = appendU32LE(dst, r.Symbol)
	dst = fcraw.AppendVarUint(dst, uint32(len(r.Memo)))
	dst = append(dst, r.Memo...)
	dst = append(dst, byte(r.Recurrence), byte(r.Recurrence>>8))
	dst = append(dst, byte(r.Executions), byte(r.Executions>>8))
	dst = appendU32LE(dst, r.Trigger)
	dst = append(dst, r.ConsecutiveFailures)
	return dst
}

func ReadRecurrentTransfer(src []byte) (RecurrentTransfer, error) {
	r := fcraw.NewReader(src)
	var rt RecurrentTransfer
	from, err := r.ReadString()
	if err != nil {
		return rt, err
	}
	rt.From = from
	if rt.To, err = r.ReadString(); err != nil {
		return rt, err
	}
	if rt.PairID, err = r.ReadUint8(); err != nil {
		return rt, err
	}
	if rt.Amount, err = r.ReadInt64(); err != nil {
		return rt, err
	}
	if rt.Symbol, err = r.ReadUint32(); err != nil {
		return rt, err
	}
	if rt.Memo, err = r.ReadString(); err != nil {
		return rt, err
	}
	if rt.Recurrence, err = r.ReadUint16(); err != nil {
		return rt, err
	}
	if rt.Executions, err = r.ReadUint16(); err != nil {
		return rt, err
	}
	if rt.Trigger, err = r.ReadUint32(); err != nil {
		return rt, err
	}
	if rt.ConsecutiveFailures, err = r.ReadUint8(); err != nil {
		return rt, err
	}
	if !r.AtEnd() {
		return rt, errors.New("recurrent_transfer: trailing bytes")
	}
	return rt, nil
}

func PutRecurrentTransfer(s *state.Session, r *RecurrentTransfer) {
	s.Put(RecurrentTransferTable, state.PrimaryIndex, recurrentKey(r.From, r.To, r.PairID), AppendRecurrentTransfer(nil, r))
}

func DeleteRecurrentTransfer(s *state.Session, from, to auth.AccountName, pairID uint8) {
	s.Delete(RecurrentTransferTable, state.PrimaryIndex, recurrentKey(from, to, pairID))
}

func GetRecurrentTransfer(s *state.Session, from, to auth.AccountName, pairID uint8) (RecurrentTransfer, error) {
	v, err := s.Get(RecurrentTransferTable, state.PrimaryIndex, recurrentKey(from, to, pairID))
	if err != nil {
		return RecurrentTransfer{}, err
	}
	return ReadRecurrentTransfer(v)
}

func HasRecurrentTransfer(s *state.Session, from, to auth.AccountName, pairID uint8) (bool, error) {
	return s.Has(RecurrentTransferTable, state.PrimaryIndex, recurrentKey(from, to, pairID))
}
