package objects

import "testing"

// Mirrors HIVE_* witness-split identities from hive/tests/unit/tests/
// config_tests.cpp's surrounding context (the upstream file is mostly
// logging-config; the portable HIVE_* parity checks live alongside the
// constants that hold the identities — split between
// internal/protocol/network and here to avoid a network→objects edge).
//
// Source: libraries/protocol/include/hive/protocol/config.hpp
//
//	#define HIVE_MAX_WITNESSES              21
//	#define HIVE_MAX_VOTED_WITNESSES_HF0    19
//	#define HIVE_MAX_MINER_WITNESSES_HF0    1
//	#define HIVE_MAX_RUNNER_WITNESSES_HF0   1
//	#define HIVE_MAX_VOTED_WITNESSES_HF17   20
//	#define HIVE_MAX_MINER_WITNESSES_HF17   0
//	#define HIVE_MAX_RUNNER_WITNESSES_HF17  1
//
// Together: voted + miner + runner == HIVE_MAX_WITNESSES at every HF.

// hf0/hf17 runner constants — kept as test-local literals because the
// runtime initialization sets MaxRunnerWitnesses=1 unconditionally in
// chain/witness_schedule.go (no exported constant to import). If a
// MaxRunnerWitnessesHFx constant is ever added to this package, replace
// the literals with it.
const (
	testMaxRunnerWitnessesHF0  uint8 = 1
	testMaxVotedWitnessesHF17  uint8 = 20
	testMaxMinerWitnessesHF17  uint8 = 0
	testMaxRunnerWitnessesHF17 uint8 = 1
)

// TestHiveWitnessSplitHF0 — pre-HF20 split sums to HIVE_MAX_WITNESSES (21).
func TestHiveWitnessSplitHF0(t *testing.T) {
	sum := uint8(MaxVotedWitnessesHF0) + uint8(MaxMinerWitnessesHF0) + testMaxRunnerWitnessesHF0
	if sum != MaxWitnesses {
		t.Fatalf("HF0 split %d+%d+%d = %d, want HIVE_MAX_WITNESSES = %d",
			MaxVotedWitnessesHF0, MaxMinerWitnessesHF0, testMaxRunnerWitnessesHF0,
			sum, MaxWitnesses)
	}
	if MaxVotedWitnessesHF0 != 19 {
		t.Fatalf("MaxVotedWitnessesHF0 = %d, want 19 (HIVE_MAX_VOTED_WITNESSES_HF0)",
			MaxVotedWitnessesHF0)
	}
	if MaxMinerWitnessesHF0 != 1 {
		t.Fatalf("MaxMinerWitnessesHF0 = %d, want 1 (HIVE_MAX_MINER_WITNESSES_HF0)",
			MaxMinerWitnessesHF0)
	}
}

// TestHiveWitnessSplitHF17 — HF17+ split (no miners) still sums to 21.
func TestHiveWitnessSplitHF17(t *testing.T) {
	sum := testMaxVotedWitnessesHF17 + testMaxMinerWitnessesHF17 + testMaxRunnerWitnessesHF17
	if sum != MaxWitnesses {
		t.Fatalf("HF17 split %d+%d+%d = %d, want HIVE_MAX_WITNESSES = %d",
			testMaxVotedWitnessesHF17, testMaxMinerWitnessesHF17, testMaxRunnerWitnessesHF17,
			sum, MaxWitnesses)
	}
	if MaxVotedWitnessesHF20 != testMaxVotedWitnessesHF17 {
		t.Fatalf("MaxVotedWitnessesHF20 = %d, want %d (HIVE_MAX_VOTED_WITNESSES_HF17)",
			MaxVotedWitnessesHF20, testMaxVotedWitnessesHF17)
	}
}

// TestHiveMaxWitnessesIs21 — pins the local copy of HIVE_MAX_WITNESSES.
// The network package holds the canonical constant and pins it to 21 in
// its own _cpp_parity test; we re-pin here so a drift between the two
// copies of the constant is caught immediately.
func TestHiveMaxWitnessesIs21(t *testing.T) {
	if MaxWitnesses != 21 {
		t.Fatalf("objects.MaxWitnesses = %d, want 21 (HIVE_MAX_WITNESSES)", MaxWitnesses)
	}
}
