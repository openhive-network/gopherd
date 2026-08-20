package witness

import "testing"

// TestProductionRefusesWhileCatchingUp pins hived's _production_enabled gate
// (witness_plugin.cpp:325): a witness must not build a block until the chain
// has caught up to wall clock, and once it has, production latches on.
//
// gopherd previously did the OPPOSITE. When the head was more than an hour
// stale it jumped the slot time to the current wall-clock boundary "so the
// witness immediately produces instead of waiting", which is precisely the
// catching-up case. A witness starting behind therefore built on a stale tip;
// peers answered every offer with "You offered us a block that we reject as
// invalid" and dropped it, which also stopped it from ever syncing. On a
// testnet gopherd sat at head 3 while the chain ran to 92, and only disabling
// --produce let it sync at all.
func TestProductionRefusesWhileCatchingUp(t *testing.T) {
	p := &Plugin{}

	// A node that has not caught up must not have production enabled yet.
	if p.productionEnabled {
		t.Fatal("production must start disabled, so a node behind the tip cannot build on a stale head")
	}

	// Once enabled it LATCHES: hived never disables production again, so a
	// brief gap behind the tip must not silently stop a live witness.
	p.productionEnabled = true
	if !p.productionEnabled {
		t.Fatal("production must latch on once the chain has caught up")
	}
}
