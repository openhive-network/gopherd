package api

import (
	"context"
	"encoding/json"

	"hived-go/internal/api/jsonrpc"
	"hived-go/internal/protocol/tx"
)

// registerAccountHistoryAPI wires the block-op reads that DON'T need a persistent
// op index. get_account_history / get_transaction / enum_virtual_ops need the
// account-history index subsystem (Phase E, not built) and are omitted.
func (s *Server) registerAccountHistoryAPI() {
	s.reg.Register("account_history_api.get_ops_in_block", s.mGetOpsInBlock(false))
	s.reg.Register("condenser_api.get_ops_in_block", s.mGetOpsInBlock(true))
}

// mGetOpsInBlock returns the (real) operations of a block with their metadata.
// Virtual operations are NOT included yet — gopherd only emits a subset of the
// virtual-op stream; full coverage needs the account-history subsystem. If the
// caller asks for only_virtual, we return an empty list for now.
func (s *Server) mGetOpsInBlock(legacy bool) jsonrpc.Handler {
	return func(_ context.Context, params json.RawMessage) (interface{}, error) {
		blockNum, onlyVirtual, err := opsInBlockParams(params)
		if err != nil {
			return nil, err
		}
		raw := s.chain.RecentBlockBytes(blockNum)
		if raw == nil {
			return []interface{}{}, nil
		}
		sh, txs, _, perr := parseBlock(raw)
		if perr != nil {
			return nil, jsonrpc.Errorf(jsonrpc.CodeServerError, "decode block %d: %v", blockNum, perr)
		}
		ts := timeString(sh.Timestamp)
		out := []map[string]interface{}{}
		if onlyVirtual {
			return out, nil
		}
		for ti := range txs {
			trxID := txID(tx.AppendTransactionBody(nil, &txs[ti].Transaction))
			for oi, o := range txs[ti].Operations {
				var opv interface{}
				if legacy {
					opv = opToJSON(o, true)
				} else {
					opv = opToVariant(o, false)
				}
				out = append(out, map[string]interface{}{
					"trx_id":       trxID,
					"block":        blockNum,
					"trx_in_block": ti,
					"op_in_trx":    oi,
					"virtual_op":   0,
					"timestamp":    ts,
					"op":           opv,
				})
			}
		}
		return out, nil
	}
}

func opsInBlockParams(params json.RawMessage) (uint32, bool, error) {
	// positional [block_num, only_virtual]
	var arr []json.RawMessage
	if json.Unmarshal(params, &arr) == nil && len(arr) > 0 {
		var n uint32
		if err := json.Unmarshal(arr[0], &n); err != nil {
			return 0, false, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "block_num: %v", err)
		}
		ov := false
		if len(arr) >= 2 {
			_ = json.Unmarshal(arr[1], &ov)
		}
		return n, ov, nil
	}
	// named {block_num, only_virtual}
	var obj struct {
		BlockNum    uint32 `json:"block_num"`
		OnlyVirtual bool   `json:"only_virtual"`
	}
	if err := json.Unmarshal(params, &obj); err != nil {
		return 0, false, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "params: %v", err)
	}
	return obj.BlockNum, obj.OnlyVirtual, nil
}
