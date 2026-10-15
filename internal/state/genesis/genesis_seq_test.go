package genesis

import (
	"testing"

	"gopherd/internal/protocol/auth"
	"gopherd/internal/state"
	"gopherd/internal/state/objects"
)

// TestGenesisAccountSeqFollowsHived pins init_genesis's creation order
// (database_init.cpp:197-245): miners, null, temp, initminer, steem.
func TestGenesisAccountSeqFollowsHived(t *testing.T) {
	db, err := state.Open(t.TempDir(), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := Apply(db); err != nil {
		t.Fatal(err)
	}
	s := state.NewSession(db)
	for i, n := range []string{"miners", "null", "temp", MainnetInitminer, "steem"} {
		a, err := objects.GetAccount(s, auth.AccountName(n))
		if err != nil {
			t.Fatal(err)
		}
		if a.Seq != uint64(i+1) {
			t.Errorf("%s seq=%d, want %d", n, a.Seq, i+1)
		}
	}
	if got := objects.AccountSeqCounter(s); got != 5 {
		t.Fatalf("counter=%d after genesis, want 5", got)
	}
}
