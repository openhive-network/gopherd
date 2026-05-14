package chain

import (
	"sort"

	"hived-go/internal/protocol/asset"
	"hived-go/internal/protocol/hardfork"
	"hived-go/internal/state"
	"hived-go/internal/state/objects"
)

// maxRecurrentTransfersPerBlock mirrors hived's
// HIVE_MAX_RECURRENT_TRANSFERS_PER_BLOCK (1000). When more than this
// many recurrent_transfer rows are due in a single block,
// process_recurrent_transfers stops after the first 1000 (sorted by
// trigger_date, then by from/to/pair_id for deterministic tie-break)
// and the rest carry over to the next block.
const maxRecurrentTransfersPerBlock = 1000

// processRecurrentTransfers mirrors database::process_recurrent_transfers
// (libraries/chain/database.cpp:2565), which fires once per HIVE_BLOCK_INTERVAL
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
//
// The `d` parameter is the chain.Database — passed in so we can push
// fill_recurrent_transfer / failed_recurrent_transfer virtual ops onto
// the per-block buffer (block_vops.go). May be nil in test contexts
// that bypass the vop buffer.
func processRecurrentTransfers(s *state.Session, blockTime, blockNum uint32, d *Database) {
	hf := hardfork.SetForBlock(blockNum)
	if !hf.Has(hardfork.HF25) {
		return
	}

	type fire struct {
		from, to string
		pairID   uint8
		trigger  uint32
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
			fires = append(fires, fire{from: rt.From, to: rt.To, pairID: rt.PairID, trigger: rt.Trigger})
			return true, nil
		})

	// Sort by trigger_date then by (from, to, pair_id) for deterministic
	// tie-break. Mirrors hived's by_trigger_date index ordering. Without
	// this the 1000-per-block cap would pick a non-deterministic subset
	// from Range's iteration order (which is keyed by primary index =
	// from/to/pair_id, not trigger).
	sort.Slice(fires, func(i, j int) bool {
		if fires[i].trigger != fires[j].trigger {
			return fires[i].trigger < fires[j].trigger
		}
		if fires[i].from != fires[j].from {
			return fires[i].from < fires[j].from
		}
		if fires[i].to != fires[j].to {
			return fires[i].to < fires[j].to
		}
		return fires[i].pairID < fires[j].pairID
	})

	// Cap at HIVE_MAX_RECURRENT_TRANSFERS_PER_BLOCK; the excess defers
	// to the next block since their trigger is already <= blockTime.
	if len(fires) > maxRecurrentTransfersPerBlock {
		fires = fires[:maxRecurrentTransfersPerBlock]
	}

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

		amt := asset.Asset{Amount: rt.Amount, Symbol: asset.Symbol{Num: asset.AssetNum(rt.Symbol)}}

		if sufficient {
			objects.PutAccount(s, &from)
			objects.PutAccount(s, &to)
			rt.ConsecutiveFailures = 0
			// hived computes `remaining_executions = current.remaining_executions - 1`
			// BEFORE deciding whether to remove the row, and emits the
			// vop with that pre-removal value.
			remaining := rt.Executions - 1
			if remaining == 0 {
				objects.DeleteRecurrentTransfer(s, f.from, f.to, f.pairID)
			} else {
				rt.Executions = remaining
				rt.Trigger += uint32(rt.Recurrence) * 3600
				objects.PutRecurrentTransfer(s, &rt)
			}
			if d != nil {
				d.pushVOp(BlockVOp{
					Kind:                BlockVOpFillRecurrentTransfer,
					From:                rt.From,
					To:                  rt.To,
					Amount:              amt,
					Memo:                rt.Memo,
					RemainingExecutions: remaining,
					PairID:              f.pairID,
				})
			}
		} else {
			// hived (database.cpp:2622-2649): consecutive_failures is
			// the value WRITTEN back (current+1). If consecutive_failures
			// < max → keep schedule, decrement remaining_executions; if
			// reaching the max OR remaining == 0 after decrement → delete.
			newCF := rt.ConsecutiveFailures + 1
			remaining := rt.Executions - 1
			deleted := false
			if newCF < maxConsecutiveFailures {
				if remaining == 0 {
					objects.DeleteRecurrentTransfer(s, f.from, f.to, f.pairID)
					deleted = true
				} else {
					rt.ConsecutiveFailures = newCF
					rt.Executions = remaining
					rt.Trigger += uint32(rt.Recurrence) * 3600
					objects.PutRecurrentTransfer(s, &rt)
				}
			} else {
				objects.DeleteRecurrentTransfer(s, f.from, f.to, f.pairID)
				deleted = true
			}
			if d != nil {
				d.pushVOp(BlockVOp{
					Kind:                BlockVOpFailedRecurrentTransfer,
					From:                rt.From,
					To:                  rt.To,
					Amount:              amt,
					Memo:                rt.Memo,
					RemainingExecutions: remaining,
					ConsecutiveFailures: newCF,
					Deleted:             deleted,
					PairID:              f.pairID,
				})
			}
		}
	}
}
