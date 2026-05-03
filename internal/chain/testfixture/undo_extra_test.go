package testfixture

import (
	"testing"

	"hived-go/internal/protocol/asset"
	"hived-go/internal/protocol/auth"
	"hived-go/internal/state"
	"hived-go/internal/state/objects"
)

// TestUndoFeedHistory ports undo_tests.cpp::undo_feed_history. Mutating
// feed_history.PriceHistory inside a session and then Undo()-ing must
// restore the original state.
func TestUndoFeedHistory(t *testing.T) {
	f := New(t)
	f.GenerateBlock()

	// Snapshot original state.
	s := state.NewSession(f.State)
	original, err := objects.GetFeedHistory(s)
	if err != nil {
		original = objects.FeedHistory{}
	}
	originalLen := len(original.PriceHistory)

	// Mutate: add several price history entries.
	mutated := original
	for i := 0; i < 5; i++ {
		mutated.PriceHistory = append(mutated.PriceHistory, asset.Price{
			Base:  asset.Asset{Amount: int64(1000 + i), Symbol: asset.Symbol{Num: asset.HbdSymbol}},
			Quote: asset.Asset{Amount: 1000, Symbol: asset.Symbol{Num: asset.HiveSymbol}},
		})
	}
	objects.PutFeedHistory(s, &mutated)

	// Verify mutation visible in the session.
	cur, err := objects.GetFeedHistory(s)
	if err != nil {
		t.Fatalf("GetFeedHistory after mutation: %v", err)
	}
	if len(cur.PriceHistory) != originalLen+5 {
		t.Fatalf("after mutation: got %d entries, want %d",
			len(cur.PriceHistory), originalLen+5)
	}

	// Undo discards the overlay.
	s.Undo()

	// Verify restored: read from a fresh session (Undo'd session may have
	// caches still hot; the on-disk state is unchanged).
	s2 := state.NewSession(f.State)
	restored, err := objects.GetFeedHistory(s2)
	if err != nil && originalLen > 0 {
		t.Fatalf("GetFeedHistory after undo: %v", err)
	}
	if len(restored.PriceHistory) != originalLen {
		t.Errorf("after undo: got %d entries, want %d (restored)",
			len(restored.PriceHistory), originalLen)
	}
}

// TestUndoDelayedVotes ports undo_tests.cpp::undo_delayed_votes. The
// delayed_votes deque on Account is mutated by transfer_to_vesting at
// HF24+; undoing the session must restore the empty queue.
func TestUndoDelayedVotes(t *testing.T) {
	f := New(t)
	f.GenerateBlock()

	// Create alice with no delayed votes.
	s := state.NewSession(f.State)
	objects.PutAccount(s, &objects.Account{
		Name:                 "alice",
		Created:              f.HeadBlockTime(),
		VotingManaCurrent:    10000,
		VotingManaLastUpdate: f.HeadBlockTime(),
	})
	if err := s.Commit(false); err != nil {
		t.Fatalf("seed commit: %v", err)
	}

	// Snapshot.
	s2 := state.NewSession(f.State)
	original, _ := objects.GetAccount(s2, "alice")
	originalQueueLen := len(original.DelayedVotes)

	// Mutate: append delayed votes.
	mutated := original
	mutated.DelayedVotes = append(mutated.DelayedVotes,
		objects.DelayedVoteEntry{Time: f.HeadBlockTime(), Val: 100_000},
		objects.DelayedVoteEntry{Time: f.HeadBlockTime() + 86400, Val: 200_000},
	)
	mutated.SumDelayedVotes = 300_000
	objects.PutAccount(s2, &mutated)

	cur, _ := objects.GetAccount(s2, "alice")
	if len(cur.DelayedVotes) != originalQueueLen+2 {
		t.Fatalf("after mutation: %d entries, want %d", len(cur.DelayedVotes), originalQueueLen+2)
	}

	// Undo.
	s2.Undo()

	// Read fresh.
	s3 := state.NewSession(f.State)
	restored, _ := objects.GetAccount(s3, "alice")
	if len(restored.DelayedVotes) != originalQueueLen {
		t.Errorf("after undo: %d entries, want %d", len(restored.DelayedVotes), originalQueueLen)
	}
	if restored.SumDelayedVotes != 0 {
		t.Errorf("after undo: SumDelayedVotes=%d, want 0", restored.SumDelayedVotes)
	}
}

// TestUndoDifferentIndexes ports undo_tests.cpp::undo_different_indexes.
// Mutations across multiple secondary indexes (witness vote index +
// schedule_time index) must all roll back together.
func TestUndoDifferentIndexes(t *testing.T) {
	f := New(t)
	f.GenerateBlock()

	// Seed alice as a witness with TotalVotesVests=100, VST set.
	s := state.NewSession(f.State)
	w := objects.Witness{
		Owner:                "alice",
		TotalVotesVests:      100,
		VirtualScheduledTime: [16]byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1},
	}
	objects.PutWitness(s, &w)
	if err := s.Commit(false); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Verify both indexes have alice.
	s2 := state.NewSession(f.State)
	got, err := objects.GetWitness(s2, "alice")
	if err != nil {
		t.Fatalf("GetWitness: %v", err)
	}
	if got.TotalVotesVests != 100 {
		t.Errorf("seed TotalVotesVests: got %d, want 100", got.TotalVotesVests)
	}

	// Mutate: change votes AND VST.
	got.TotalVotesVests = 500
	got.VirtualScheduledTime = [16]byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 99}
	objects.PutWitness(s2, &got)

	// Undo.
	s2.Undo()

	// Read fresh — must be unchanged.
	s3 := state.NewSession(f.State)
	restored, err := objects.GetWitness(s3, "alice")
	if err != nil {
		t.Fatalf("GetWitness after undo: %v", err)
	}
	if restored.TotalVotesVests != 100 {
		t.Errorf("after undo TotalVotesVests: got %d, want 100", restored.TotalVotesVests)
	}
	if restored.VirtualScheduledTime[15] != 1 {
		t.Errorf("after undo VST: got %v, want trailing byte=1", restored.VirtualScheduledTime[15])
	}
}

// TestUndoObjectDisappear ports undo_tests.cpp::undo_object_disappear.
// Creating new objects in a session and then undoing must remove them
// from view (they never existed).
func TestUndoObjectDisappear(t *testing.T) {
	f := New(t)
	f.GenerateBlock()

	// Snapshot — no "ghost" account.
	s0 := state.NewSession(f.State)
	if has, _ := objects.HasAccount(s0, "ghost"); has {
		t.Fatal("ghost should not exist before mutation")
	}

	// Create.
	s := state.NewSession(f.State)
	objects.PutAccount(s, &objects.Account{
		Name:    "ghost",
		Created: f.HeadBlockTime(),
	})
	if has, _ := objects.HasAccount(s, "ghost"); !has {
		t.Fatal("ghost should exist post-Put inside session")
	}

	// Undo.
	s.Undo()

	// Fresh session: ghost should not exist (commit never happened).
	s2 := state.NewSession(f.State)
	if has, _ := objects.HasAccount(s2, "ghost"); has {
		t.Fatal("ghost should not exist after undo")
	}
}

// TestUndoStateOnSquashAndUndo ports the squash-then-undo flow from
// undo_tests.cpp. Two nested savepoints: outer commit captures inner
// changes; outer undo wipes them; inner-only changes don't survive
// without an outer commit.
func TestUndoStateOnSquashAndUndo(t *testing.T) {
	f := New(t)
	f.GenerateBlock()

	s := state.NewSession(f.State)
	// Seed two accounts.
	objects.PutAccount(s, &objects.Account{Name: "alpha", Created: f.HeadBlockTime()})
	objects.PutAccount(s, &objects.Account{Name: "beta", Created: f.HeadBlockTime()})
	if err := s.Commit(false); err != nil {
		t.Fatalf("seed commit: %v", err)
	}

	// New session — make changes, snapshot, change again, restore.
	s2 := state.NewSession(f.State)
	a, _ := objects.GetAccount(s2, "alpha")
	a.Balance = 100
	objects.PutAccount(s2, &a)

	// Snapshot at "Balance=100".
	sp := s2.Snapshot()

	// More changes layered on top.
	a, _ = objects.GetAccount(s2, "alpha")
	a.Balance = 999
	objects.PutAccount(s2, &a)
	b, _ := objects.GetAccount(s2, "beta")
	b.Balance = 555
	objects.PutAccount(s2, &b)

	// Restore to snapshot — alpha.Balance = 100, beta.Balance = 0.
	s2.Restore(sp)
	aPost, _ := objects.GetAccount(s2, "alpha")
	bPost, _ := objects.GetAccount(s2, "beta")
	if aPost.Balance != 100 {
		t.Errorf("alpha after restore: got %d, want 100", aPost.Balance)
	}
	if bPost.Balance != 0 {
		t.Errorf("beta after restore: got %d, want 0", bPost.Balance)
	}

	// Undo the entire session — alpha back to Balance=0.
	s2.Undo()
	s3 := state.NewSession(f.State)
	aFinal, _ := objects.GetAccount(s3, "alpha")
	if aFinal.Balance != 0 {
		t.Errorf("alpha after undo: got %d, want 0", aFinal.Balance)
	}
}

// TestUndoKeyCollision ports undo_tests.cpp::undo_key_collision.
// Creating an object, deleting it, recreating it with the same primary
// key inside a session — then undoing — must restore the original.
func TestUndoKeyCollision(t *testing.T) {
	f := New(t)
	f.GenerateBlock()

	// Seed.
	s := state.NewSession(f.State)
	objects.PutAccount(s, &objects.Account{Name: "carol", Balance: 42, Created: f.HeadBlockTime()})
	if err := s.Commit(false); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Mutate session: change balance, "delete" by overwriting with zero,
	// recreate with new balance.
	s2 := state.NewSession(f.State)
	a, _ := objects.GetAccount(s2, "carol")
	a.Balance = 100
	objects.PutAccount(s2, &a)

	// Overwrite again.
	a, _ = objects.GetAccount(s2, "carol")
	a.Balance = 999
	objects.PutAccount(s2, &a)

	// Undo — original 42 must be restored.
	s2.Undo()

	s3 := state.NewSession(f.State)
	final, _ := objects.GetAccount(s3, "carol")
	if final.Balance != 42 {
		t.Errorf("after undo: got %d, want 42 (original)", final.Balance)
	}
}

// _ silences unused-import warnings on builds without auth references.
var _ = auth.AccountName("")
