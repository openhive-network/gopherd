// pack_type.go — HF26 pack-type plumbing.
//
// Background. Until HIVE_HARDFORK_1_26_ENABLE_NEW_SERIALIZATION (HF26),
// the only acceptable on-the-wire encoding for an asset_symbol inside a
// transaction was the 8-byte "legacy" form ("STEEM\0\0"/"SBD\0\0\0\0"/
// "VESTS\0\0" prefixed by the precision byte). HF26 introduced an
// alternative 4-byte NAI form ("hf26 pack"). The chain-side gate that
// rejects the new encoding pre-HF26 is database.cpp:
//
//	if (!has_hardfork(HIVE_HARDFORK_1_26_ENABLE_NEW_SERIALIZATION))
//	  HIVE_ASSERT(full_transaction->is_legacy_pack(),
//	              hive::protocol::transaction_auth_exception,
//	              "legacy serialization must be used until hardfork 26");
//
// where full_transaction::is_legacy_pack() re-packs the parsed
// signed_transaction with serialization_mode=legacy and byte-compares
// against the wire (full_transaction.cpp:262).
//
// The Go codec (see tx.go + protocol/asset/symbol.go) preserves per-
// symbol pack provenance via Symbol.WasLegacyEncoded — parsing a
// legacy-form symbol sets it true; parsing the 4-byte NAI form leaves
// it false. So the C++ "re-pack-as-legacy + memcmp" check has a direct
// Go equivalent: a parsed transaction is legacy-packed iff every
// asset symbol inside it has WasLegacyEncoded == true.
//
// This file implements:
//   - IsLegacyPack(wire) — the consensus-level pre-HF26 gate predicate.
//   - RepackAsHF26(wire) — companion encoder that re-emits the parsed
//     transaction with all well-known symbols forced to the 4-byte
//     NAI form. Used by tests that want to drive a non-legacy-packed
//     blob through the unpacker without hand-rolling the bytes.
//
// Together these are sufficient to reproduce the C++
// hf26_tests.cpp::pack_transaction_basic contrast without porting the
// full serialization_mode_controller / full_transaction machinery.
package tx

import (
	"bytes"
	"fmt"

	"gopherd/internal/fcraw"
	"gopherd/internal/protocol/asset"
	"gopherd/internal/protocol/op"
)

// IsLegacyPack reports whether the wire-encoded signed_transaction in
// `wire` is packed in the legacy serialization mode — i.e. every asset
// symbol it contains is in the 8-byte legacy form (HIVE → "STEEM",
// HBD → "SBD", VESTS → "VESTS"). Mirrors C++
// full_transaction::is_legacy_pack().
//
// Implementation note: this parses `wire` and reads
// Symbol.WasLegacyEncoded on every asset-bearing op field rather than
// round-trip-re-encoding + memcmp. The two are equivalent — Symbol's
// pack provenance is the single bit of state that distinguishes the
// 8-byte and 4-byte serializations — and the direct read is faster.
func IsLegacyPack(wire []byte) (bool, error) {
	res, err := ReadSignedTransaction(fcraw.NewReader(wire))
	if err != nil {
		return false, fmt.Errorf("tx.IsLegacyPack: parse: %w", err)
	}
	legacy := true
	walkAssetSymbols(&res.SignedTx.Transaction, func(s *asset.Symbol) {
		// Only the three well-known legacy symbols can be encoded in
		// either form; pure-SMT/NAI symbols (HF24+ SMT space) have no
		// legacy 8-byte representation and are not gated.
		if isWellKnownLegacy(s.Num) && !s.WasLegacyEncoded {
			legacy = false
		}
	})
	return legacy, nil
}

// RepackAsHF26 takes a legacy-packed signed_transaction wire and
// re-emits it with every well-known asset symbol forced to the 4-byte
// NAI ("hf26") form. The header, op tags, non-asset op fields, and
// signatures are byte-identical to the input.
//
// Returns the parsed SignedTransaction alongside the new wire so
// callers can assert against the structured form.
func RepackAsHF26(wire []byte) (*SignedTransaction, []byte, error) {
	res, err := ReadSignedTransaction(fcraw.NewReader(wire))
	if err != nil {
		return nil, nil, fmt.Errorf("tx.RepackAsHF26: parse: %w", err)
	}
	st := &res.SignedTx
	walkAssetSymbols(&st.Transaction, func(s *asset.Symbol) {
		if isWellKnownLegacy(s.Num) {
			s.WasLegacyEncoded = false
		}
	})
	out := AppendSignedTransaction(nil, st)
	return st, out, nil
}

// RepackAsLegacy is the inverse of RepackAsHF26 — emits a wire blob
// where every well-known symbol is in 8-byte legacy form regardless of
// the original encoding. Round-trips IsLegacyPack to true.
func RepackAsLegacy(wire []byte) (*SignedTransaction, []byte, error) {
	res, err := ReadSignedTransaction(fcraw.NewReader(wire))
	if err != nil {
		return nil, nil, fmt.Errorf("tx.RepackAsLegacy: parse: %w", err)
	}
	st := &res.SignedTx
	walkAssetSymbols(&st.Transaction, func(s *asset.Symbol) {
		if isWellKnownLegacy(s.Num) {
			s.WasLegacyEncoded = true
		}
	})
	out := AppendSignedTransaction(nil, st)
	return st, out, nil
}

// WireEqual is a small convenience that compares two wire blobs for
// byte equality (avoids dragging bytes into every test).
func WireEqual(a, b []byte) bool { return bytes.Equal(a, b) }

// isWellKnownLegacy reports whether n is one of the three legacy-
// encodable asset_nums (HIVE/HBD/VESTS). Only these have an 8-byte
// legacy serialization; SMT NAI symbols are always 4 bytes.
func isWellKnownLegacy(n asset.AssetNum) bool {
	switch n {
	case asset.HiveSymbol, asset.HbdSymbol, asset.VestsSymbol:
		return true
	}
	return false
}

// walkAssetSymbols invokes fn for every asset_symbol_type inside t —
// across every regular op type that carries an asset or price field.
// Pointers are into the parsed Op storage; mutations through fn are
// visible to a subsequent AppendSignedTransaction.
//
// Coverage matches the union of every `asset.Asset` / `asset.Price`
// field defined in the protocol/op package (see ops_*.go).
func walkAssetSymbols(t *Transaction, fn func(*asset.Symbol)) {
	for i := range t.Operations {
		o := &t.Operations[i]
		switch o.OpTag() {
		case op.TagTransfer:
			fn(&o.AsTransfer().Amount.Symbol)
		case op.TagTransferToVesting:
			fn(&o.AsTransferToVesting().Amount.Symbol)
		case op.TagWithdrawVesting:
			fn(&o.AsWithdrawVesting().VestingShares.Symbol)
		case op.TagLimitOrderCreate:
			x := o.AsLimitOrderCreate()
			fn(&x.AmountToSell.Symbol)
			fn(&x.MinToReceive.Symbol)
		case op.TagFeedPublish:
			x := o.AsFeedPublish()
			fn(&x.ExchangeRate.Base.Symbol)
			fn(&x.ExchangeRate.Quote.Symbol)
		case op.TagConvert:
			fn(&o.AsConvert().Amount.Symbol)
		case op.TagAccountCreate:
			fn(&o.AsAccountCreate().Fee.Symbol)
		case op.TagWitnessUpdate:
			fn(&o.AsWitnessUpdate().Fee.Symbol)
		case op.TagCommentOptions:
			fn(&o.AsCommentOptions().MaxAcceptedPayout.Symbol)
		case op.TagLimitOrderCreate2:
			x := o.AsLimitOrderCreate2()
			fn(&x.AmountToSell.Symbol)
			fn(&x.ExchangeRate.Base.Symbol)
			fn(&x.ExchangeRate.Quote.Symbol)
		case op.TagEscrowTransfer:
			x := o.AsEscrowTransfer()
			fn(&x.HbdAmount.Symbol)
			fn(&x.HiveAmount.Symbol)
			fn(&x.Fee.Symbol)
		case op.TagEscrowRelease:
			x := o.AsEscrowRelease()
			fn(&x.HbdAmount.Symbol)
			fn(&x.HiveAmount.Symbol)
		case op.TagTransferToSavings:
			fn(&o.AsTransferToSavings().Amount.Symbol)
		case op.TagTransferFromSavings:
			fn(&o.AsTransferFromSavings().Amount.Symbol)
		case op.TagClaimRewardBalance:
			x := o.AsClaimRewardBalance()
			fn(&x.RewardHive.Symbol)
			fn(&x.RewardHbd.Symbol)
			fn(&x.RewardVests.Symbol)
		case op.TagDelegateVestingShares:
			fn(&o.AsDelegateVestingShares().VestingShares.Symbol)
		case op.TagAccountCreateWithDelegation:
			x := o.AsAccountCreateWithDelegation()
			fn(&x.Fee.Symbol)
			fn(&x.Delegation.Symbol)
		case op.TagCreateProposal:
			fn(&o.AsCreateProposal().DailyPay.Symbol)
		case op.TagUpdateProposal:
			fn(&o.AsUpdateProposal().DailyPay.Symbol)
		case op.TagCollateralizedConvert:
			fn(&o.AsCollateralizedConvert().Amount.Symbol)
		case op.TagRecurrentTransfer:
			fn(&o.AsRecurrentTransfer().Amount.Symbol)
		}
	}
}
