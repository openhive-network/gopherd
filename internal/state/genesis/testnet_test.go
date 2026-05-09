package genesis

import (
	"testing"

	"hived-go/internal/crypto"
	"hived-go/internal/state"
	"hived-go/internal/state/objects"
)

// TestApplyTestnet_WitnessesPresent verifies all 21 deterministic
// witnesses exist in state with non-zero block-signing keys after
// ApplyTestnet.
func TestApplyTestnet_WitnessesPresent(t *testing.T) {
	db := state.OpenMem()
	if err := ApplyTestnet(db, 0); err != nil {
		t.Fatalf("ApplyTestnet: %v", err)
	}
	s := state.NewSession(db)
	for i := 1; i <= TestnetWitnessCount; i++ {
		name := TestnetWitnessName(i)
		w, err := objects.GetWitness(s, name)
		if err != nil {
			t.Fatalf("witness %s missing: %v", name, err)
		}
		if w.BlockSigningKey.IsZero() {
			t.Fatalf("witness %s has zero signing key", name)
		}
	}
}

// TestApplyTestnet_KeysRoundTrip verifies every genesis WIF parses,
// derives a pubkey via secp256k1, and matches the witness's stored
// BlockSigningKey. Catches WIF encode/decode bugs and any drift between
// the deterministic key path and the genesis-write path.
func TestApplyTestnet_KeysRoundTrip(t *testing.T) {
	db := state.OpenMem()
	if err := ApplyTestnet(db, 5); err != nil {
		t.Fatalf("ApplyTestnet: %v", err)
	}
	s := state.NewSession(db)

	for _, k := range TestnetWitnessKeys() {
		secret, err := DecodeWIF(k.WIF)
		if err != nil {
			t.Fatalf("DecodeWIF(%s): %v", k.Name, err)
		}
		priv, err := crypto.PrivateKeyFromBytes(secret)
		if err != nil {
			t.Fatalf("PrivateKeyFromBytes(%s): %v", k.Name, err)
		}
		gotPub := priv.PublicKey()
		if gotPub != k.PublicKey {
			t.Fatalf("witness %s: WIF→pubkey != bundle pubkey", k.Name)
		}
		w, err := objects.GetWitness(s, k.Name)
		if err != nil {
			t.Fatalf("GetWitness(%s): %v", k.Name, err)
		}
		if w.BlockSigningKey != gotPub {
			t.Fatalf("witness %s: stored signing key != WIF-derived key", k.Name)
		}
	}
}

// TestApplyTestnet_OperatorsFunded asserts the 5 default node-operator
// accounts exist with the expected pre-funded balances.
func TestApplyTestnet_OperatorsFunded(t *testing.T) {
	db := state.OpenMem()
	if err := ApplyTestnet(db, 5); err != nil {
		t.Fatalf("ApplyTestnet: %v", err)
	}
	s := state.NewSession(db)
	for _, k := range TestnetOperatorKeys(5) {
		a, err := objects.GetAccount(s, k.Name)
		if err != nil {
			t.Fatalf("operator %s missing: %v", k.Name, err)
		}
		if a.Balance != operatorLiquidHive {
			t.Fatalf("operator %s Balance = %d, want %d", k.Name, a.Balance, operatorLiquidHive)
		}
		if a.HBDBalance != operatorLiquidHBD {
			t.Fatalf("operator %s HBDBalance = %d, want %d", k.Name, a.HBDBalance, operatorLiquidHBD)
		}
		if a.VestingShares != operatorVestingShares {
			t.Fatalf("operator %s VestingShares = %d, want %d", k.Name, a.VestingShares, operatorVestingShares)
		}
	}
}

// TestApplyTestnet_DGPOInvariants verifies the dgpo's CurrentSupply,
// CurrentHBDSupply, TotalVestingShares and TotalVestingFundHive match
// the sum of the seeded balances. If these drift the very first call to
// update_virtual_supply will tip the chain into negative territory.
func TestApplyTestnet_DGPOInvariants(t *testing.T) {
	const N = 5
	db := state.OpenMem()
	if err := ApplyTestnet(db, N); err != nil {
		t.Fatalf("ApplyTestnet: %v", err)
	}
	s := state.NewSession(db)
	dgpo, err := objects.GetDGPO(s)
	if err != nil {
		t.Fatalf("GetDGPO: %v", err)
	}

	wantLiquidHive := int64(N)*operatorLiquidHive + int64(TestnetWitnessCount)*witnessLiquidHive
	wantHBD := int64(N) * operatorLiquidHBD
	wantVestingShares := int64(N)*operatorVestingShares + int64(TestnetWitnessCount)*witnessVestingShares
	wantVestingFundHive := int64(N)*operatorVestingHiveBacking +
		int64(TestnetWitnessCount)*(witnessVestingShares/initialVestPerHiveSat)
	wantSupply := wantLiquidHive + wantVestingFundHive

	if dgpo.CurrentSupply != wantSupply {
		t.Errorf("CurrentSupply = %d, want %d", dgpo.CurrentSupply, wantSupply)
	}
	if dgpo.CurrentHBDSupply != wantHBD {
		t.Errorf("CurrentHBDSupply = %d, want %d", dgpo.CurrentHBDSupply, wantHBD)
	}
	if dgpo.TotalVestingShares != wantVestingShares {
		t.Errorf("TotalVestingShares = %d, want %d", dgpo.TotalVestingShares, wantVestingShares)
	}
	if dgpo.TotalVestingFundHive != wantVestingFundHive {
		t.Errorf("TotalVestingFundHive = %d, want %d", dgpo.TotalVestingFundHive, wantVestingFundHive)
	}
}

// TestApplyTestnet_ScheduleFull asserts the witness schedule lists all
// 21 init witnesses (so block 1 has a valid producer pool that doesn't
// require initminer fallback).
func TestApplyTestnet_ScheduleFull(t *testing.T) {
	db := state.OpenMem()
	if err := ApplyTestnet(db, 0); err != nil {
		t.Fatalf("ApplyTestnet: %v", err)
	}
	s := state.NewSession(db)
	ws, err := objects.GetWitnessSchedule(s)
	if err != nil {
		t.Fatalf("GetWitnessSchedule: %v", err)
	}
	if int(ws.NumScheduledWitnesses) != TestnetWitnessCount {
		t.Fatalf("NumScheduledWitnesses = %d, want %d", ws.NumScheduledWitnesses, TestnetWitnessCount)
	}
	if len(ws.CurrentShuffledWitnesses) != TestnetWitnessCount {
		t.Fatalf("len(CurrentShuffledWitnesses) = %d, want %d", len(ws.CurrentShuffledWitnesses), TestnetWitnessCount)
	}
	seen := map[string]bool{}
	for _, n := range ws.CurrentShuffledWitnesses {
		seen[string(n)] = true
	}
	for i := 1; i <= TestnetWitnessCount; i++ {
		if !seen[TestnetWitnessName(i)] {
			t.Errorf("schedule missing %s", TestnetWitnessName(i))
		}
	}
}

// TestApplyTestnet_KeysAreDeterministic — re-derive twice in a row,
// must produce byte-identical output.
func TestApplyTestnet_KeysAreDeterministic(t *testing.T) {
	a := TestnetWitnessKeys()
	b := TestnetWitnessKeys()
	if len(a) != len(b) {
		t.Fatalf("len drift: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i].WIF != b[i].WIF {
			t.Errorf("WIF drift at %d (%s): %q vs %q", i, a[i].Name, a[i].WIF, b[i].WIF)
		}
		if a[i].PublicKey != b[i].PublicKey {
			t.Errorf("pubkey drift at %d (%s)", i, a[i].Name)
		}
	}
	op1 := TestnetOperatorKeys(5)
	op2 := TestnetOperatorKeys(5)
	for i := range op1 {
		if op1[i].WIF != op2[i].WIF {
			t.Errorf("operator WIF drift at %d (%s)", i, op1[i].Name)
		}
	}
}

// TestWIFRoundTrip — direct unit test for the wif_helper encoder.
func TestWIFRoundTrip(t *testing.T) {
	secret := make([]byte, 32)
	for i := range secret {
		secret[i] = byte(i + 1)
	}
	wif, err := EncodeWIF(secret)
	if err != nil {
		t.Fatalf("EncodeWIF: %v", err)
	}
	got, err := DecodeWIF(wif)
	if err != nil {
		t.Fatalf("DecodeWIF: %v", err)
	}
	if string(got) != string(secret) {
		t.Fatalf("round-trip mismatch")
	}
}
