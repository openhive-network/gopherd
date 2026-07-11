package api

import (
	"context"
	"encoding/hex"
	"encoding/json"

	"hived-go/internal/api/jsonrpc"
	"hived-go/internal/protocol/tx"
)

func (s *Server) registerTxAPI() {
	s.reg.Register("condenser_api.get_transaction_hex", s.mGetTransactionHex(true))
	s.reg.Register("database_api.get_transaction_hex", s.mGetTransactionHex(false))
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
