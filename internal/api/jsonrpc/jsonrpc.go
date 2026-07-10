// Package jsonrpc is a minimal JSON-RPC 2.0 server matching hived's request /
// response / error shapes, with a method registry keyed by "api.method".
//
// Methods receive their params as a raw json.RawMessage and decode it
// themselves — this is what lets one registry serve BOTH the modern named-object
// convention (database_api.find_accounts {"accounts":[...]}) and the legacy
// positional-array convention (condenser_api.get_accounts [["alice"]]).
package jsonrpc

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
)

// Standard JSON-RPC 2.0 error codes (hived uses these).
const (
	CodeParseError     = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternalError  = -32603
	CodeServerError    = -32000
)

// Request is a single JSON-RPC 2.0 request. id may be a number, string, or null;
// we keep it raw and echo it back verbatim.
type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// Response is a single JSON-RPC 2.0 response. Exactly one of Result / Error set.
type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  interface{}     `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}

// Error is a JSON-RPC 2.0 error object.
type Error struct {
	Code    int         `json:"code"`
	Message string      `json:"message"`
	Data    interface{} `json:"data,omitempty"`
}

func (e *Error) Error() string { return fmt.Sprintf("jsonrpc error %d: %s", e.Code, e.Message) }

// Errorf builds an *Error with a formatted message.
func Errorf(code int, format string, a ...interface{}) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, a...)}
}

// Handler implements one API method. params is the raw JSON params (object,
// array, or absent); the handler decodes what it expects. A returned error that
// is already an *Error is passed through verbatim; any other error becomes a
// CodeServerError.
type Handler func(ctx context.Context, params json.RawMessage) (interface{}, error)

// Registry maps "api.method" names to handlers.
type Registry struct {
	mu      sync.RWMutex
	methods map[string]Handler
}

func NewRegistry() *Registry { return &Registry{methods: make(map[string]Handler, 128)} }

// Register adds a handler. Panics on a duplicate name (programmer error).
func (r *Registry) Register(name string, h Handler) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.methods[name]; dup {
		panic("jsonrpc: duplicate method " + name)
	}
	r.methods[name] = h
}

// Alias registers an existing method under another name (e.g. bare "get_block"
// aliased to "block_api.get_block"). No-op if target is missing.
func (r *Registry) Alias(alias, target string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if h, ok := r.methods[target]; ok {
		r.methods[alias] = h
	}
}

// Names returns the registered method names (unsorted). For introspection.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.methods))
	for n := range r.methods {
		out = append(out, n)
	}
	return out
}

// Dispatch runs one request and returns its response. Handles the "call"
// convention too: {"method":"call","params":["api","method",<args>]} is
// rewritten to method "api.method" with <args> as params (hived legacy form).
func (r *Registry) Dispatch(ctx context.Context, req Request) Response {
	method := req.Method
	params := req.Params
	if method == "call" {
		if m, p, ok := unwrapCall(params); ok {
			method, params = m, p
		}
	}
	resp := Response{JSONRPC: "2.0", ID: req.ID}
	r.mu.RLock()
	h, ok := r.methods[method]
	r.mu.RUnlock()
	if !ok {
		resp.Error = Errorf(CodeMethodNotFound, "method %q not found", method)
		return resp
	}
	res, err := h(ctx, params)
	if err != nil {
		if e, isErr := err.(*Error); isErr {
			resp.Error = e
		} else {
			resp.Error = &Error{Code: CodeServerError, Message: err.Error()}
		}
		return resp
	}
	resp.Result = res
	return resp
}

// unwrapCall parses the legacy ["api","method",<args>] params form.
func unwrapCall(params json.RawMessage) (method string, args json.RawMessage, ok bool) {
	var arr []json.RawMessage
	if err := json.Unmarshal(params, &arr); err != nil || len(arr) < 2 {
		return "", nil, false
	}
	var api, m string
	if json.Unmarshal(arr[0], &api) != nil || json.Unmarshal(arr[1], &m) != nil {
		return "", nil, false
	}
	if len(arr) >= 3 {
		args = arr[2]
	}
	return api + "." + m, args, true
}
