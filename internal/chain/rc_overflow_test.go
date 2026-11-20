package chain

import (
	"errors"
	"testing"

	"gopherd/internal/protocol/auth"
	"gopherd/internal/state"
	"gopherd/internal/state/genesis"
	"gopherd/internal/state/objects"
)

// TestBlockHookRCOverflowFaultsSession pins hived's u_regen <= INT64_MAX
// assertion on automatic block processing. When an expired RC delegation is
// returned (rc_plugin handle_expired_delegations), the delegator's RC is
// regenerated uncapped; for 100 billion VESTS idle for 461 days that throws
// in hived and aborts apply_block. The hook has no error return, so it
// records the failure on the session, and ApplyBlock refuses a block that
// ends with a fault.
func TestBlockHookRCOverflowFaultsSession(t *testing.T) {
	db, err := state.Open(t.TempDir(), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := genesis.Apply(db); err != nil {
		t.Fatal(err)
	}
	const now = uint32(1_700_000_000)
	const idle = 39_844_968 // smallest idle time that overflows for 1e17

	for _, tc := range []struct {
		idle   uint32
		failed bool
	}{{idle - 1, false}, {idle, true}} {
		s := state.NewSession(db)
		whale := objects.Account{Name: "whale", VestingShares: 100_000_000_000_000_000, DelegatedRCOut: 5}
		whale.LastMaxRC = whale.MaximumRC()
		whale.RCManaLastUpdate = now - tc.idle
		objects.PutAccount(s, &whale)
		QueueRCDelegationExpiration(s, auth.AccountName("whale"), 5, now-rcDelegationReturnPeriodSec)

		rcHandleExpiredDelegations(s, now)

		if got := errors.Is(s.Fault(), objects.ErrManaRegenOverflow); got != tc.failed {
			t.Errorf("idle %d s: fault %v, want overflow %v", tc.idle, s.Fault(), tc.failed)
		}
		s.Undo()
		s.Close()
	}
}
