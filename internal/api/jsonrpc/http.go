package jsonrpc

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
)

// maxBatchRequests caps the number of requests in one JSON-RPC batch. Each is
// dispatched sequentially and may hold the apply read lock, so an uncapped batch
// (bounded only by the 8 MiB body) is a work-amplification vector.
const maxBatchRequests = 500

// HTTPHandler serves the registry over HTTP JSON-RPC. POST a single request
// object or a batch array. Read-only APIs; GET/other methods are rejected.
func (r *Registry) HTTPHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPost {
			http.Error(w, "JSON-RPC endpoint accepts POST only", http.StatusMethodNotAllowed)
			return
		}
		body, err := io.ReadAll(io.LimitReader(req.Body, 8<<20)) // 8 MiB cap
		if err != nil {
			writeJSON(w, Response{JSONRPC: "2.0", Error: Errorf(CodeParseError, "read body: %v", err)})
			return
		}
		out := r.Handle(req.Context(), body)
		writeJSON(w, out)
	})
}

// Handle dispatches a raw JSON-RPC body (single object or batch array) and
// returns the value to marshal back (a Response or a []Response). Exposed so
// the WebSocket path and tests can reuse the same dispatch as HTTP.
func (r *Registry) Handle(ctx context.Context, body []byte) interface{} {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) > 0 && trimmed[0] == '[' {
		var batch []Request
		if err := json.Unmarshal(trimmed, &batch); err != nil {
			return Response{JSONRPC: "2.0", Error: Errorf(CodeParseError, "parse batch: %v", err)}
		}
		if len(batch) == 0 {
			return Response{JSONRPC: "2.0", Error: Errorf(CodeInvalidRequest, "empty batch")}
		}
		if len(batch) > maxBatchRequests {
			return Response{JSONRPC: "2.0", Error: Errorf(CodeInvalidRequest, "batch too large (%d > %d)", len(batch), maxBatchRequests)}
		}
		out := make([]Response, 0, len(batch))
		for i := range batch {
			out = append(out, r.Dispatch(ctx, batch[i]))
		}
		return out
	}
	var single Request
	if err := json.Unmarshal(trimmed, &single); err != nil {
		return Response{JSONRPC: "2.0", Error: Errorf(CodeParseError, "parse request: %v", err)}
	}
	return r.Dispatch(ctx, single)
}

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	_ = enc.Encode(v)
}
