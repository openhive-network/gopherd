package chain

import (
	"gopherd/internal/protocol/asset"
	"gopherd/internal/protocol/op"
	"gopherd/internal/state"
	"gopherd/internal/state/objects"
)

// processSavingsWithdraws mirrors database::process_savings_withdraws
// (libraries/chain/database_market.cpp:134). For every savings_withdraw
// row whose `complete` time has passed, credit the recipient's matching
// liquid balance and remove the row (and the by-complete index entry).
// Iteration order is by-complete time, so we stop at the first
// not-yet-mature row.
func processSavingsWithdraws(s *state.Session, blockTime uint32, d *Database) {
	if hint, ok := s.GetExpHint(objects.SavingsWithdrawTable, objects.SavingsWithdrawByCompleteIndex); ok && hint > blockTime {
		return
	}

	type pending struct {
		complete  uint32
		from      string
		requestID uint32
	}
	var ready []pending
	var nextExp uint32

	_ = s.RangeFrom(objects.SavingsWithdrawTable, objects.SavingsWithdrawByCompleteIndex, expHintLowerBound(s, objects.SavingsWithdrawTable, objects.SavingsWithdrawByCompleteIndex),
		func(body, _ []byte) (bool, error) {
			if len(body) < 4+state.AccountNameKeyLen+4 {
				return true, nil
			}
			c := uint32(body[0])<<24 | uint32(body[1])<<16 | uint32(body[2])<<8 | uint32(body[3])
			if c > blockTime {
				nextExp = c
				return false, nil
			}
			fromBytes := body[4 : 4+state.AccountNameKeyLen]
			n := state.AccountNameKeyLen
			for n > 0 && fromBytes[n-1] == 0 {
				n--
			}
			off := 4 + state.AccountNameKeyLen
			rid := uint32(body[off])<<24 | uint32(body[off+1])<<16 | uint32(body[off+2])<<8 | uint32(body[off+3])
			ready = append(ready, pending{complete: c, from: string(fromBytes[:n]), requestID: rid})
			return true, nil
		})

	for _, p := range ready {
		w, err := objects.GetSavingsWithdraw(s, p.from, p.requestID)
		if err != nil {
			continue
		}
		to, gerr := objects.GetAccount(s, w.To)
		if gerr == nil {
			switch asset.AssetNum(w.Symbol) {
			case asset.HiveSymbol:
				to.Balance += w.Amount
			case asset.HbdSymbol:
				objects.AdjustHBDBalance(s, &to, blockTime, w.Amount)
			}
			objects.PutAccount(s, &to)
		}
		if d != nil {
			d.pushVOp(op.FillTransferFromSavings{
				From:      w.From,
				To:        w.To,
				Amount:    asset.Asset{Amount: w.Amount, Symbol: asset.Symbol{Num: asset.AssetNum(w.Symbol)}},
				RequestID: w.RequestID,
				Memo:      w.Memo,
			})
		}
		objects.DeleteSavingsWithdrawFull(s, &w)
	}
	if nextExp == 0 {
		nextExp = ^uint32(0)
	}
	s.SetExpHint(objects.SavingsWithdrawTable, objects.SavingsWithdrawByCompleteIndex, nextExp)
}
