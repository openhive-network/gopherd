package objects

import (
	"errors"
	"fmt"

	"hived-go/internal/fcraw"
	"hived-go/internal/protocol/auth"
	"hived-go/internal/state"
)

// SavingsWithdraw is the chain-level savings_withdraw_object — a pending
// transfer from the savings balance back to a regular account, with a
// HIVE_SAVINGS_WITHDRAW_TIME (3 days) delay.
type SavingsWithdraw struct {
	From      auth.AccountName
	RequestID uint32
	To        auth.AccountName
	Amount    int64
	Symbol    uint32 // HiveSymbol or HbdSymbol
	Memo      string
	Complete  uint32 // time_point_sec when funds become available
}

const SavingsWithdrawTable state.TableID = 7

// SavingsWithdrawByCompleteIndex orders entries by complete time so the
// per-block process_savings_withdraws can iterate matures in time order.
// Key: [complete_BE_4][from_padded_16][request_id_BE_4].
const SavingsWithdrawByCompleteIndex state.IndexID = 1

func savingsWithdrawKey(from auth.AccountName, requestID uint32) []byte {
	out := state.AppendAccountName(nil, from)
	return state.AppendUint32BE(out, requestID)
}

func savingsWithdrawByCompleteKey(complete uint32, from auth.AccountName, requestID uint32) []byte {
	out := state.AppendUint32BE(nil, complete)
	out = state.AppendAccountName(out, from)
	return state.AppendUint32BE(out, requestID)
}

func AppendSavingsWithdraw(dst []byte, w *SavingsWithdraw) []byte {
	dst = fcraw.AppendVarUint(dst, uint32(len(w.From)))
	dst = append(dst, w.From...)
	dst = appendU32LE(dst, w.RequestID)
	dst = fcraw.AppendVarUint(dst, uint32(len(w.To)))
	dst = append(dst, w.To...)
	dst = appendI64LE(dst, w.Amount)
	dst = appendU32LE(dst, w.Symbol)
	dst = fcraw.AppendVarUint(dst, uint32(len(w.Memo)))
	dst = append(dst, w.Memo...)
	dst = appendU32LE(dst, w.Complete)
	return dst
}

func ReadSavingsWithdraw(src []byte) (SavingsWithdraw, error) {
	r := fcraw.NewReader(src)
	var w SavingsWithdraw
	from, err := r.ReadString()
	if err != nil {
		return w, fmt.Errorf("savings_withdraw.from: %w", err)
	}
	w.From = from
	if w.RequestID, err = r.ReadUint32(); err != nil {
		return w, err
	}
	if w.To, err = r.ReadString(); err != nil {
		return w, err
	}
	if w.Amount, err = r.ReadInt64(); err != nil {
		return w, err
	}
	if w.Symbol, err = r.ReadUint32(); err != nil {
		return w, err
	}
	if w.Memo, err = r.ReadString(); err != nil {
		return w, err
	}
	if w.Complete, err = r.ReadUint32(); err != nil {
		return w, err
	}
	if !r.AtEnd() {
		return w, errors.New("savings_withdraw: trailing bytes")
	}
	return w, nil
}

func PutSavingsWithdraw(s *state.Session, w *SavingsWithdraw) {
	s.Put(SavingsWithdrawTable, state.PrimaryIndex, savingsWithdrawKey(w.From, w.RequestID), AppendSavingsWithdraw(nil, w))
	s.Put(SavingsWithdrawTable, SavingsWithdrawByCompleteIndex, savingsWithdrawByCompleteKey(w.Complete, w.From, w.RequestID), nil)
	if w.Complete != 0 {
		s.NudgeExpHint(SavingsWithdrawTable, SavingsWithdrawByCompleteIndex, w.Complete)
	}
}

func GetSavingsWithdraw(s *state.Session, from auth.AccountName, requestID uint32) (SavingsWithdraw, error) {
	v, err := s.Get(SavingsWithdrawTable, state.PrimaryIndex, savingsWithdrawKey(from, requestID))
	if err != nil {
		return SavingsWithdraw{}, err
	}
	return ReadSavingsWithdraw(v)
}

func HasSavingsWithdraw(s *state.Session, from auth.AccountName, requestID uint32) (bool, error) {
	return s.Has(SavingsWithdrawTable, state.PrimaryIndex, savingsWithdrawKey(from, requestID))
}

func DeleteSavingsWithdraw(s *state.Session, from auth.AccountName, requestID uint32) {
	s.Delete(SavingsWithdrawTable, state.PrimaryIndex, savingsWithdrawKey(from, requestID))
}

// DeleteSavingsWithdrawFull removes the primary entry AND the secondary
// by-complete entry. Use this when you have the full row available.
func DeleteSavingsWithdrawFull(s *state.Session, w *SavingsWithdraw) {
	s.Delete(SavingsWithdrawTable, state.PrimaryIndex, savingsWithdrawKey(w.From, w.RequestID))
	s.Delete(SavingsWithdrawTable, SavingsWithdrawByCompleteIndex, savingsWithdrawByCompleteKey(w.Complete, w.From, w.RequestID))
}
