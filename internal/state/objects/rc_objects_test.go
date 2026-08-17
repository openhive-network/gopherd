package objects

import (
	"testing"

	"gopherd/internal/state"
)

// TestRCResourceParamRoundTrip exercises the singleton encoder/decoder
// for the rc_resource_param_object — 5 per-resource entries of dynamics
// + price curve.
func TestRCResourceParamRoundTrip(t *testing.T) {
	original := RCResourceParam{}
	for i := 0; i < RCNumResources; i++ {
		original.Entries[i] = RCResourceEntry{
			Dynamics: ResourceDynamics{
				ResourceUnit:    uint64(1<<32 + i),
				BudgetPerTime:   int32(10000 + i),
				PoolEq:          uint64(1<<48 + i),
				MaxPool:         uint64(1<<50 + i),
				DecayPerTime:    uint32(347321 + i),
				DecayDenomShift: uint8(36 + i),
				MinDecay:        int64(7 + i),
			},
			Curve: PriceCurve{
				CoeffA: uint64(1<<40 + i),
				CoeffB: uint64(1<<30 + i),
				Shift:  uint8(8 + i),
			},
		}
	}
	encoded := AppendRCResourceParam(nil, &original)
	decoded, err := ReadRCResourceParam(encoded)
	if err != nil {
		t.Fatalf("ReadRCResourceParam: %v", err)
	}
	for i := 0; i < RCNumResources; i++ {
		if decoded.Entries[i] != original.Entries[i] {
			t.Errorf("entry[%d]: got %+v, want %+v", i, decoded.Entries[i], original.Entries[i])
		}
	}

	// Persistence — store + reload via the singleton key.
	db, err := state.Open(t.TempDir(), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := state.NewSession(db)
	if HasRCResourceParam(s) {
		t.Fatal("HasRCResourceParam true on fresh DB")
	}
	PutRCResourceParam(s, &original)
	if err := s.Commit(false); err != nil {
		t.Fatalf("commit: %v", err)
	}
	s = state.NewSession(db)
	if !HasRCResourceParam(s) {
		t.Fatal("HasRCResourceParam false after Put")
	}
	got, err := GetRCResourceParam(s)
	if err != nil {
		t.Fatalf("GetRCResourceParam: %v", err)
	}
	for i := 0; i < RCNumResources; i++ {
		if got.Entries[i] != original.Entries[i] {
			t.Errorf("persisted entry[%d]: got %+v, want %+v", i, got.Entries[i], original.Entries[i])
		}
	}
}

// TestRCPoolFullRoundTrip exercises the canonical (PoolArray + usage +
// budget + weights + sum + last_recalc_time + legacy) layout. The
// older legacy-only layout has its own coverage in rc_test.go.
func TestRCPoolFullRoundTrip(t *testing.T) {
	original := RCPool{
		SumOfResourceWeights: 31415,
		LastRecalcTime:       1_700_123_456,
	}
	for i := 0; i < RCNumResources; i++ {
		original.PoolArray[i] = int64(1_000_000 + i)
		original.UsageInWindow[i] = int64(2_000_000 + i)
		original.LastKnownBudget[i] = int64(3_000_000 + i)
		original.ResourceWeights[i] = uint16(100 + i)
		original.Pool[i] = RCPoolEntry{
			Current: int64(40 + i), Max: int64(50 + i),
			Update: uint32(60 + i), Cost: int64(70 + i),
		}
	}
	encoded := AppendRCPool(nil, &original)
	decoded, err := ReadRCPool(encoded)
	if err != nil {
		t.Fatalf("ReadRCPool: %v", err)
	}
	for i := 0; i < RCNumResources; i++ {
		if decoded.PoolArray[i] != original.PoolArray[i] {
			t.Errorf("pool_array[%d]: got %d want %d", i, decoded.PoolArray[i], original.PoolArray[i])
		}
		if decoded.UsageInWindow[i] != original.UsageInWindow[i] {
			t.Errorf("usage_in_window[%d]: got %d want %d", i, decoded.UsageInWindow[i], original.UsageInWindow[i])
		}
		if decoded.LastKnownBudget[i] != original.LastKnownBudget[i] {
			t.Errorf("last_known_budget[%d]: got %d want %d", i, decoded.LastKnownBudget[i], original.LastKnownBudget[i])
		}
		if decoded.ResourceWeights[i] != original.ResourceWeights[i] {
			t.Errorf("resource_weights[%d]: got %d want %d", i, decoded.ResourceWeights[i], original.ResourceWeights[i])
		}
		if decoded.Pool[i] != original.Pool[i] {
			t.Errorf("legacy pool[%d]: got %+v want %+v", i, decoded.Pool[i], original.Pool[i])
		}
	}
	if decoded.SumOfResourceWeights != original.SumOfResourceWeights {
		t.Errorf("sum_of_resource_weights: got %d want %d",
			decoded.SumOfResourceWeights, original.SumOfResourceWeights)
	}
	if decoded.LastRecalcTime != original.LastRecalcTime {
		t.Errorf("last_recalc_time: got %d want %d",
			decoded.LastRecalcTime, original.LastRecalcTime)
	}
}

// TestRCStatsRoundTrip exercises the rc_stats_object encoder/decoder
// (both pending and archive instances share the same wire format).
func TestRCStatsRoundTrip(t *testing.T) {
	original := RCStats{
		BlockNum: 26_000_000,
		Regen:    1_234_567_890,
	}
	for i := 0; i < 32; i++ {
		original.Stamp[i] = byte(i*7 + 3)
	}
	for i := 0; i < RCNumResources; i++ {
		original.Budget[i] = int64(1<<30 + i)
		original.Pool[i] = int64(1<<40 + i)
		original.Share[i] = uint16(1000 + i)
	}
	// Set distinctive values on a few op-stat / payer-stat slots so
	// we exercise the trailing array indices.
	original.OpStats[0] = RCOpStats{
		Usage: [RCNumResources]int64{1, 2, 3, 4, 5},
		Cost:  [RCNumResources]int64{10, 20, 30, 40, 50},
		Count: 7,
	}
	original.OpStats[RCNumOperations] = RCOpStats{
		Usage: [RCNumResources]int64{9, 9, 9, 9, 9},
		Cost:  [RCNumResources]int64{99, 99, 99, 99, 99},
		Count: 123,
	}
	original.PayerStats[0] = RCPayerStats{
		Usage:             [RCNumResources]int64{11, 12, 13, 14, 15},
		Cost:              [RCNumResources]int64{21, 22, 23, 24, 25},
		LessThan5Percent:  5,
		LessThan10Percent: 10,
		LessThan20Percent: 20,
		CantAfford:        [3]uint32{1, 2, 3},
		Count:             100,
	}
	original.PayerStats[RCNumPayerRanks-1] = RCPayerStats{Count: 1}
	original.AverageCost = [3]int64{100, 200, 300}

	encoded := AppendRCStats(nil, &original)
	decoded, err := ReadRCStats(encoded)
	if err != nil {
		t.Fatalf("ReadRCStats: %v", err)
	}
	if decoded.BlockNum != original.BlockNum {
		t.Errorf("block_num: got %d want %d", decoded.BlockNum, original.BlockNum)
	}
	if decoded.Regen != original.Regen {
		t.Errorf("regen: got %d want %d", decoded.Regen, original.Regen)
	}
	if decoded.Stamp != original.Stamp {
		t.Errorf("stamp round-trip mismatch")
	}
	for i := 0; i < RCNumResources; i++ {
		if decoded.Budget[i] != original.Budget[i] {
			t.Errorf("budget[%d]: %d vs %d", i, decoded.Budget[i], original.Budget[i])
		}
		if decoded.Pool[i] != original.Pool[i] {
			t.Errorf("pool[%d]: %d vs %d", i, decoded.Pool[i], original.Pool[i])
		}
		if decoded.Share[i] != original.Share[i] {
			t.Errorf("share[%d]: %d vs %d", i, decoded.Share[i], original.Share[i])
		}
	}
	if decoded.OpStats[0] != original.OpStats[0] {
		t.Errorf("op_stats[0]: %+v vs %+v", decoded.OpStats[0], original.OpStats[0])
	}
	if decoded.OpStats[RCNumOperations] != original.OpStats[RCNumOperations] {
		t.Errorf("op_stats[NUM]: %+v vs %+v",
			decoded.OpStats[RCNumOperations], original.OpStats[RCNumOperations])
	}
	if decoded.PayerStats[0] != original.PayerStats[0] {
		t.Errorf("payer_stats[0]: %+v vs %+v",
			decoded.PayerStats[0], original.PayerStats[0])
	}
	if decoded.AverageCost != original.AverageCost {
		t.Errorf("average_cost: %v vs %v", decoded.AverageCost, original.AverageCost)
	}

	// Persistence — two-slot dual-instance table.
	db, err := state.Open(t.TempDir(), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := state.NewSession(db)
	if HasRCStats(s, RCStatsIDPending) || HasRCStats(s, RCStatsIDArchive) {
		t.Fatal("RC stats rows exist on fresh DB")
	}
	PutRCStats(s, RCStatsIDPending, &original)
	archive := original
	archive.BlockNum = 25_000_000 // distinct value to confirm two slots
	PutRCStats(s, RCStatsIDArchive, &archive)
	if err := s.Commit(false); err != nil {
		t.Fatalf("commit: %v", err)
	}
	s = state.NewSession(db)
	if !HasRCStats(s, RCStatsIDPending) || !HasRCStats(s, RCStatsIDArchive) {
		t.Fatal("rc stats rows missing post-put")
	}
	gotPending, err := GetRCStats(s, RCStatsIDPending)
	if err != nil {
		t.Fatalf("GetRCStats(pending): %v", err)
	}
	if gotPending.BlockNum != original.BlockNum {
		t.Errorf("pending.block_num: got %d want %d", gotPending.BlockNum, original.BlockNum)
	}
	gotArchive, err := GetRCStats(s, RCStatsIDArchive)
	if err != nil {
		t.Fatalf("GetRCStats(archive): %v", err)
	}
	if gotArchive.BlockNum != archive.BlockNum {
		t.Errorf("archive.block_num: got %d want %d", gotArchive.BlockNum, archive.BlockNum)
	}
}

// TestRCDirectDelegationFullIndex confirms the by_to secondary index
// gets populated by PutRCDirectDelegationFull.
func TestRCDirectDelegationFullIndex(t *testing.T) {
	db, err := state.Open(t.TempDir(), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := state.NewSession(db)

	d := RCDirectDelegation{From: "alice", To: "bob", DelegatedRC: 12345}
	PutRCDirectDelegationFull(s, &d)
	if err := s.Commit(false); err != nil {
		t.Fatalf("commit: %v", err)
	}

	s = state.NewSession(db)
	got, err := GetRCDirectDelegation(s, "alice", "bob")
	if err != nil {
		t.Fatalf("Get primary: %v", err)
	}
	if got != d {
		t.Errorf("primary: got %+v want %+v", got, d)
	}

	// Confirm the by_to index has the (bob, alice) entry.
	wantByToKey := rcDirectDelegationByToKey("bob", "alice")
	has, err := s.Has(RCDirectDelegationTable, RCDirectDelegationByToIndex, wantByToKey)
	if err != nil {
		t.Fatalf("Has by_to: %v", err)
	}
	if !has {
		t.Errorf("by_to index missing (bob, alice)")
	}

	DeleteRCDirectDelegationFull(s, "alice", "bob")
	has, _ = s.Has(RCDirectDelegationTable, RCDirectDelegationByToIndex, wantByToKey)
	if has {
		t.Errorf("by_to index still present after full delete")
	}
}

// TestRCExpiredDelegationRoundTrip exercises the encoder/decoder + the
// single-row-per-delegator constraint (primary key = from).
func TestRCExpiredDelegationRoundTrip(t *testing.T) {
	original := RCExpiredDelegation{
		From:             "carol",
		RemainingExpired: 9_876_543_210,
		ExpiryTime:       1_700_999_888,
	}
	encoded := AppendRCExpiredDelegation(nil, &original)
	decoded, err := ReadRCExpiredDelegation(encoded)
	if err != nil {
		t.Fatalf("ReadRCExpiredDelegation: %v", err)
	}
	if decoded != original {
		t.Errorf("round-trip: got %+v want %+v", decoded, original)
	}

	db, err := state.Open(t.TempDir(), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := state.NewSession(db)
	if HasRCExpiredDelegation(s, "carol") {
		t.Fatal("HasRCExpiredDelegation true on fresh DB")
	}
	PutRCExpiredDelegation(s, &original)
	if err := s.Commit(false); err != nil {
		t.Fatalf("commit: %v", err)
	}
	s = state.NewSession(db)
	if !HasRCExpiredDelegation(s, "carol") {
		t.Fatal("HasRCExpiredDelegation false after Put")
	}
	got, err := GetRCExpiredDelegation(s, "carol")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != original {
		t.Errorf("persisted: got %+v want %+v", got, original)
	}
	DeleteRCExpiredDelegation(s, "carol")
	if HasRCExpiredDelegation(s, "carol") {
		t.Error("HasRCExpiredDelegation true after Delete")
	}
}

// TestRCUsageBucketRoundTrip exercises the bucket encoder/decoder + the
// by-seqno secondary index.
func TestRCUsageBucketRoundTrip(t *testing.T) {
	original := RCUsageBucket{
		BucketSeqno: 42,
		Created:     1_700_555_444,
		Used:        [RCNumResources]int64{111, 222, 333, 444, 555},
	}
	encoded := AppendRCUsageBucket(nil, &original)
	decoded, err := ReadRCUsageBucket(encoded)
	if err != nil {
		t.Fatalf("ReadRCUsageBucket: %v", err)
	}
	if decoded != original {
		t.Errorf("round-trip: got %+v want %+v", decoded, original)
	}

	db, err := state.Open(t.TempDir(), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := state.NewSession(db)
	PutRCUsageBucket(s, &original)
	if err := s.Commit(false); err != nil {
		t.Fatalf("commit: %v", err)
	}

	s = state.NewSession(db)
	got, err := GetRCUsageBucket(s, 42)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != original {
		t.Errorf("persisted: got %+v want %+v", got, original)
	}

	// By-seqno index entry should exist.
	has, err := s.Has(RCUsageBucketTable, RCUsageBucketBySeqnoIndex, rcUsageBucketKey(42))
	if err != nil {
		t.Fatalf("Has by_seqno: %v", err)
	}
	if !has {
		t.Error("by_seqno index missing")
	}

	DeleteRCUsageBucket(s, 42)
	has, _ = s.Has(RCUsageBucketTable, state.PrimaryIndex, rcUsageBucketKey(42))
	if has {
		t.Error("primary still present after delete")
	}
	has, _ = s.Has(RCUsageBucketTable, RCUsageBucketBySeqnoIndex, rcUsageBucketKey(42))
	if has {
		t.Error("by_seqno still present after delete")
	}
}
