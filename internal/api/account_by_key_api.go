package api

import (
	"context"
	"encoding/json"

	"hived-go/internal/api/jsonrpc"
	"hived-go/internal/crypto"
	"hived-go/internal/protocol/auth"
	"hived-go/internal/state"
	"hived-go/internal/state/objects"
)

// registerAccountByKeyAPI wires get_key_references (pubkey → accounts).
//
// First cut: an ON-DEMAND scan of every account's authorities + memo key, rather
// than a maintained key→account index. Correct and needs no new apply-time
// subsystem; O(accounts) per call, so a real deployment should later back it with
// the account_by_key index. Read-only.
func (s *Server) registerAccountByKeyAPI() {
	s.reg.Register("account_by_key_api.get_key_references", s.mGetKeyReferences(true))
	s.reg.Register("condenser_api.get_key_references", s.mGetKeyReferences(false))
}

func (s *Server) mGetKeyReferences(named bool) jsonrpc.Handler {
	return func(_ context.Context, params json.RawMessage) (interface{}, error) {
		keys, err := keysFromParams(params, named)
		if err != nil {
			return nil, err
		}
		refs := make([][]string, len(keys))
		want := make(map[string]int, len(keys))
		for i, k := range keys {
			want[k] = i
			refs[i] = []string{}
		}
		add := func(key, name string, seen map[int]bool) {
			if idx, ok := want[key]; ok && !seen[idx] {
				refs[idx] = append(refs[idx], name)
				seen[idx] = true
			}
		}
		_ = s.chain.WithReadSession(func(sess *state.Session) error {
			return objects.RangeAccounts(sess, func(a *objects.Account) (bool, error) {
				name := string(a.Name)
				seen := map[int]bool{}
				add(crypto.PublicKey(a.MemoKey).String(), name, seen)
				au, err := objects.GetAccountAuthority(sess, a.Name)
				if err != nil {
					return true, nil
				}
				for _, kas := range [][]auth.KeyAuth{au.Owner.KeyAuths, au.Active.KeyAuths, au.Posting.KeyAuths} {
					for _, ka := range kas {
						add(ka.Key.String(), name, seen)
					}
				}
				return true, nil
			})
		})
		if named {
			return map[string]interface{}{"accounts": refs}, nil
		}
		return refs, nil
	}
}

// keysFromParams decodes the key list from {"keys":[...]} or [["STM..",..]].
func keysFromParams(params json.RawMessage, named bool) ([]string, error) {
	var obj struct {
		Keys []string `json:"keys"`
	}
	if err := json.Unmarshal(params, &obj); err == nil && obj.Keys != nil {
		return obj.Keys, nil
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(params, &arr); err == nil && len(arr) > 0 {
		var keys []string
		if err := json.Unmarshal(arr[0], &keys); err == nil {
			return keys, nil
		}
	}
	return nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "expected {\"keys\":[...]} or [[...]]")
}
