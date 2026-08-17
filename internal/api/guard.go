package api

import (
	"context"
	"encoding/json"

	"gopherd/internal/api/jsonrpc"
)

// expensiveUnboundedMethods are the read methods that scan a whole store under
// the ApplyBlock read lock with no secondary index — each is O(store size) per
// call, so a remote caller can stall block application (a DoS) when the API is
// bound to a non-loopback address. They stay enabled on loopback (trusted local
// use) and are refused on a public endpoint until a by-key/by-account index
// lands (see project_api_security_review / feedback_indexes_at_head_not_replay).
//
// Keep this list in sync with the registrations in account_by_key_api.go,
// lists_api.go and rc_api.go.
var expensiveUnboundedMethods = []string{
	// account_by_key: scans all ~1.2M accounts + a per-account authority walk.
	"account_by_key_api.get_key_references",
	"condenser_api.get_key_references",
	// lists_api: whole-table scans filtered by account in Go (no by-account index).
	"condenser_api.get_conversion_requests",
	"condenser_api.get_collateralized_conversion_requests",
	"condenser_api.get_vesting_delegations",
	"condenser_api.get_savings_withdraw_from",
	"condenser_api.get_savings_withdraw_to",
	"condenser_api.find_recurrent_transfers",
	"condenser_api.get_open_orders",
	"condenser_api.get_owner_history",
	// rc_api: whole-table scan (name-keyed table has no by-from index).
	"rc_api.list_rc_direct_delegations",
	"condenser_api.list_rc_direct_delegations",
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
