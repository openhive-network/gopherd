package api

import (
	"context"
	"encoding/json"

	"gopherd/internal/api/jsonrpc"
)

// expensiveUnboundedMethods are the read methods that still scan a whole store
// under the ApplyBlock read lock with no usable index — each is O(store size)
// per call, so a remote caller can stall block application (a DoS) when the API
// is bound to a non-loopback address. They stay enabled on loopback (trusted
// local use) and are refused on a public endpoint.
//
// Most of the original list has been retired: the account-filtered lookups now
// seek their account's key prefix, and get_key_references answers from the
// public-key index (see objects/key_lookup.go). What remains are the two cases
// where the account is genuinely not usable as a key prefix:
//
//   - get_open_orders — LimitOrderStore is a typed in-memory store keyed
//     (owner, order_id) in a Go MAP, which cannot be prefix-scanned. Needs a
//     by-owner index inside the store, which is on the hot trading path.
//
// Keep this list in sync with the registrations in lists_api.go.
var expensiveUnboundedMethods = []string{
	"condenser_api.get_open_orders",
}

// RestrictForPublicEndpoint disables the expensive unbounded-scan methods,
// replacing each with a handler that returns an error. Call this when the API
// is bound to a non-loopback address so a remote caller can't stall block
// application with an O(store) scan. Returns the number of methods disabled.
// Idempotent and safe to call before serving.
func (s *Server) RestrictForPublicEndpoint() int {
	n := 0
	for _, name := range expensiveUnboundedMethods {
		m := name
		refuse := func(_ context.Context, _ json.RawMessage) (interface{}, error) {
			return nil, jsonrpc.Errorf(jsonrpc.CodeServerError,
				"method %q is disabled on a non-loopback endpoint: it performs an "+
					"unbounded state scan (DoS risk) pending a secondary index; "+
					"bind the API to loopback to use it", m)
		}
		if s.reg.Replace(name, refuse) {
			n++
		}
	}
	return n
}
