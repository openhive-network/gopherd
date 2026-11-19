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
	"runtime/debug"
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
	// panicHook, when set, is told about a handler that panicked before
	// Dispatch turns the panic into an error response.
	panicHook func(method string, p any, stack []byte)
}

// SetPanicHook installs fn to be called with the method, the panic value and
// the stack whenever a handler panics. Tests use it to fail on what Dispatch
// would otherwise answer as an internal error; a node can log it.
func (r *Registry) SetPanicHook(fn func(method string, p any, stack []byte)) {
	r.mu.Lock()
	r.panicHook = fn
	r.mu.Unlock()
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

// Replace overwrites an already-registered method's handler. Returns true if
// the name existed (and was replaced), false otherwise. Unlike Register it does
// not panic — used to swap in a guard (e.g. disabling expensive methods on a
// public endpoint) after the normal handlers are wired.
func (r *Registry) Replace(name string, h Handler) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.methods[name]; !ok {
		return false
	}
	r.methods[name] = h
	return true
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
	res, err := r.call(ctx, method, h, params)
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

// call runs h, turning a panic into an error. net/http would recover a
// panicking handler too, but only after unwinding through whatever it held:
// a read session left open or a mutex left locked would outlive the request.
// Recovering here answers the request with an internal error instead, and
// reports the panic through the hook, since it is always a bug.
func (r *Registry) call(ctx context.Context, method string, h Handler, params json.RawMessage) (res interface{}, err error) {
	defer func() {
		if p := recover(); p != nil {
			r.mu.RLock()
			hook := r.panicHook
			r.mu.RUnlock()
			if hook != nil {
				hook(method, p, debug.Stack())
			}
			res, err = nil, Errorf(CodeInternalError, "internal error in %s", method)
		}
	}()
	return h(ctx, params)
}
