//go:build testnet

package chain

import (
	"testing"

	"gopherd/internal/state"
	"gopherd/internal/state/genesis"
	"gopherd/internal/state/objects"
)

// TestTestnetWitnessPayNormalization pins the witness-pay normalization factor
// on a fresh testnet chain.
//
// process_funds multiplies the producer's reward by HIVE_MAX_WITNESSES and the
// producer's schedule weight, then divides by witness_pay_normalization_factor.
// That factor is elected_weight*num_elected + miner_weight*num_miners +
// timeshare_weight*num_timeshare, so if the genesis schedule leaves the WEIGHTS
// at zero the factor is zero too, gopherd's divide-by-zero guard falls back to
// the unweighted reward, and the witness is paid 1/21 of what hived pays.
//
// Measured against a live hived testnet before the fix: 2093 vs 6773 sat/block
// of inflation, while every block ID still matched — block IDs do not commit to
// state, so only a state comparison catches this.
//
// The factor is computed into the HF26+ FUTURE schedule at the first rebuild
// (block 21) and takes effect when that schedule is promoted (block 42), so the
// window here must cover a full promotion cycle.
func TestTestnetWitnessPayNormalization(t *testing.T) {
	db := state.OpenMem()
	if err := genesis.ApplyTestnet(db, nil); err != nil {
		t.Fatal(err)
	}
	s := state.NewSession(db)

	ws, err := objects.GetWitnessSchedule(s)
	if err != nil {
		t.Fatal(err)
	}
	if ws.ElectedWeight == 0 || ws.MinerWeight == 0 || ws.TimeshareWeight == 0 {
		t.Fatalf("genesis schedule weights must be seeded (hived's struct defaults 1/1/5), got elected=%d miner=%d timeshare=%d",
			ws.ElectedWeight, ws.MinerWeight, ws.TimeshareWeight)
	}

	var scratch witnessScheduleScratch
	for bn := uint32(1); bn <= 42; bn++ {
		updateWitnessSchedule(s, bn, &scratch)
	}
	ws, err = objects.GetWitnessSchedule(s)
	if err != nil {
		t.Fatal(err)
	}
	if got := ws.WitnessPayNormalizationFactor; got != 1 {
		t.Fatalf("witness_pay_normalization_factor = %d after a full promotion cycle, want 1 "+
			"(single elected witness at weight 1, matching hived); a 0 here underpays the producer 21x",
			got)
	}
}
