package testfixture

import (
	"path/filepath"
	"reflect"
	"regexp"
	"testing"

	"gopherd/internal/chain"
	"gopherd/internal/chain/evaluator"
	"gopherd/internal/fcraw"
	"gopherd/internal/protocol/hardfork"
	"gopherd/internal/protocol/op"
	"gopherd/internal/state"
	"gopherd/internal/testutil/corpus"
)

// fuzzActors are the accounts the seed state funds; seed operations are
// rewritten to name them.
var fuzzActors = []string{"alice", "bob", "carol", "dave"}

// fuzzHardforks are the rule sets an input can be applied under, picked by
// its first byte: the eras where evaluator behaviour changes most.
var fuzzHardforks = []hardfork.ID{hardfork.HF28, hardfork.HF20, hardfork.HF25, hardfork.HF21, hardfork.HF29}

// FuzzApplyOp applies an arbitrary operation to a funded chain state.
//
// Operations reach the evaluators from any peer's block, and gopherd does not
// run op validators on network blocks (only at mempool admission), so an
// evaluator sees whatever a block producer put in. Whatever the input:
//   - no evaluator may panic: a panic on a crafted operation stops every
//     gopherd node that applies the block;
//   - an operation that passes its validator and applies without error must
//     conserve HIVE, HBD and VESTS (chain.CheckInvariants, a port of hived's
//     validate_invariants).
//
// The first input byte picks the hardfork era and whether the node is in
// control (producing or admitting rather than applying a block); the rest is
// one encoded operation. Each input runs on a throwaway session, so inputs
// cannot affect each other. Seeds are the mainnet operations of
// internal/protocol/op's corpus, rewritten to name this state's accounts.
func FuzzApplyOp(f *testing.F) {
	fx := NewShared(f)
	fx.HardforksUpTo(hardfork.HF28)
	fx.Actors(fuzzActors...)
	for _, a := range fuzzActors {
		fx.FundLiquid(a, 1_000_000)
		fx.FundLiquidHBD(a, 100_000)
		fx.FundVesting(a, 1_000_000)
	}
	fx.GenerateBlock()
	seedFuzzState(fx)

	base := state.NewSession(fx.State)
	before, err := chain.CheckInvariants(base)
	base.Close()
	if err != nil {
		f.Fatal(err)
	}
	if !before.OK() {
		f.Fatalf("seed state breaks the invariants:%s", before.FailString())
	}
	dgpo := fx.DGPO()

	for _, src := range corpus.LoadDir(f, filepath.Join("..", "..", "protocol", "op", "testdata", "fuzz", "FuzzReadOp")) {
		if seed, ok := remapSeed(src); ok {
			f.Add(append([]byte{0}, seed...))
		}
	}

	f.Fuzz(func(t *testing.T, in []byte) {
		if len(in) < 2 {
			return
		}
		mode, raw := in[0], in[1:]
		o, err := op.ReadOp(fcraw.NewReader(raw))
		if err != nil {
			return
		}
		hf := fuzzHardforks[int(mode&0x7f)%len(fuzzHardforks)]
		s := state.NewSession(fx.State)
		defer s.Close()
		defer s.Undo()
		ctx := &evaluator.Context{
			DB:                         s,
			Hardforks:                  hardfork.SetForBlock(hardfork.MainnetActivationBlock[hf] + 1),
			BlockNum:                   dgpo.HeadBlockNumber + 1,
			BlockTime:                  dgpo.Time + 3,
			HeadBlockTime:              dgpo.Time,
			InControl:                  mode&0x80 != 0,
			Witness:                    "alice",
			OwnerAuthHistoryStartBlock: 1,
		}
		applyErr := evaluator.ApplyOp(ctx, o)
		if applyErr != nil {
			return
		}
		if v, ok := o.Operation().(op.Validator); ok && v.Validate() != nil {
			return
		}
		after, err := chain.CheckInvariants(s)
		if err != nil {
			t.Fatalf("invariants after %s: %v", o.Tag.Name(), err)
		}
		if !after.OK() {
			t.Fatalf("%s (HF%d rules, in control %v) broke conservation:%s", o.Tag.Name(), hf, ctx.InControl, after.FailString())
		}
	})
}

// seedFuzzState gives the evaluators something to find beyond funded
// accounts: a witness, a post and a reply, a vote, a delegation, a proposal,
// open orders and a savings withdrawal.
func seedFuzzState(fx *Fixture) {
	fx.MustApplyOp(op.WitnessUpdate{
		Owner: "alice", URL: "u", BlockSigningKey: fx.ActorKey("alice").PublicKey(),
		Props: op.LegacyChainProperties{AccountCreationFee: LegacyHIVE(1_000), MaximumBlockSize: 65536},
		Fee:   HIVE(0),
	})
	fx.MustApplyOp(op.Comment{Author: "bob", Permlink: "p", ParentPermlink: "c", Title: "t", Body: "b", JSONMetadata: "{}"})
	fx.MustApplyOp(op.Comment{ParentAuthor: "bob", ParentPermlink: "p", Author: "carol", Permlink: "r", Body: "b", JSONMetadata: "{}"})
	fx.MustApplyOp(op.Vote{Voter: "alice", Author: "bob", Permlink: "p", Weight: 5000})
	fx.MustApplyOp(op.DelegateVestingShares{Delegator: "dave", Delegatee: "carol", VestingShares: VESTS(fx.Vests("dave") / 10)})
	start := fx.HeadBlockTime()
	fx.MustApplyOp(op.CreateProposal{Creator: "carol", Receiver: "carol", StartDate: start, EndDate: start + 30*86400, DailyPay: HBD(1000), Subject: "s", Permlink: "r"})
	fx.MustApplyOp(op.LimitOrderCreate{Owner: "dave", OrderID: 1, AmountToSell: HIVE(1000), MinToReceive: HBD(300), Expiration: start + 86400})
	fx.MustApplyOp(op.TransferToSavings{From: "bob", To: "bob", Amount: HBD(5000), Memo: ""})
	fx.MustApplyOp(op.TransferFromSavings{From: "bob", RequestID: 1, To: "bob", Amount: HBD(1000), Memo: ""})
	fx.GenerateBlock()
}

var accountName = regexp.MustCompile(`^[a-z][a-z0-9.-]{2,15}$`)

// notNames are string fields that hold free text or ids, never accounts.
var notNames = map[string]bool{
	"Title": true, "Body": true, "JSONMetadata": true, "PostingJSONMetadata": true,
	"Memo": true, "URL": true, "ID": true, "JSON": true, "Subject": true, "ParentPermlink": true,
}

// remapSeed rewrites a mainnet operation to name this state's accounts and
// post: account-like string fields map, in order of first appearance, onto
// fuzzActors, and permlinks onto bob's post. It returns the re-encoded
// operation.
func remapSeed(src []byte) ([]byte, bool) {
	o, err := op.ReadOp(fcraw.NewReader(src))
	if err != nil {
		return nil, false
	}
	v := reflect.ValueOf(o.Operation())
	cp := reflect.New(v.Type()).Elem()
	cp.Set(v)
	names := map[string]string{}
	var walk func(reflect.Value, string)
	walk = func(fv reflect.Value, field string) {
		switch fv.Kind() {
		case reflect.String:
			s := fv.String()
			switch {
			case field == "Permlink":
				fv.SetString("p")
			case notNames[field] || s == "" || !accountName.MatchString(s):
			default:
				m, ok := names[s]
				if !ok {
					m = fuzzActors[len(names)%len(fuzzActors)]
					names[s] = m
				}
				fv.SetString(m)
			}
		case reflect.Struct:
			for i := 0; i < fv.NumField(); i++ {
				if fv.Type().Field(i).IsExported() {
					walk(fv.Field(i), fv.Type().Field(i).Name)
				}
			}
		case reflect.Slice:
			if fv.Type().Elem().Kind() != reflect.Uint8 {
				for i := 0; i < fv.Len(); i++ {
					walk(fv.Index(i), field)
				}
			}
		case reflect.Pointer:
			if !fv.IsNil() {
				walk(fv.Elem(), field)
			}
		}
	}
	walk(cp, "")
	out, ok := cp.Interface().(op.Operation)
	if !ok {
		return nil, false
	}
	return op.AppendOperation(nil, out), true
}
