package evaluator

import (
	"errors"
	"fmt"

	"hived-go/internal/protocol/op"
	"hived-go/internal/state"
	"hived-go/internal/state/objects"
)

// Errors specific to witness evaluators.
var (
	ErrWitnessNotFound        = errors.New("witness not found")
	ErrTooManyWitnessVotes    = errors.New("account has too many active witness votes")
	ErrAlreadyVotedForWitness = errors.New("account already votes for this witness")
	ErrNotVotedForWitness     = errors.New("account does not currently vote for this witness")
	ErrCantVoteWithProxy      = errors.New("account witness vote not allowed while a proxy is set")
	ErrSelfProxy              = errors.New("cannot proxy to self")
)

// HIVE_MAX_ACCOUNT_WITNESS_VOTES from libraries/protocol/include/hive/protocol/config.hpp.
const maxAccountWitnessVotes uint16 = 30

// witnessUpdateEvaluator implements witness_update.
//
// Hived semantics: creates the witness record if missing, otherwise
// updates url / block_signing_key / props in place. The fee is debited
// only when the witness is being created (HF6 onward) — earlier blocks
// always charged the fee. We mirror the post-HF6 semantics for now.
type witnessUpdateEvaluator struct{}

func (witnessUpdateEvaluator) Apply(ctx *Context, o op.Operation) error {
	wu, ok := o.(op.WitnessUpdate)
	if !ok {
		return fmt.Errorf("witnessUpdateEvaluator: unexpected op type %T", o)
	}
	// hived's witness_update_evaluator only verifies the owner exists.
	// The op's `fee` field is IGNORED — there is no on-chain fee for
	// becoming a witness; the field is a historical artifact. Charging it
	// would lock out witnesses with low HIVE balances. See
	// libraries/chain/hive_evaluator.cpp::witness_update_evaluator::do_apply.
	if has, err := objects.HasAccount(ctx.DB, wu.Owner); err != nil {
		return err
	} else if !has {
		return fmt.Errorf("%w: owner=%q", ErrAccountNotFound, wu.Owner)
	}

	exists, err := objects.HasWitness(ctx.DB, wu.Owner)
	if err != nil {
		return err
	}

	w := objects.Witness{
		Owner:              wu.Owner,
		Created:            ctx.BlockTime,
		URL:                wu.URL,
		BlockSigningKey:    wu.BlockSigningKey,
		AccountCreationFee: wu.Props.AccountCreationFee.Amount,
		MaximumBlockSize:   wu.Props.MaximumBlockSize,
		HBDInterestRate:    wu.Props.HBDInterestRate,
	}
	if exists {
		// Preserve cumulative counters that aren't in the op.
		prev, err := objects.GetWitness(ctx.DB, wu.Owner)
		if err == nil {
			w.Created = prev.Created
			w.TotalVotesVests = prev.TotalVotesVests
			w.TotalMissedBlocks = prev.TotalMissedBlocks
			w.HardforkVersionVote = prev.HardforkVersionVote
			w.HardforkTimeVote = prev.HardforkTimeVote
			w.RunningVersion = prev.RunningVersion
		}
	}
	objects.PutWitness(ctx.DB, &w)
	return nil
}

// witnessSetPropertiesEvaluator implements HF20+ witness_set_properties.
//
// hived parses the flat_map<string, vector<char>> against ~14 known keys
// (account_creation_fee, maximum_block_size, hbd_interest_rate, key,
// new_signing_key, url, account_subsidy_budget, etc.). For ghive's
// minimum-viable implementation we touch the three keys that map to
// existing Witness fields; unknown keys are accepted silently so the
// pipeline doesn't reject otherwise-valid blocks.
type witnessSetPropertiesEvaluator struct{}

func (witnessSetPropertiesEvaluator) Apply(ctx *Context, o op.Operation) error {
	wsp, ok := o.(op.WitnessSetProperties)
	if !ok {
		return fmt.Errorf("witnessSetPropertiesEvaluator: unexpected op type %T", o)
	}
	w, err := objects.GetWitness(ctx.DB, wsp.Owner)
	if err != nil {
		if err == state.ErrNotFound {
			return fmt.Errorf("%w: owner=%q (witness_set_properties before witness_update)", ErrWitnessNotFound, wsp.Owner)
		}
		return err
	}

	// hived: the "key" prop MUST match the witness's current signing
	// key — it's a self-attestation that this op came from the holder
	// of that key (libraries/chain/hive_evaluator.cpp:130). The op has
	// no signature meta, so this `key` is the only proof.
	for _, p := range wsp.Props {
		if p.Key == "key" {
			if len(p.Value) >= 33 {
				var supplied [33]byte
				copy(supplied[:], p.Value[:33])
				if supplied != w.BlockSigningKey {
					return fmt.Errorf("witness_set_properties: 'key' does not match witness signing key for %q", wsp.Owner)
				}
			}
		}
	}

	for _, p := range wsp.Props {
		switch p.Key {
		case "account_creation_fee":
			// 8 bytes amount + 4 or 8 bytes symbol — for legacy_hive_asset.
			// We're lenient here since the symbol form varies.
			if len(p.Value) >= 8 {
				w.AccountCreationFee = int64(le64(p.Value[:8]))
			}
		case "maximum_block_size":
			if len(p.Value) >= 4 {
				w.MaximumBlockSize = le32(p.Value[:4])
			}
		case "hbd_interest_rate":
			if len(p.Value) >= 2 {
				w.HBDInterestRate = uint16(p.Value[0]) | uint16(p.Value[1])<<8
			}
		case "key", "new_signing_key":
			if len(p.Value) >= 33 {
				copy(w.BlockSigningKey[:], p.Value[:33])
			}
		case "url":
			// vector<char> = varint length + bytes; for url the value is the
			// payload bytes verbatim.
			w.URL = string(p.Value)
		}
		// Other keys (account_subsidy_budget, etc.) are silently accepted
		// until corresponding Witness fields land.
	}
	objects.PutWitness(ctx.DB, &w)
	return nil
}

// accountWitnessVoteEvaluator implements account_witness_vote.
type accountWitnessVoteEvaluator struct{}

func (accountWitnessVoteEvaluator) Apply(ctx *Context, o op.Operation) error {
	wv, ok := o.(op.AccountWitnessVote)
	if !ok {
		return fmt.Errorf("accountWitnessVoteEvaluator: unexpected op type %T", o)
	}

	voter, err := objects.GetAccount(ctx.DB, wv.Account)
	if err != nil {
		if err == state.ErrNotFound {
			return fmt.Errorf("%w: account=%q", ErrAccountNotFound, wv.Account)
		}
		return err
	}
	if voter.Proxy != "" {
		return ErrCantVoteWithProxy
	}

	w, err := objects.GetWitness(ctx.DB, wv.Witness)
	if err != nil {
		if err == state.ErrNotFound {
			return fmt.Errorf("%w: witness=%q", ErrWitnessNotFound, wv.Witness)
		}
		return err
	}

	had, err := objects.HasWitnessVote(ctx.DB, wv.Account, wv.Witness)
	if err != nil {
		return err
	}

	if wv.Approve {
		if had {
			return fmt.Errorf("%w: %q -> %q", ErrAlreadyVotedForWitness, wv.Account, wv.Witness)
		}
		if voter.WitnessesVotedFor >= maxAccountWitnessVotes {
			return fmt.Errorf("%w: %q has %d/%d", ErrTooManyWitnessVotes, wv.Account, voter.WitnessesVotedFor, maxAccountWitnessVotes)
		}
		objects.PutWitnessVote(ctx.DB, wv.Account, wv.Witness)
		voter.WitnessesVotedFor++
		w.TotalVotesVests += voter.VestingShares
	} else {
		if !had {
			return fmt.Errorf("%w: %q -> %q", ErrNotVotedForWitness, wv.Account, wv.Witness)
		}
		objects.DeleteWitnessVote(ctx.DB, wv.Account, wv.Witness)
		if voter.WitnessesVotedFor > 0 {
			voter.WitnessesVotedFor--
		}
		w.TotalVotesVests -= voter.VestingShares
		if w.TotalVotesVests < 0 {
			w.TotalVotesVests = 0
		}
	}
	objects.PutAccount(ctx.DB, &voter)
	objects.PutWitness(ctx.DB, &w)
	return nil
}

// accountWitnessProxyEvaluator implements account_witness_proxy.
type accountWitnessProxyEvaluator struct{}

func (accountWitnessProxyEvaluator) Apply(ctx *Context, o op.Operation) error {
	pr, ok := o.(op.AccountWitnessProxy)
	if !ok {
		return fmt.Errorf("accountWitnessProxyEvaluator: unexpected op type %T", o)
	}
	if pr.Account == pr.Proxy {
		return ErrSelfProxy
	}
	a, err := objects.GetAccount(ctx.DB, pr.Account)
	if err != nil {
		if err == state.ErrNotFound {
			return fmt.Errorf("%w: account=%q", ErrAccountNotFound, pr.Account)
		}
		return err
	}
	// HIVE_PROXY_TO_SELF_ACCOUNT (= "") clears the proxy.
	a.Proxy = pr.Proxy
	objects.PutAccount(ctx.DB, &a)
	return nil
}

// --- low-level LE readers --------------------------------------------

func le32(b []byte) uint32 {
	return uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24
}

func le64(b []byte) uint64 {
	return uint64(b[0]) | uint64(b[1])<<8 | uint64(b[2])<<16 | uint64(b[3])<<24 |
		uint64(b[4])<<32 | uint64(b[5])<<40 | uint64(b[6])<<48 | uint64(b[7])<<56
}

func init() {
	register(op.TagWitnessUpdate, witnessUpdateEvaluator{})
	register(op.TagWitnessSetProperties, witnessSetPropertiesEvaluator{})
	register(op.TagAccountWitnessVote, accountWitnessVoteEvaluator{})
	register(op.TagAccountWitnessProxy, accountWitnessProxyEvaluator{})
}
