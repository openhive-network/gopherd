package chain

import (
	"hived-go/internal/protocol/asset"
	"hived-go/internal/protocol/hardfork"
	"hived-go/internal/state"
	"hived-go/internal/state/objects"
)

// processRecurrentTransfers mirrors database::process_recurrent_transfers
// (libraries/chain/database.cpp:2351), which fires once per HIVE_BLOCK_INTERVAL
// (every block) and dispatches due recurrent_transfer rows. Each fire:
//   - debit `from` by Amount
//   - credit `to` by Amount
//   - decrement Executions
//   - if Executions reached zero, remove the row
//   - else advance Trigger by Recurrence hours
//
// If the sender's balance is insufficient, hived increments
// consecutive_failures and skips this firing. After a configurable
// threshold of consecutive failures, the schedule is removed.
//
// Pre-HF25 the recurrent_transfer feature is unused (rows can't be
// created), so this hook is a no-op outside that range.
func processRecurrentTransfers(s *state.Session, blockTime, blockNum uint32) {
	hf := hardfork.SetForBlock(blockNum)
	if !hf.Has(hardfork.HF25) {
		return
	}

	type fire struct {
		from, to string
		pairID   uint8
	}
	var fires []fire

	_ = s.Range(objects.RecurrentTransferTable, state.PrimaryIndex, nil,
		func(_, value []byte) (bool, error) {
			rt, err := objects.ReadRecurrentTransfer(value)
			if err != nil {
				return true, nil
			}
			if rt.Trigger > blockTime {
				return true, nil
			}
			fires = append(fires, fire{from: rt.From, to: rt.To, pairID: rt.PairID})
			return true, nil
		})

	const maxConsecutiveFailures uint8 = 10
	for _, f := range fires {
		rt, err := objects.GetRecurrentTransfer(s, f.from, f.to, f.pairID)
		if err != nil {
			continue
		}
		from, ferr := objects.GetAccount(s, f.from)
		if ferr != nil {
			objects.DeleteRecurrentTransfer(s, f.from, f.to, f.pairID)
			continue
		}
		to, terr := objects.GetAccount(s, f.to)
		if terr != nil {
			objects.DeleteRecurrentTransfer(s, f.from, f.to, f.pairID)
			continue
		}

		// Try to fire — sender must have balance.
		var sufficient bool
		switch asset.AssetNum(rt.Symbol) {
		case asset.HiveSymbol:
			if from.Balance >= rt.Amount {
				from.Balance -= rt.Amount
				to.Balance += rt.Amount
				sufficient = true
			}
		case asset.HbdSymbol:
			if from.HBDBalance >= rt.Amount {
				objects.AdjustHBDBalance(s, &from, blockTime, -rt.Amount)
				objects.AdjustHBDBalance(s, &to, blockTime, rt.Amount)
				sufficient = true
			}
		}

		if sufficient {
			objects.PutAccount(s, &from)
			objects.PutAccount(s, &to)
			rt.ConsecutiveFailures = 0
			rt.Executions--
			if rt.Executions == 0 {
				objects.DeleteRecurrentTransfer(s, f.from, f.to, f.pairID)
				continue
			}
			rt.Trigger += uint32(rt.Recurrence) * 3600
			objects.PutRecurrentTransfer(s, &rt)
		} else {
			rt.ConsecutiveFailures++
			if rt.ConsecutiveFailures >= maxConsecutiveFailures {
				objects.DeleteRecurrentTransfer(s, f.from, f.to, f.pairID)
				continue
			}
			// Advance trigger so we don't retry every block on the same row.
			rt.Trigger += uint32(rt.Recurrence) * 3600
			objects.PutRecurrentTransfer(s, &rt)
		}
	}
}
