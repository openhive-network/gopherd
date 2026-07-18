package api

import (
	"strings"
	"testing"
)

// TestRestrictForPublicEndpointDisablesAllExpensive verifies the public-endpoint
// DoS guard: every listed unbounded-scan method is actually registered (so the
// list can't silently drift from the registrations) and, once restricted,
// refuses with a clear error — while an ordinary method keeps working.
func TestRestrictForPublicEndpointDisablesAllExpensive(t *testing.T) {
	srv := compatServer(t)

	// Sanity: before restricting, a guarded method is callable (no "disabled" error).
	if resp := callR(srv, "condenser_api.get_key_references", `[["STM1111111111111111111111111111111114T1Anm"]]`); resp.Error != nil && strings.Contains(resp.Error.Message, "disabled") {
		t.Fatalf("method disabled before RestrictForPublicEndpoint was called")
	}

	n := srv.RestrictForPublicEndpoint()
	if n != len(expensiveUnboundedMethods) {
		t.Fatalf("RestrictForPublicEndpoint disabled %d methods, want %d — the list has drifted from the registrations", n, len(expensiveUnboundedMethods))
	}

	// Every guarded method now refuses with the "disabled ... non-loopback" error.
	for _, m := range expensiveUnboundedMethods {
		resp := callR(srv, m, `[]`)
		if resp.Error == nil {
			t.Errorf("%s: expected an error after restriction, got result %v", m, resp.Result)
			continue
		}
		if !strings.Contains(resp.Error.Message, "disabled") || !strings.Contains(resp.Error.Message, "non-loopback") {
			t.Errorf("%s: error %q lacks the guard rationale", m, resp.Error.Message)
		}
	}

	// A non-guarded method still works.
	if resp := callR(srv, "condenser_api.get_dynamic_global_properties", `[]`); resp.Error != nil {
		t.Fatalf("non-guarded method should still work after restriction: %+v", resp.Error)
	}
}

// TestRestrictForPublicEndpointIdempotent: calling twice is safe (all still present).
func TestRestrictForPublicEndpointIdempotent(t *testing.T) {
	srv := compatServer(t)
	if a, b := srv.RestrictForPublicEndpoint(), srv.RestrictForPublicEndpoint(); a != b {
		t.Fatalf("not idempotent: first disabled %d, second %d", a, b)
	}
}
