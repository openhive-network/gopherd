package testfixture

import (
	"fmt"
	"testing"

	"gopherd/internal/protocol/auth"
	"gopherd/internal/state"
	"gopherd/internal/state/genesis"
	"gopherd/internal/state/objects"
)

// newBenchState builds a fresh state.Database with genesis applied,
// without using the testfixture (which calls t.Parallel/TempDir — both
// require a real *testing.T). Returns the db plus a function-cleanup
// hook the caller defers.
func newBenchState(b *testing.B) (*state.Database, uint32) {
	b.Helper()
	dir := b.TempDir()
	db, err := state.Open(dir, 16<<20)
	if err != nil {
		b.Fatalf("open state: %v", err)
	}
	b.Cleanup(func() { _ = db.Close() })
	if err := genesis.Apply(db); err != nil {
		b.Fatalf("genesis: %v", err)
	}
	// Read dgpo.Time to use as "now" for created timestamps.
	s := state.NewSession(db)
	dgpo, err := objects.GetDGPO(s)
	if err != nil {
		b.Fatalf("dgpo: %v", err)
	}
	return db, dgpo.Time
}

// This file ports the six undo_tests.cpp cases that were missing from
// the existing TestUndo* coverage:
//
//   - undo_generate_blocks
//   - undo_state_on_push_and_commit
//   - undo_state_on_squash_and_undo_with_keep_alive
//   - empty_undo_benchmark
//   - debug_update_undo_bug
//   - filled_undo_benchmark
//
// The C++ tests are written against chainbase's nested-undo-session
// stack (start_undo_session / session.push() / session.undo(keep_alive)
// / session.squash(keep_alive) / db->commit(rev) / db->undo_all()).
// Go's state.Session deliberately collapses that to a SINGLE overlay
// with per-tx Snapshot/Restore, because we own replay (monotonic
// forward) and the only reorg path we need today is per-tx rollback
// inside apply_block. Tests that exercise the nested-stack semantics
// have no faithful mapping; they are reported via t.Skip() with a
// reason. The rest are adapted to the overlay+savepoint API.

// ----------------------------------------------------------------------
// 1) undo_generate_blocks
//
// C++: pushes a comment_operation, generates a block, pop_block(),
// asserts the comment_index size is back to where it was. Tests that a
// fully-applied block can be rolled back at the block boundary.
//
// Go: there is no Chain.PopBlock — apply_block in Go writes through
// state.Session.Commit and that's that. The closest analog is wrapping
// the per-block apply in a Snapshot/Restore, but ApplyBlock takes its
// own session internally, so we can't intercept at this layer.
//
// We do, however, port the SPIRIT of the test directly against the
// Session overlay: stage account writes inside a session, "generate"
// the block by NOT committing, and verify Undo() returns the row count
// to its pre-state. The check<comment_index>() invariant in C++ is the
// same equality check on row count + key set.
func TestUndoGenerateBlocks(t *testing.T) {
	f := New(t)
	f.GenerateBlock()

	preSess := state.NewSession(f.State)
	preCount := 0
	_ = objects.RangeAccounts(preSess, func(_ *objects.Account) (bool, error) {
		preCount++
		return true, nil
	})

	// "Block N": create one comment-author account.
	s := state.NewSession(f.State)
	objects.PutAccount(s, &objects.Account{
		Name:    "bob",
		Created: f.HeadBlockTime(),
	})
	if err := s.Commit(false); err != nil {
		t.Fatalf("commit block N: %v", err)
	}
	committedCount := preCount + 1

	// "Block N+1": create another account, but DO NOT commit — simulate
	// pop_block().
	s2 := state.NewSession(f.State)
	objects.PutAccount(s2, &objects.Account{
		Name:    "alice",
		Created: f.HeadBlockTime(),
	})
	s2.Undo()

	// After Undo, fresh-session count must match the committed count
	// (just bob), not include alice.
	postSess := state.NewSession(f.State)
	postCount := 0
	_ = objects.RangeAccounts(postSess, func(_ *objects.Account) (bool, error) {
		postCount++
		return true, nil
	})
	if postCount != committedCount {
		t.Fatalf("after Undo of block N+1: got %d accounts, want %d", postCount, committedCount)
	}
	if has, _ := objects.HasAccount(postSess, "alice"); has {
		t.Fatalf("alice from rolled-back block should not exist")
	}
	if has, _ := objects.HasAccount(postSess, "bob"); !has {
		t.Fatalf("bob from committed block should still exist")
	}

	// Two ops in one block; same drill.
	s3 := state.NewSession(f.State)
	objects.PutAccount(s3, &objects.Account{
		Name:    "dan",
		Created: f.HeadBlockTime(),
	})
	objects.PutAccount(s3, &objects.Account{
		Name:    "chuck",
		Created: f.HeadBlockTime(),
	})
	s3.Undo()

	post2 := state.NewSession(f.State)
	postCount2 := 0
	_ = objects.RangeAccounts(post2, func(_ *objects.Account) (bool, error) {
		postCount2++
		return true, nil
	})
	if postCount2 != committedCount {
		t.Fatalf("after Undo of 2-op block: got %d accounts, want %d", postCount2, committedCount)
	}
}

// ----------------------------------------------------------------------
// 2) undo_state_on_push_and_commit
//
// C++ exercises chainbase's nested undo-session stack: open 22 sessions
// in sequence (.push() each), then commit() a middle revision and call
// undo_all(), and assert that exactly the right N reversible sessions
// roll back while the committed prefix stays. The test relies on
// db->revision() arithmetic and a stack of pushed sessions — neither
// concept exists in state.Session, which has ONE overlay and ONE
// optional savepoint chain via Snapshot/Restore.
//
// No faithful port is possible at the Session layer; the design
// equivalent in Go is "Commit writes through to the backend, Undo
// drops the overlay" — exercised everywhere already. We record the
// skip explicitly so the C++ test isn't silently ignored.
func TestUndoStateOnPushAndCommit(t *testing.T) {
	t.Skip("C++ test exercises chainbase's nested undo-session stack + " +
		"db->revision() / db->commit(rev) / db->undo_all(); Go's " +
		"state.Session has a single overlay with Snapshot/Restore — " +
		"no faithful mapping. Per-tx rollback is covered by " +
		"TestUndoStateOnSquashAndUndo + mid-block undo tests.")
}

// ----------------------------------------------------------------------
// 3) undo_state_on_squash_and_undo_with_keep_alive
//
// C++: opens a base session, makes changes, then calls undo(true) and
// squash(true) repeatedly to test "rewind in place but keep session
// alive" + "merge nested into parent but keep nested alive". Nested
// sessions inside the base test the same.
//
// Go equivalent:
//   - undo(true)  ≈ Restore(sp) followed by Snapshot() again
//   - squash(true) ≈ DiscardSavepoint(sp) followed by Snapshot() again
//
// We port the SCENARIO (create+modify+remove a layer, undo it, repeat,
// then squash it) using nested savepoints. Revision arithmetic is
// dropped since Go has no per-Session revision counter.
func TestUndoStateOnSquashAndUndoWithKeepAlive(t *testing.T) {
	f := New(t)
	f.GenerateBlock()

	// Pre-seed: 6 "modify" accounts + 6 "remove" accounts, persisted.
	const DEPTH = 6
	seed := state.NewSession(f.State)
	for i := 0; i < DEPTH; i++ {
		objects.PutAccount(seed, &objects.Account{
			Name:    auth.AccountName(fmt.Sprintf("mod%d", i)),
			Created: f.HeadBlockTime(),
			Balance: 100,
		})
		objects.PutAccount(seed, &objects.Account{
			Name:    auth.AccountName(fmt.Sprintf("rem%d", i)),
			Created: f.HeadBlockTime(),
			Balance: 0,
		})
	}
	if err := seed.Commit(false); err != nil {
		t.Fatalf("seed commit: %v", err)
	}

	// Open the base "session" — a single Session here, savepointed at
	// the start so we can rewind.
	s := state.NewSession(f.State)

	mutateLayer := func(idx int) {
		// "create" — new account.
		objects.PutAccount(s, &objects.Account{
			Name:    auth.AccountName(fmt.Sprintf("new%d", idx)),
			Created: f.HeadBlockTime(),
		})
		// "modify" — bump balance.
		modName := auth.AccountName(fmt.Sprintf("mod%d", idx))
		a, _ := objects.GetAccount(s, modName)
		a.Balance = int64(1000 + idx)
		objects.PutAccount(s, &a)
		// "remove".
		objects.DeleteAccount(s, auth.AccountName(fmt.Sprintf("rem%d", idx)))
	}

	checkLayerUndone := func(idx int) {
		// new<idx> must not exist.
		if has, _ := objects.HasAccount(s, auth.AccountName(fmt.Sprintf("new%d", idx))); has {
			t.Fatalf("new%d should be absent after undo", idx)
		}
		// mod<idx>.Balance must be the seeded 100.
		a, err := objects.GetAccount(s, auth.AccountName(fmt.Sprintf("mod%d", idx)))
		if err != nil {
			t.Fatalf("mod%d should still exist after undo: %v", idx, err)
		}
		if a.Balance != 100 {
			t.Fatalf("mod%d.Balance: got %d, want 100 (seeded)", idx, a.Balance)
		}
		// rem<idx> must be restored.
		if has, _ := objects.HasAccount(s, auth.AccountName(fmt.Sprintf("rem%d", idx))); !has {
			t.Fatalf("rem%d should be restored after undo", idx)
		}
	}

	checkLayerLive := func(idx int) {
		if has, _ := objects.HasAccount(s, auth.AccountName(fmt.Sprintf("new%d", idx))); !has {
			t.Fatalf("new%d should be present (layer survived)", idx)
		}
		a, _ := objects.GetAccount(s, auth.AccountName(fmt.Sprintf("mod%d", idx)))
		if a.Balance != int64(1000+idx) {
			t.Fatalf("mod%d.Balance: got %d, want %d (layer survived)", idx, a.Balance, 1000+idx)
		}
		if has, _ := objects.HasAccount(s, auth.AccountName(fmt.Sprintf("rem%d", idx))); has {
			t.Fatalf("rem%d should be absent (layer survived)", idx)
		}
	}

	// === Base savepoint (analog of base_session = db->start_undo_session()).
	baseSp := s.Snapshot()

	// Layer 0: mutate, then "undo(keep_alive)" = Restore + new Snapshot.
	mutateLayer(0)
	s.Restore(baseSp)
	checkLayerUndone(0)
	baseSp = s.Snapshot()

	// Layer 0 again, then undo(keep_alive) again. Confirms repeated
	// undo on the same base session is idempotent (matches "you can
	// undo with 'keep_alive' multiple times on the same base session").
	mutateLayer(0)
	s.Restore(baseSp)
	checkLayerUndone(0)
	baseSp = s.Snapshot()

	// Layer 0 a third time. Now squash(keep_alive): merge into the
	// effective "committed" state by discarding the savepoint, then
	// open a new one. The mutations remain in the overlay.
	mutateLayer(0)
	s.DiscardSavepoint(baseSp)
	baseSp = s.Snapshot()
	checkLayerLive(0)

	// Layer 1, squash again. Layer 0 + 1 are now both alive.
	mutateLayer(1)
	s.DiscardSavepoint(baseSp)
	baseSp = s.Snapshot()
	checkLayerLive(0)
	checkLayerLive(1)

	// Layer 2 in the base + open a nested savepoint and exercise undo/
	// squash on it.
	mutateLayer(2)

	// --- Nested savepoint.
	nestedSp := s.Snapshot()
	mutateLayer(3)
	// undo(keep_alive) on nested
	s.Restore(nestedSp)
	checkLayerUndone(3)
	nestedSp = s.Snapshot()

	mutateLayer(3)
	s.Restore(nestedSp)
	checkLayerUndone(3)
	nestedSp = s.Snapshot()

	// Layer 3 third time, squash(keep_alive) into base.
	mutateLayer(3)
	s.DiscardSavepoint(nestedSp)
	nestedSp = s.Snapshot()
	checkLayerLive(3)

	// Layer 4 in nested.
	mutateLayer(4)
	// Implicit-undo of nested: Restore.
	s.Restore(nestedSp)
	checkLayerUndone(4)
	// Layer 3 (squashed) survives.
	checkLayerLive(3)

	// --- End nested. Implicit-undo of BASE = Restore(baseSp).
	s.Restore(baseSp)

	// Layer 2 (live in base before nesting) must be gone.
	checkLayerUndone(2)
	// Layer 3 (squashed nested-into-base) must ALSO be gone — its
	// effects rolled into base, which we just undid.
	checkLayerUndone(3)
	// Layer 0 + 1 (squashed before nesting opened) must SURVIVE.
	checkLayerLive(0)
	checkLayerLive(1)
}

// ----------------------------------------------------------------------
// 4) empty_undo_benchmark
//
// C++ measures overhead of empty undo sessions during heavy traffic
// (10 blocks × 10000 incoming txs × per-tx session start/squash). The
// faithful Go analog is: per-tx Session-Snapshot + DiscardSavepoint
// (success path) or Session-Snapshot + Restore (failure path), nested
// inside a per-block Session.
//
// We benchmark BOTH the "fresh-session-per-tx" path (matches the
// original) and the keep-alive path (matches the second half of the
// C++ test). The benchmark is opportunistic — no t.Fatal asserts; it
// reports timing only.
func BenchmarkEmptyUndoSessions(b *testing.B) {
	const (
		NewTxBeforeBlock   = 10000
		InvalidTxFreq      = 5
		BlockCapacity      = 7000
		BlocksPerIteration = 10
	)
	db, _ := newBenchState(b)

	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		mempool := 0
		for blockNum := 1; blockNum <= BlocksPerIteration; blockNum++ {
			// Incoming "tx" loop.
			pendingSess := state.NewSession(db)
			for tx := 0; tx < NewTxBeforeBlock; tx++ {
				sp := pendingSess.Snapshot()
				if tx%InvalidTxFreq > 0 {
					// "valid" tx — discard savepoint = squash.
					pendingSess.DiscardSavepoint(sp)
					mempool++
				} else {
					// "invalid" tx — restore = undo.
					pendingSess.Restore(sp)
				}
			}
			if blockNum == 5 {
				// Block produced locally: drop pending, open new, apply
				// BlockCapacity txs as squashes.
				pendingSess.Undo()
				pendingSess = state.NewSession(db)
				for tx := 0; tx < BlockCapacity; tx++ {
					sp := pendingSess.Snapshot()
					pendingSess.DiscardSavepoint(sp)
				}
				pendingSess.Undo()
				pendingSess = state.NewSession(db)
			}
			// Apply block (just commit the pending session as the
			// "block_session.push()" analog).
			pendingSess.Undo()
			blockSess := state.NewSession(db)
			_ = blockSess.Commit(false)

			// Reapply mempool overflow.
			if mempool > BlockCapacity {
				overflow := mempool - BlockCapacity
				mempool -= BlockCapacity
				overflowSess := state.NewSession(db)
				for tx := 0; tx < overflow; tx++ {
					sp := overflowSess.Snapshot()
					overflowSess.DiscardSavepoint(sp)
				}
				overflowSess.Undo()
			} else {
				mempool = 0
			}
		}
	}
}

// BenchmarkEmptyUndoSessionsKeepAlive mirrors the second half of the
// C++ test that uses session.squash(true) / session.undo(true) on the
// SAME persistent session, instead of starting a new chainbase session
// per tx. The Go analog reuses ONE state.Session across the txs.
func BenchmarkEmptyUndoSessionsKeepAlive(b *testing.B) {
	const (
		NewTxBeforeBlock   = 10000
		InvalidTxFreq      = 5
		BlockCapacity      = 7000
		BlocksPerIteration = 10
	)
	db, _ := newBenchState(b)

	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		mempool := 0
		for blockNum := 1; blockNum <= BlocksPerIteration; blockNum++ {
			s := state.NewSession(db)
			sp := s.Snapshot()
			for tx := 0; tx < NewTxBeforeBlock; tx++ {
				if tx%InvalidTxFreq > 0 {
					s.DiscardSavepoint(sp)
					sp = s.Snapshot()
					mempool++
				} else {
					s.Restore(sp)
					sp = s.Snapshot()
				}
			}
			if blockNum == 5 {
				s.DiscardSavepoint(sp)
				s.Undo()
				s = state.NewSession(db)
				sp = s.Snapshot()
				for tx := 0; tx < BlockCapacity; tx++ {
					s.DiscardSavepoint(sp)
					sp = s.Snapshot()
				}
			}
			s.DiscardSavepoint(sp)
			s.Undo()
			blockSess := state.NewSession(db)
			_ = blockSess.Commit(false)
			if mempool > BlockCapacity {
				mempool -= BlockCapacity
			} else {
				mempool = 0
			}
		}
	}
}

// ----------------------------------------------------------------------
// 5) debug_update_undo_bug
//
// C++ test specifically targets the chainbase quirk: db_plugin->
// debug_update() modifies state while a block_session sits on the
// undo stack; the modification must persist past block boundaries
// without being undone. It exercises the debug_plugin code path which
// is NOT present in gopherd (no debug_plugin, no live mutation
// outside Session-bracketed apply_block path).
//
// Reporting the skip lets future ports come back to it if a Go
// debug_update equivalent is ever added.
func TestDebugUpdateUndoBug(t *testing.T) {
	t.Skip("requires db_plugin->debug_update equivalent in Go; not present " +
		"in gopherd. The C++ bug repro relies on debug_update modifying " +
		"state OUTSIDE a Session-bracketed apply_block path so the modification " +
		"survives subsequent block sessions on the undo stack. Go's state.Session " +
		"makes that path nonexistent by construction (no mutation outside an " +
		"open Session).")
}

// ----------------------------------------------------------------------
// 6) filled_undo_benchmark
//
// C++ benchmark measures cost of creating 30K big objects (accounts) +
// 30K small objects (comments) inside an undo session, with both
// undo() and push()+commit() teardown variants.
//
// Go port: NewSession + bulk PutAccount + Undo, then NewSession + bulk
// PutAccount + Commit. Comment-side mirrors with PutComment.
func BenchmarkFilledUndoCreateAccountsThenUndo(b *testing.B) {
	const NumObjects = 30000
	db, now := newBenchState(b)

	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		s := state.NewSession(db)
		for i := 0; i < NumObjects; i++ {
			objects.PutAccount(s, &objects.Account{
				Name:    auth.AccountName(fmt.Sprintf("a%d", i)),
				Created: now,
			})
		}
		s.Undo()
	}
}

func BenchmarkFilledUndoCreateAccountsThenCommit(b *testing.B) {
	const NumObjects = 30000

	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		b.StopTimer()
		db, now := newBenchState(b)
		b.StartTimer()

		s := state.NewSession(db)
		for i := 0; i < NumObjects; i++ {
			objects.PutAccount(s, &objects.Account{
				Name:    auth.AccountName(fmt.Sprintf("a%d", i)),
				Created: now,
			})
		}
		if err := s.Commit(false); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkFilledUndoModifyAccountsThenUndo(b *testing.B) {
	const NumObjects = 30000
	db, now := newBenchState(b)

	// Seed the accounts once outside the timer.
	seed := state.NewSession(db)
	for i := 0; i < NumObjects; i++ {
		objects.PutAccount(seed, &objects.Account{
			Name:    auth.AccountName(fmt.Sprintf("a%d", i)),
			Created: now,
		})
	}
	if err := seed.Commit(false); err != nil {
		b.Fatal(err)
	}

	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		s := state.NewSession(db)
		for i := 0; i < NumObjects; i++ {
			a, err := objects.GetAccount(s, auth.AccountName(fmt.Sprintf("a%d", i)))
			if err != nil {
				b.Fatalf("GetAccount a%d: %v", i, err)
			}
			a.Balance = int64(i + 1)
			objects.PutAccount(s, &a)
		}
		s.Undo()
	}
}

func BenchmarkFilledUndoCreateCommentsThenUndo(b *testing.B) {
	const NumObjects = 30000
	db, now := newBenchState(b)

	// Pre-create author accounts (commit once outside the timer).
	seed := state.NewSession(db)
	for i := 0; i < NumObjects; i++ {
		objects.PutAccount(seed, &objects.Account{
			Name:    auth.AccountName(fmt.Sprintf("a%d", i)),
			Created: now,
		})
	}
	if err := seed.Commit(false); err != nil {
		b.Fatal(err)
	}

	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		s := state.NewSession(db)
		for i := 0; i < NumObjects; i++ {
			objects.PutComment(s, &objects.Comment{
				Author:         auth.AccountName(fmt.Sprintf("a%d", i)),
				Permlink:       "test",
				ParentPermlink: "tag",
				Created:        now,
				RootAuthor:     auth.AccountName(fmt.Sprintf("a%d", i)),
				RootPermlink:   "test",
			})
		}
		s.Undo()
	}
}
