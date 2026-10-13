package chain

import (
	"testing"

	"gopherd/internal/state"
	"gopherd/internal/state/objects"
)

// TestRCResourceParamsMatchHived pins the HF20 bootstrap to hived's generated
// parameters (mainnet rc_api.get_resource_params), and the startup alignment
// that corrects state written with the old approximated values.
func TestRCResourceParamsMatchHived(t *testing.T) {
	db, err := state.Open(t.TempDir(), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := state.NewSession(db)
	initRCBootstrapHF20(s, 1_600_000_000)

	rp, err := objects.GetRCResourceParam(s)
	if err != nil {
		t.Fatal(err)
	}
	exec := rp.Entries[objects.RCExecutionTime]
	if exec.Dynamics.DecayPerTime != 2539677724 || exec.Dynamics.DecayDenomShift != 42 ||
		exec.Dynamics.PoolEq != 69256028844 || exec.Dynamics.MaxPool != 138512057687 ||
		exec.Curve.CoeffA != 14969827235074865152 || exec.Curve.CoeffB != 541062725 {
		t.Fatalf("execution_time params %+v differ from hived's", exec)
	}
	pool, err := objects.GetRCPool(s)
	if err != nil {
		t.Fatal(err)
	}
	if pool.PoolArray[objects.RCStateBytes] != 27139923979692 {
		t.Fatalf("state_bytes pool seeded at %d, want hived's pool_eq 27139923979692", pool.PoolArray[objects.RCStateBytes])
	}

	// State from an older gopherd: the approximated execution_time entry.
	old := rp
	old.Entries[objects.RCExecutionTime].Dynamics.DecayPerTime = 39722240
	old.Entries[objects.RCExecutionTime].Dynamics.DecayDenomShift = 36
	old.Entries[objects.RCExecutionTime].Dynamics.MaxPool = 69200001547
	newAccounts := old.Entries[objects.RCNewAccounts]
	objects.PutRCResourceParam(s, &old)
	alignRCResourceParams(s)
	got, _ := objects.GetRCResourceParam(s)
	if got.Entries[objects.RCExecutionTime] != hivedRCResourceParams[objects.RCExecutionTime] {
		t.Fatalf("alignment left execution_time at %+v", got.Entries[objects.RCExecutionTime])
	}
	if got.Entries[objects.RCNewAccounts] != newAccounts {
		t.Fatal("alignment touched new_accounts, which set_pool_params owns")
	}
}
