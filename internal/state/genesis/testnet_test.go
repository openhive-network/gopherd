//go:build testnet

package genesis

import (
	"sort"
	"testing"

	"gopherd/internal/protocol/auth"
	"gopherd/internal/state"
	"gopherd/internal/state/objects"
)

// The values below were read from a RUNNING hived testnet node
// (build_testnet, node_type "testnet") via database_api. They are the spec:
// if gopherd's genesis differs anywhere, the two implementations disagree at
// block 1 and can never share a chain.
const (
	hivedInitminerPubKey = "STM6LLegbAgLAy28EHrffBVuANFWcFgmqRMW13wBmTExqFE9SCkg4"
)

// hivedGenesisAccounts is exactly what list_accounts returns on a fresh hived
// testnet — including BOTH treasuries, which mainnet only creates at HF21/HF24.
var hivedGenesisAccounts = []string{
	"hive.fund", "initminer", "miners", "null", "steem", "steem.dao", "temp",
}

func applyForTest(t *testing.T, extra []string) *state.Session {
	t.Helper()
	db := state.OpenMem()
	if err := ApplyTestnet(db, extra); err != nil {
		t.Fatalf("ApplyTestnet: %v", err)
	}
	return state.NewSession(db)
}

// TestInitminerKeyMatchesHived pins the derivation of the testnet init key.
// hived builds it as regenerate(sha256("init_key")); if our derivation drifts,
// every block we sign is rejected and every block hived signs fails our check.
func TestInitminerKeyMatchesHived(t *testing.T) {
	priv, err := TestnetInitminerKey()
	if err != nil {
		t.Fatal(err)
	}
	if got := priv.PublicKey().EncodeBase58(); got != hivedInitminerPubKey {
		t.Fatalf("initminer pubkey = %s, want %s (hived's live value)", got, hivedInitminerPubKey)
	}
}

func TestGenesisAccountSetMatchesHived(t *testing.T) {
	s := applyForTest(t, nil)
	var got []string
	for _, name := range hivedGenesisAccounts {
		if _, err := objects.GetAccount(s, auth.AccountName(name)); err != nil {
			t.Errorf("account %q missing from genesis (hived has it)", name)
			continue
		}
		got = append(got, name)
	}
	sort.Strings(got)
	if len(got) != len(hivedGenesisAccounts) {
		t.Fatalf("genesis accounts = %v, want %v", got, hivedGenesisAccounts)
	}
}

// TestInitminerHoldsInitialSupply — hived issues the whole initial supply to
// initminer at genesis. Getting this wrong desynchronises every supply-derived
// value (virtual_supply, inflation, HBD ratio) from block 1 onward.
func TestInitminerHoldsInitialSupply(t *testing.T) {
	s := applyForTest(t, nil)
	a, err := objects.GetAccount(s, MainnetInitminer)
	if err != nil {
		t.Fatal(err)
	}
	if a.Balance != TestnetInitHiveSupply {
		t.Errorf("initminer HIVE = %d, want %d", a.Balance, TestnetInitHiveSupply)
	}
	if a.HBDBalance != TestnetInitHbdSupply {
		t.Errorf("initminer HBD = %d, want %d", a.HBDBalance, TestnetInitHbdSupply)
	}
	p, err := objects.GetDGPO(s)
	if err != nil {
		t.Fatal(err)
	}
	if p.CurrentSupply != TestnetInitHiveSupply || p.CurrentHBDSupply != TestnetInitHbdSupply {
		t.Errorf("dgpo supply = %d/%d, want %d/%d",
			p.CurrentSupply, p.CurrentHBDSupply, TestnetInitHiveSupply, TestnetInitHbdSupply)
	}
	if want := TestnetInitHiveSupply + TestnetInitHbdSupply; p.VirtualSupply != want {
		t.Errorf("virtual_supply = %d, want %d (1:1 feed at genesis)", p.VirtualSupply, want)
	}
	if p.CurrentWitness != MainnetInitminer {
		t.Errorf("current_witness = %q, want initminer", p.CurrentWitness)
	}
}

// TestTempAccountHasOpenAuthority guards a subtle one: hived gives "temp"
// authority(0) — threshold ZERO, satisfied by anything — while every other
// system account gets authority(1) with no keys. Seeding temp as locked would
// make us reject transactions the rest of the network accepts.
func TestTempAccountHasOpenAuthority(t *testing.T) {
	s := applyForTest(t, nil)
	tmp, err := objects.GetAccountAuthority(s, "temp")
	if err != nil {
		t.Fatal(err)
	}
	if tmp.Active.WeightThreshold != 0 {
		t.Errorf("temp active threshold = %d, want 0 (hived's open_auth)", tmp.Active.WeightThreshold)
	}
	nul, err := objects.GetAccountAuthority(s, "null")
	if err != nil {
		t.Fatal(err)
	}
	if nul.Active.WeightThreshold != 1 || len(nul.Active.KeyAuths) != 0 {
		t.Errorf("null active = threshold %d with %d keys, want hived's locked_auth(1) with none",
			nul.Active.WeightThreshold, len(nul.Active.KeyAuths))
	}
}

func TestGenesisWitnessMatchesHived(t *testing.T) {
	s := applyForTest(t, nil)
	w, err := objects.GetWitness(s, MainnetInitminer)
	if err != nil {
		t.Fatal(err)
	}
	if got := w.BlockSigningKey.EncodeBase58(); got != hivedInitminerPubKey {
		t.Errorf("initminer signing key = %s, want %s", got, hivedInitminerPubKey)
	}
	if w.Schedule != objects.WitnessScheduleMiner {
		t.Errorf("initminer schedule = %v, want miner", w.Schedule)
	}
	// hived's init_witness leaves `created` at its zero value; list_witnesses
	// reports 1970-01-01, not the genesis time.
	if w.Created != 0 {
		t.Errorf("witness created = %d, want 0 (hived leaves it unset)", w.Created)
	}
	sch, err := objects.GetWitnessSchedule(s)
	if err != nil {
		t.Fatal(err)
	}
	if sch.NumScheduledWitnesses != 1 || len(sch.CurrentShuffledWitnesses) != 1 ||
		sch.CurrentShuffledWitnesses[0] != auth.AccountName(MainnetInitminer) {
		t.Errorf("schedule = %v, want [initminer]", sch.CurrentShuffledWitnesses)
	}
}

// TestInitWitnessesAreRegistered covers hived's configurable init_witnesses:
// each becomes an account AND a witness sharing the init key.
func TestInitWitnessesAreRegistered(t *testing.T) {
	extra := []string{"wit-a", "wit-b"}
	s := applyForTest(t, extra)
	for _, n := range extra {
		if _, err := objects.GetAccount(s, auth.AccountName(n)); err != nil {
			t.Errorf("init witness %q has no account: %v", n, err)
		}
		w, err := objects.GetWitness(s, auth.AccountName(n))
		if err != nil {
			t.Errorf("init witness %q not registered: %v", n, err)
			continue
		}
		if got := w.BlockSigningKey.EncodeBase58(); got != hivedInitminerPubKey {
			t.Errorf("init witness %q signs with %s, want the shared init key", n, got)
		}
	}
	sch, err := objects.GetWitnessSchedule(s)
	if err != nil {
		t.Fatal(err)
	}
	if got := int(sch.NumScheduledWitnesses); got != 1+len(extra) {
		t.Errorf("scheduled witnesses = %d, want %d", got, 1+len(extra))
	}
}

// TestFeedHistorySeeded — testnet starts with a 1:1 price feed (mainnet has
// none until witnesses publish). virtual_supply depends on it immediately.
// TestGenesisScheduleDefaultsMatchHived pins witness_schedule_object's C++
// struct defaults (witness_objects.hpp:191-194). These are literals in hived,
// not computed values.
//
// witness_pay_normalization_factor is the one that bites: it divides the
// witness reward, which process_funds has already multiplied by
// HIVE_MAX_WITNESSES. Left at 0, gopherd's divide-by-zero guard pays the
// UNWEIGHTED slice — 234 sat at block 1 instead of 234*21/25 = 196. That 38-sat
// overpayment lands in the vesting fund, permanently shifting the VESTS price,
// and was the ENTIRE measured divergence from hived (+1558 sat by the time a
// large transfer_to_vesting had amplified it).
func TestGenesisScheduleDefaultsMatchHived(t *testing.T) {
	s := applyForTest(t, nil)
	ws, err := objects.GetWitnessSchedule(s)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name string
		got  uint32
		want uint32
	}{
		{"elected_weight", uint32(ws.ElectedWeight), 1},
		{"miner_weight", uint32(ws.MinerWeight), 1},
		{"timeshare_weight", uint32(ws.TimeshareWeight), 5},
		{"witness_pay_normalization_factor", ws.WitnessPayNormalizationFactor, 25},
	} {
		if c.got != c.want {
			t.Errorf("%s = %d, want hived's struct default %d", c.name, c.got, c.want)
		}
	}
}

func TestFeedHistorySeeded(t *testing.T) {
	s := applyForTest(t, nil)
	h, err := objects.GetFeedHistory(s)
	if err != nil {
		t.Fatalf("no feed history at genesis: %v", err)
	}
	if h.CurrentMedianHistory.Base.Amount != h.CurrentMedianHistory.Quote.Amount {
		t.Errorf("median feed = %v, want 1:1", h.CurrentMedianHistory)
	}
}
