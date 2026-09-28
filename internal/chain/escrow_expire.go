package chain

import (
	"gopherd/internal/protocol/op"
	"gopherd/internal/state"
	"gopherd/internal/state/objects"
)

// expireEscrowRatifications mirrors database::expire_escrow_ratification
// in libraries/chain/database_market.cpp:58. For every escrow whose
// ratification deadline has passed without both to and agent approving,
// refund the from account (HIVE + HBD + fee) and remove the row.
//
// Approved escrows skip the expiration path — they remain pending until
// the escrow_expiration deadline (handled by escrow_dispute /
// escrow_release).
func expireEscrowRatifications(s *state.Session, blockTime uint32, d *Database) {
	if hint, ok := s.GetExpHint(objects.EscrowTable, objects.EscrowByRatificationIndex); ok && hint > blockTime {
		return
	}

	type pending struct {
		deadline uint32
		from     string
		escrowID uint32
	}
	var ready []pending
	var nextExp uint32

	_ = s.Range(objects.EscrowTable, objects.EscrowByRatificationIndex, nil,
		func(body, _ []byte) (bool, error) {
			if len(body) < 4+state.AccountNameKeyLen+4 {
				return true, nil
			}
			d := uint32(body[0])<<24 | uint32(body[1])<<16 | uint32(body[2])<<8 | uint32(body[3])
			if d > blockTime {
				nextExp = d
				return false, nil
			}
			fromBytes := body[4 : 4+state.AccountNameKeyLen]
			n := state.AccountNameKeyLen
			for n > 0 && fromBytes[n-1] == 0 {
				n--
			}
			off := 4 + state.AccountNameKeyLen
			eid := uint32(body[off])<<24 | uint32(body[off+1])<<16 | uint32(body[off+2])<<8 | uint32(body[off+3])
			ready = append(ready, pending{deadline: d, from: string(fromBytes[:n]), escrowID: eid})
			return true, nil
		})

	for _, p := range ready {
		esc, err := objects.GetEscrow(s, p.from, p.escrowID)
		if err != nil {
			continue
		}
		// hived skips approved escrows: they remain pending until the
		// escrow_expiration deadline (which is *separate* from the
		// ratification_deadline and not handled by this hook).
		if esc.ToApproved && esc.AgentApproved {
			continue
		}
		from, ferr := objects.GetAccount(s, esc.From)
		if ferr == nil {
			from.Balance += esc.HiveAmount
			hbdRefund := esc.HBDAmount
			if esc.PendingFeeIsHBD {
				hbdRefund += esc.PendingFee
			} else {
				from.Balance += esc.PendingFee
			}
			objects.AdjustHBDBalance(s, &from, blockTime, hbdRefund)
			objects.PutAccount(s, &from)
		}
		// hived's remove_escrow emits escrow_rejected on this path
		// (database_market.cpp:100). The ratification deadline passing is a
		// rejection by default: nobody approved, so the funds go home.
		d.pushVOp(op.EscrowRejected{
			From:       esc.From,
			To:         esc.To,
			Agent:      esc.Agent,
			EscrowID:   esc.EscrowID,
			HbdAmount:  vopHbd(esc.HBDAmount),
			HiveAmount: vopHive(esc.HiveAmount),
			Fee:        escrowFeeAsset(esc.PendingFee, esc.PendingFeeIsHBD),
		})
		objects.DeleteEscrowFull(s, &esc)
	}
	if nextExp == 0 {
		nextExp = ^uint32(0)
	}
	s.SetExpHint(objects.EscrowTable, objects.EscrowByRatificationIndex, nextExp)
}
