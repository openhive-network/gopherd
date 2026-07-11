package api

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"

	"hived-go/internal/crypto"
	"hived-go/internal/protocol/asset"
	"hived-go/internal/protocol/auth"
	"hived-go/internal/protocol/op"
	"hived-go/internal/protocol/tx"
)

// opProtos lists a zero value of every operation the parser can build (the ops
// wallets/apps actually submit). name→type is derived from Tag.Name().
var opProtos = []op.Operation{
	op.Vote{}, op.Comment{}, op.Transfer{}, op.TransferToVesting{}, op.WithdrawVesting{},
	op.LimitOrderCreate{}, op.LimitOrderCreate2{}, op.LimitOrderCancel{}, op.FeedPublish{},
	op.Convert{}, op.CollateralizedConvert{}, op.AccountCreate{}, op.AccountCreateWithDelegation{},
	op.AccountUpdate{}, op.AccountUpdate2{}, op.WitnessUpdate{}, op.WitnessSetProperties{},
	op.AccountWitnessVote{}, op.AccountWitnessProxy{}, op.CustomJSON{}, op.Custom{}, op.CustomBinary{},
	op.DeleteComment{}, op.CommentOptions{}, op.SetWithdrawVestingRoute{}, op.ClaimAccount{},
	op.CreateClaimedAccount{}, op.RequestAccountRecovery{}, op.RecoverAccount{}, op.ChangeRecoveryAccount{},
	op.EscrowTransfer{}, op.EscrowDispute{}, op.EscrowRelease{}, op.EscrowApprove{},
	op.TransferToSavings{}, op.TransferFromSavings{}, op.CancelTransferFromSavings{},
	op.DeclineVotingRights{}, op.ResetAccount{}, op.SetResetAccount{}, op.ClaimRewardBalance{},
	op.DelegateVestingShares{}, op.CreateProposal{}, op.UpdateProposal{}, op.UpdateProposalVotes{},
	op.RemoveProposal{}, op.RecurrentTransfer{},
}

var opTypeByName = func() map[string]reflect.Type {
	m := make(map[string]reflect.Type, len(opProtos))
	for _, p := range opProtos {
		m[p.OpTag().Name()] = reflect.TypeOf(p)
	}
	return m
}()

// parseOp builds an op.Op from hived's legacy ["name", {fields}] form.
func parseOp(raw json.RawMessage) (op.Op, error) {
	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err != nil || len(arr) != 2 {
		// try modern {type, value}
		var mv struct {
			Type  string          `json:"type"`
			Value json.RawMessage `json:"value"`
		}
		if json.Unmarshal(raw, &mv) == nil && mv.Type != "" {
			name := mv.Type
			if len(name) > 10 && name[len(name)-10:] == "_operation" {
				name = name[:len(name)-10]
			}
			return buildOp(name, mv.Value)
		}
		return op.Op{}, fmt.Errorf("operation must be [name, {...}] or {type,value}")
	}
	var name string
	if err := json.Unmarshal(arr[0], &name); err != nil {
		return op.Op{}, fmt.Errorf("operation name: %w", err)
	}
	return buildOp(name, arr[1])
}

func buildOp(name string, fieldsRaw json.RawMessage) (op.Op, error) {
	t, ok := opTypeByName[name]
	if !ok {
		return op.Op{}, fmt.Errorf("unsupported operation %q", name)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(fieldsRaw, &fields); err != nil {
		return op.Op{}, fmt.Errorf("%s fields: %w", name, err)
	}
	pv := reflect.New(t) // *T
	sv := pv.Elem()
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.PkgPath != "" {
			continue
		}
		raw, present := fields[snakeCase(f.Name)]
		if !present {
			continue
		}
		if err := setField(sv.Field(i), raw); err != nil {
			return op.Op{}, fmt.Errorf("%s.%s: %w", name, f.Name, err)
		}
	}
	return op.FromOperation(sv.Interface().(op.Operation)), nil
}

// setField populates one struct field from its JSON value, inverting the
// convertValue rendering (asset from string or NAI object, pubkey from STM,
// authority from {weight_threshold,...}, byte arrays from hex, else json).
func setField(fv reflect.Value, raw json.RawMessage) error {
	switch fv.Interface().(type) {
	case asset.Asset:
		a, err := parseAsset(raw)
		if err != nil {
			return err
		}
		fv.Set(reflect.ValueOf(a))
		return nil
	case crypto.PublicKey:
		k, err := parsePubKey(raw)
		if err != nil {
			return err
		}
		fv.Set(reflect.ValueOf(k))
		return nil
	case auth.AccountName:
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return err
		}
		fv.SetString(s)
		return nil
	case auth.Authority:
		a, err := parseAuthority(raw)
		if err != nil {
			return err
		}
		fv.Set(reflect.ValueOf(a))
		return nil
	case *auth.Authority:
		if string(raw) == "null" {
			return nil
		}
		a, err := parseAuthority(raw)
		if err != nil {
			return err
		}
		fv.Set(reflect.ValueOf(&a))
		return nil
	}
	switch fv.Kind() {
	case reflect.Array:
		if fv.Type().Elem().Kind() == reflect.Uint8 {
			var s string
			if err := json.Unmarshal(raw, &s); err != nil {
				return err
			}
			b, err := hex.DecodeString(s)
			if err != nil {
				return err
			}
			reflect.Copy(fv, reflect.ValueOf(b))
			return nil
		}
	}
	// Primitives, strings, and slices of them: standard json into the addr.
	return json.Unmarshal(raw, fv.Addr().Interface())
}

func parseAsset(raw json.RawMessage) (asset.Asset, error) {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return asset.ParseLegacyAsset(s)
	}
	var a asset.Asset
	if err := a.UnmarshalJSON(raw); err != nil {
		return asset.Asset{}, err
	}
	return a, nil
}

func parsePubKey(raw json.RawMessage) (crypto.PublicKey, error) {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return crypto.PublicKey{}, err
	}
	return crypto.DecodePublicKey(s)
}

func parseAuthority(raw json.RawMessage) (auth.Authority, error) {
	var a struct {
		WeightThreshold uint32               `json:"weight_threshold"`
		AccountAuths    [][2]json.RawMessage `json:"account_auths"`
		KeyAuths        [][2]json.RawMessage `json:"key_auths"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return auth.Authority{}, err
	}
	out := auth.Authority{WeightThreshold: a.WeightThreshold}
	for _, aa := range a.AccountAuths {
		var name string
		var w uint16
		_ = json.Unmarshal(aa[0], &name)
		_ = json.Unmarshal(aa[1], &w)
		out.AccountAuths = append(out.AccountAuths, auth.AccountAuth{Account: auth.AccountName(name), Weight: auth.Weight(w)})
	}
	for _, ka := range a.KeyAuths {
		var ks string
		var w uint16
		_ = json.Unmarshal(ka[0], &ks)
		_ = json.Unmarshal(ka[1], &w)
		k, err := crypto.DecodePublicKey(ks)
		if err != nil {
			return auth.Authority{}, err
		}
		out.KeyAuths = append(out.KeyAuths, auth.KeyAuth{Key: k, Weight: auth.Weight(w)})
	}
	return out, nil
}

// parseTransaction builds a tx.SignedTransaction from hived's JSON form.
func parseTransaction(raw json.RawMessage) (tx.SignedTransaction, error) {
	var t struct {
		RefBlockNum    uint16            `json:"ref_block_num"`
		RefBlockPrefix uint32            `json:"ref_block_prefix"`
		Expiration     string            `json:"expiration"`
		Operations     []json.RawMessage `json:"operations"`
		Signatures     []string          `json:"signatures"`
	}
	if err := json.Unmarshal(raw, &t); err != nil {
		return tx.SignedTransaction{}, err
	}
	exp, err := parseTimeString(t.Expiration)
	if err != nil {
		return tx.SignedTransaction{}, fmt.Errorf("expiration: %w", err)
	}
	st := tx.SignedTransaction{Transaction: tx.Transaction{
		RefBlockNum:    t.RefBlockNum,
		RefBlockPrefix: t.RefBlockPrefix,
		Expiration:     exp,
	}}
	for i, oraw := range t.Operations {
		o, err := parseOp(oraw)
		if err != nil {
			return tx.SignedTransaction{}, fmt.Errorf("op %d: %w", i, err)
		}
		st.Operations = append(st.Operations, o)
	}
	for _, s := range t.Signatures {
		b, err := hex.DecodeString(s)
		if err != nil || len(b) != tx.SignatureSize {
			return tx.SignedTransaction{}, fmt.Errorf("bad signature hex")
		}
		var sig [tx.SignatureSize]byte
		copy(sig[:], b)
		st.Signatures = append(st.Signatures, sig)
	}
	return st, nil
}
