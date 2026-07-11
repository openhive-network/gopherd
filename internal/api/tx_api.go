package api

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"sort"

	"hived-go/internal/api/jsonrpc"
	"hived-go/internal/chain"
	"hived-go/internal/protocol/auth"
	"hived-go/internal/protocol/network"
	"hived-go/internal/protocol/tx"
	"hived-go/internal/state"
	"hived-go/internal/state/objects"
)

func (s *Server) registerTxAPI() {
	s.reg.Register("condenser_api.get_transaction_hex", s.mGetTransactionHex(false))
	s.reg.Register("database_api.get_transaction_hex", s.mGetTransactionHex(true))
	s.reg.Register("condenser_api.verify_authority", s.mVerifyAuthority(false))
	s.reg.Register("database_api.verify_authority", s.mVerifyAuthority(true))
	s.reg.Register("condenser_api.get_potential_signatures", s.mGetPotentialSignatures(true))
	s.reg.Register("condenser_api.get_required_signatures", s.mGetRequiredSignatures(true))
}

func authorityForLevel(au objects.AccountAuthority, level chain.AuthorityLevel) auth.Authority {
	switch level {
	case chain.AuthorityOwner:
		return au.Owner
	case chain.AuthorityActive:
		return au.Active
	case chain.AuthorityPosting:
		return au.Posting
	}
	return auth.Authority{}
}

// mVerifyAuthority: parse a tx, recover its signers, and check they satisfy the
// required authorities. Returns valid=true/false.
func (s *Server) mVerifyAuthority(named bool) jsonrpc.Handler {
	return func(_ context.Context, params json.RawMessage) (interface{}, error) {
		txRaw, err := txParam(params, named)
		if err != nil {
			return nil, err
		}
		st, perr := parseTransaction(txRaw)
		if perr != nil {
			return nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "parse transaction: %v", perr)
		}
		unsigned := tx.AppendTransactionBody(nil, &st.Transaction)
		valid := true
		_ = s.chain.WithReadSession(func(sess *state.Session) error {
			if verr := chain.VerifyAuthority(sess, &st, unsigned, network.ChainID, false); verr != nil {
				valid = false
			}
			return nil
		})
		if named {
			return map[string]interface{}{"valid": valid}, nil
		}
		return valid, nil
	}
}

// potentialSigningKeys collects the key_auths (one account-auth level deep) of
// every authority required by the tx.
func (s *Server) potentialSigningKeys(st *tx.SignedTransaction) []string {
	required := chain.RequiredAuthorities(st)
	keyset := map[string]bool{}
	_ = s.chain.WithReadSession(func(sess *state.Session) error {
		for level, accts := range required {
			for acct := range accts {
				au, err := objects.GetAccountAuthority(sess, auth.AccountName(acct))
				if err != nil {
					continue
				}
				a := authorityForLevel(au, level)
				for _, ka := range a.KeyAuths {
					keyset[ka.Key.String()] = true
				}
				for _, aa := range a.AccountAuths {
					if sub, err := objects.GetAccountAuthority(sess, aa.Account); err == nil {
						for _, ka := range authorityForLevel(sub, level).KeyAuths {
							keyset[ka.Key.String()] = true
						}
					}
				}
			}
		}
		return nil
	})
	out := make([]string, 0, len(keyset))
	for k := range keyset {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func (s *Server) mGetPotentialSignatures(named bool) jsonrpc.Handler {
	return func(_ context.Context, params json.RawMessage) (interface{}, error) {
		txRaw, err := txParam(params, named)
		if err != nil {
			return nil, err
		}
		st, perr := parseTransaction(txRaw)
		if perr != nil {
			return nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "parse transaction: %v", perr)
		}
		return s.potentialSigningKeys(&st), nil
	}
}

// mGetRequiredSignatures: condenser [tx, [available_keys]] → the subset of
// available_keys that appear in the tx's required authorities.
func (s *Server) mGetRequiredSignatures(named bool) jsonrpc.Handler {
	return func(_ context.Context, params json.RawMessage) (interface{}, error) {
		var arr []json.RawMessage
		if json.Unmarshal(params, &arr) != nil || len(arr) < 1 {
			return nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "expected [tx, available_keys]")
		}
		st, perr := parseTransaction(arr[0])
		if perr != nil {
			return nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "parse transaction: %v", perr)
		}
		var avail []string
		if len(arr) >= 2 {
			_ = json.Unmarshal(arr[1], &avail)
		}
		potential := map[string]bool{}
		for _, k := range s.potentialSigningKeys(&st) {
			potential[k] = true
		}
		out := []string{}
		for _, k := range avail {
			if potential[k] {
				out = append(out, k)
			}
		}
		return out, nil
	}
}

// txParam pulls the transaction JSON from condenser [tx] or database_api {"trx":tx}.
func txParam(params json.RawMessage, named bool) (json.RawMessage, error) {
	if named {
		var obj struct {
			Trx json.RawMessage `json:"trx"`
		}
		if err := json.Unmarshal(params, &obj); err == nil && obj.Trx != nil {
			return obj.Trx, nil
		}
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(params, &arr); err == nil && len(arr) > 0 {
		return arr[0], nil
	}
	// Fall back to treating the whole params as the tx.
	if len(params) > 0 && params[0] == '{' {
		return params, nil
	}
	return nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "expected a transaction")
}

func (s *Server) mGetTransactionHex(named bool) jsonrpc.Handler {
	return func(_ context.Context, params json.RawMessage) (interface{}, error) {
		txRaw, err := txParam(params, named)
		if err != nil {
			return nil, err
		}
		st, perr := parseTransaction(txRaw)
		if perr != nil {
			return nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "parse transaction: %v", perr)
		}
		b := tx.AppendSignedTransaction(nil, &st)
		h := hex.EncodeToString(b)
		if named {
			return map[string]interface{}{"hex": h}, nil
		}
		return h, nil
	}
}
