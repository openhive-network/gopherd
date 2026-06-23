package objects

import (
	"fmt"
	"math/rand"
	"testing"
)

const paReserve = 512 << 20 // 512 MiB sparse

// mkAccount builds a deterministic Account with varied fields (incl. a
// variable-size DelayedVotes queue) so the codec round-trip is meaningful.
func mkAccount(name string, seed int) Account {
	a := Account{
		Name:          name,
		Created:       uint32(1000000 + seed),
		Balance:       int64(seed) * 7,
		HBDBalance:    int64(seed) * 3,
		VestingShares: int64(seed) * 1000,
		RewardVests:   int64(seed) % 500,
	}
	if seed%4 == 0 {
		n := seed%3 + 1
		for i := 0; i < n; i++ {
			a.DelayedVotes = append(a.DelayedVotes, DelayedVoteEntry{
				Time: uint32(2000000 + seed + i), Val: uint64(seed*10 + i),
			})
			a.SumDelayedVotes += uint64(seed*10 + i)
		}
	}
	return a
}

func acctBytes(a Account) string { return string(AppendAccount(nil, &a)) }

func verifyAcct(t *testing.T, p *PersistAccountStore, ref map[string]Account, ctx string) {
	t.Helper()
	if p.Len() != len(ref) {
		t.Fatalf("%s: Len got %d want %d", ctx, p.Len(), len(ref))
	}
	for name, want := range ref {
		got, ok := p.Get(name)
		if !ok {
			t.Fatalf("%s: missing %q", ctx, name)
		}
		if acctBytes(got) != acctBytes(want) {
			t.Fatalf("%s: %q mismatch", ctx, name)
		}
	}
	// Ordered range must be ascending and cover exactly ref.
	var prev string
	seen := 0
	_ = p.RangePrimary(func(a *Account) (bool, error) {
		if seen > 0 && string(a.Name) <= prev {
			t.Fatalf("%s: RangePrimary not ascending at %q (prev %q)", ctx, a.Name, prev)
		}
		prev = string(a.Name)
		seen++
		if acctBytes(*a) != acctBytes(ref[string(a.Name)]) {
			t.Fatalf("%s: range %q mismatch", ctx, a.Name)
		}
		return true, nil
	})
	if seen != len(ref) {
		t.Fatalf("%s: range covered %d want %d", ctx, seen, len(ref))
	}
}

// TestPersistAccountStore_Equivalence_Undo_Restart: a persiststore-backed
// account store is a correct drop-in (matches a reference map over a random op
// mix using the REAL codec), supports tx rollback, and survives a restart with
// no replay.
func TestPersistAccountStore_Equivalence_Undo_Restart(t *testing.T) {
	dir := t.TempDir()
	p, err := OpenPersistAccountStore(dir, paReserve)
	if err != nil {
		t.Fatal(err)
	}
	ref := map[string]Account{}
	rng := rand.New(rand.NewSource(42))

	// Build a population with overwrites + deletes.
	for i := 0; i < 4000; i++ {
		name := fmt.Sprintf("acct-%05d", rng.Intn(1500))
		switch rng.Intn(10) {
		case 0:
			p.Delete(name)
			delete(ref, name)
		default:
			a := mkAccount(name, i)
			p.Put(&a)
			ref[name] = a
		}
	}
	verifyAcct(t, p, ref, "after random ops")

	// === transaction rollback ===
	p.CommitPoint()
	p.SetUndoEnabled(true)
	sp := p.SnapshotPoint()
	for i := 0; i < 800; i++ {
		name := fmt.Sprintf("acct-%05d", rng.Intn(1500))
		if rng.Intn(3) == 0 {
			p.Delete(name)
		} else {
			a := mkAccount(name, 90000+i)
			p.Put(&a)
		}
	}
	p.RestoreToPoint(sp)
	p.SetUndoEnabled(false)
	verifyAcct(t, p, ref, "after rollback")

	// === restart (the whole point) ===
	if err := p.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	p2, err := OpenPersistAccountStore(dir, paReserve)
	if err != nil {
		t.Fatal(err)
	}
	defer p2.Close()
	verifyAcct(t, p2, ref, "after restart (no replay)")
	t.Logf("OK: %d accounts persisted + restored without replay; dead ratio %.2f", p2.Len(), p2.DeadRatio())
}
