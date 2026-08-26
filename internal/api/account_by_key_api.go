package api

import (
	"context"
	"encoding/json"

	"gopherd/internal/api/jsonrpc"
	"gopherd/internal/crypto"
	"gopherd/internal/state"
	"gopherd/internal/state/objects"
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
		for i := range keys {
			refs[i] = []string{}
		}
		// One indexed seek per requested key. This used to walk every account
		// and load every authority row under the apply read lock -- ~2M
		// accounts on mainnet -- which let one cheap RPC stall block
		// application. Unparseable keys simply match nothing, as before.
		_ = s.chain.WithReadSession(func(sess *state.Session) error {
			for i, k := range keys {
				pk, err := crypto.DecodePublicKey(k)
				if err != nil {
					continue
				}
				objects.RangeAccountsByKey(sess, pk, func(name string) bool {
					refs[i] = append(refs[i], name)
					return true
				})
			}
			return nil
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
