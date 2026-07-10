package api

import (
	"context"
	"encoding/hex"
	"encoding/json"

	"hived-go/internal/api/jsonrpc"
	"hived-go/internal/protocol/block"
)

func (s *Server) registerBlockAPI() {
	s.reg.Register("block_api.get_block_header", s.mGetBlockHeader())
	// get_block / get_block_range need operation→JSON expansion (all ~50 op
	// types); tracked separately. get_block_header is header-only so it lands now.
}

// blockNumParam extracts block_num from either {"block_num":N} (named) or [N]
// (positional/condenser).
func blockNumParam(params json.RawMessage) (uint32, error) {
	if len(params) == 0 {
		return 0, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "missing block_num")
	}
	// Positional: [N]
	var arr []json.RawMessage
	if err := json.Unmarshal(params, &arr); err == nil {
		if len(arr) == 0 {
			return 0, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "empty params")
		}
		var n uint32
		if err := json.Unmarshal(arr[0], &n); err != nil {
			return 0, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "block_num: %v", err)
		}
		return n, nil
	}
	// Named: {"block_num":N}
	var obj struct {
		BlockNum uint32 `json:"block_num"`
	}
	if err := json.Unmarshal(params, &obj); err != nil {
		return 0, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "params: %v", err)
	}
	return obj.BlockNum, nil
}

func blockHeaderResult(sh block.SignedHeader) map[string]interface{} {
	return map[string]interface{}{
		"previous":                hex.EncodeToString(sh.Previous[:]),
		"timestamp":               timeString(sh.Timestamp),
		"witness":                 sh.Witness,
		"transaction_merkle_root": hex.EncodeToString(sh.TransactionMerkleRoot[:]),
		"extensions":              []interface{}{},
	}
}

func (s *Server) mGetBlockHeader() jsonrpc.Handler {
	return func(_ context.Context, params json.RawMessage) (interface{}, error) {
		n, err := blockNumParam(params)
		if err != nil {
			return nil, err
		}
		raw := s.chain.RecentBlockBytes(n)
		if raw == nil {
			// Not in the recent-block cache; historical read needs the block_log
			// reader wired to the API (follow-up).
			return map[string]interface{}{"header": nil}, nil
		}
		sh, _, err := block.UnmarshalSignedHeader(raw)
		if err != nil {
			return nil, jsonrpc.Errorf(jsonrpc.CodeServerError, "decode block %d: %v", n, err)
		}
		return map[string]interface{}{"header": blockHeaderResult(sh)}, nil
	}
}
