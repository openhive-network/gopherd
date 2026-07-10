package api

import (
	"context"
	"encoding/hex"
	"encoding/json"

	"hived-go/internal/api/jsonrpc"
	"hived-go/internal/fcraw"
	"hived-go/internal/protocol/block"
	"hived-go/internal/protocol/tx"
)

func (s *Server) registerBlockAPI() {
	s.reg.Register("block_api.get_block_header", s.mGetBlockHeader())
	s.reg.Register("block_api.get_block", s.mGetBlock(false))
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

// txResult renders a signed_transaction with fully-expanded operations.
func txResult(st tx.SignedTransaction, legacy bool) map[string]interface{} {
	ops := make([]interface{}, 0, len(st.Operations))
	for _, o := range st.Operations {
		if legacy {
			ops = append(ops, opToJSON(o, true))
		} else {
			ops = append(ops, opToVariant(o, false))
		}
	}
	sigs := make([]string, 0, len(st.Signatures))
	for _, s := range st.Signatures {
		sigs = append(sigs, hex.EncodeToString(s[:]))
	}
	return map[string]interface{}{
		"ref_block_num":    st.RefBlockNum,
		"ref_block_prefix": st.RefBlockPrefix,
		"expiration":       timeString(st.Expiration),
		"operations":       ops,
		"extensions":       []interface{}{},
		"signatures":       sigs,
	}
}

// parseBlock decodes a raw signed_block into (header, transactions).
func parseBlock(raw []byte) (block.SignedHeader, []tx.SignedTransaction, int, error) {
	sh, hdrLen, err := block.UnmarshalSignedHeader(raw)
	if err != nil {
		return sh, nil, 0, err
	}
	r := fcraw.NewReader(raw[hdrLen:])
	count, err := r.ReadVarUint()
	if err != nil {
		return sh, nil, hdrLen, err
	}
	txs := make([]tx.SignedTransaction, 0, count)
	for i := uint32(0); i < count; i++ {
		res, err := tx.ReadSignedTransaction(r)
		if err != nil {
			return sh, txs, hdrLen, err
		}
		txs = append(txs, res.SignedTx)
	}
	return sh, txs, hdrLen, nil
}

func (s *Server) mGetBlock(legacy bool) jsonrpc.Handler {
	return func(_ context.Context, params json.RawMessage) (interface{}, error) {
		n, err := blockNumParam(params)
		if err != nil {
			return nil, err
		}
		raw := s.chain.RecentBlockBytes(n)
		if raw == nil {
			return map[string]interface{}{"block": nil}, nil
		}
		sh, txs, hdrLen, perr := parseBlock(raw)
		if perr != nil {
			return nil, jsonrpc.Errorf(jsonrpc.CodeServerError, "decode block %d: %v", n, perr)
		}
		txsOut := make([]interface{}, 0, len(txs))
		for i := range txs {
			txsOut = append(txsOut, txResult(txs[i], legacy))
		}
		blk := blockHeaderResult(sh)
		blk["witness_signature"] = hex.EncodeToString(sh.WitnessSignature[:])
		blk["transactions"] = txsOut
		bid := block.ConstructID(raw[:hdrLen], n)
		blk["block_id"] = hex.EncodeToString(bid[:])
		blk["transaction_ids"] = []interface{}{}
		if legacy {
			return blk, nil // condenser: bare block object
		}
		return map[string]interface{}{"block": blk}, nil
	}
}
