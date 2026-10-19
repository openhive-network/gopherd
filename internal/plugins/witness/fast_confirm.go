package witness

import (
	"encoding/binary"
	"time"

	"gopherd/internal/chain"
	"gopherd/internal/crypto"
	"gopherd/internal/protocol/auth"
	"gopherd/internal/protocol/block"
	"gopherd/internal/protocol/hardfork"
	"gopherd/internal/protocol/network"
	"gopherd/internal/protocol/op"
	"gopherd/internal/protocol/tx"
	"gopherd/internal/state"
	"gopherd/internal/state/objects"
)

// fastConfirmUpToDate is HIVE_UP_TO_DATE_MARGIN__FAST_CONFIRM: a block older
// than this when it is applied is catch-up, not news, and is not approved.
const fastConfirmUpToDate = 60 * time.Second

// fastConfirmExpiration is HIVE_MAX_TIME_UNTIL_EXPIRATION/2, the lifetime
// hived gives its fast-confirm transactions.
const fastConfirmExpiration = 30 * 60

// BuildFastConfirms ports witness_plugin_impl::on_finish_push_block
// (witness_plugin.cpp:218): for a freshly applied block, sign one
// witness_block_approve per local producer on the irreversibility schedule
// whose registered signing key we hold, except the block's own producer (the
// block is its approval). Returns the signed transactions for the caller to
// apply and broadcast. Call it from the apply loop, once per applied block.
func (p *Plugin) BuildFastConfirms(rawBlock []byte, num uint32, now time.Time) [][]byte {
	if !p.FastConfirm || p.DryRun || p.Chain == nil || len(p.Producers) == 0 || num <= p.lastFastConfirmNum {
		return nil
	}
	sh, hdrLen, err := block.UnmarshalSignedHeader(rawBlock)
	if err != nil || now.Sub(time.Unix(int64(sh.Timestamp), 0)) >= fastConfirmUpToDate {
		return nil
	}
	p.lastFastConfirmNum = num
	id := block.ConstructID(rawBlock[:hdrLen], num)

	var out [][]byte
	_ = p.Chain.WithReadSession(func(s *state.Session) error {
		dgpo, err := objects.GetDGPO(s)
		if err != nil || !hardfork.SetForBlock(dgpo.HeadBlockNumber).Has(hardfork.HF26) {
			return nil
		}
		sched := chain.IrreversibilitySchedule(s, dgpo.HeadBlockNumber)
		// Reference the last irreversible block, as hived does; the head if
		// that is not known.
		ref := block.ID(dgpo.HeadBlockID)
		if lib := dgpo.LastIrreversibleBlockNum; lib > 0 {
			if bs, err := objects.GetBlockSummary(s, lib); err == nil {
				ref = block.ID(bs.BlockID)
			}
		}
		for i := range p.Producers {
			prod := &p.Producers[i]
			if prod.Account == sh.Witness || !scheduled(sched, prod.Account) {
				continue
			}
			w, err := objects.GetWitness(s, auth.AccountName(prod.Account))
			if err != nil || w.BlockSigningKey != prod.PublicKey33 {
				continue
			}
			if body, err := signFastConfirm(prod, id, ref, dgpo.Time+fastConfirmExpiration); err == nil {
				out = append(out, body)
			} else {
				p.logEvery(time.Minute, "fast-confirm for %s, block %d: %v", prod.Account, num, err)
			}
		}
		return nil
	})
	return out
}

func scheduled(sched []auth.AccountName, name string) bool {
	for _, n := range sched {
		if string(n) == name {
			return true
		}
	}
	return false
}

// signFastConfirm builds and signs the single-op approval transaction.
func signFastConfirm(prod *Producer, approved, ref block.ID, expiration uint32) ([]byte, error) {
	t := tx.Transaction{
		RefBlockNum:    uint16(ref.Num()),
		RefBlockPrefix: binary.LittleEndian.Uint32(ref[4:8]),
		Expiration:     expiration,
		Operations: []op.Op{op.FromOperation(op.WitnessBlockApprove{
			Witness: auth.AccountName(prod.Account),
			BlockID: approved,
		})},
	}
	unsigned := tx.AppendTransactionBody(nil, &t)
	sig, err := crypto.SignCompact(prod.PrivateKey, crypto.SHA256Of2(network.ChainID[:], unsigned))
	if err != nil {
		return nil, err
	}
	st := tx.SignedTransaction{Transaction: t, Signatures: [][tx.SignatureSize]byte{[tx.SignatureSize]byte(sig)}}
	return tx.AppendSignedTransaction(nil, &st), nil
}
