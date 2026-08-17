package objects

import (
	"fmt"
	"testing"

	"gopherd/internal/protocol/auth"
)

// TestPersistTables_RestartAtHead is the phase-6 capstone: drive all tables like
// the node applying blocks, do a clean shutdown (FlushAll/CloseAll), then REOPEN
// — every table comes back at head with NO replay, and per-tx rollback fans
// across all tables atomically.
func TestPersistTables_RestartAtHead(t *testing.T) {
	dir := t.TempDir()
	const small = 256 << 20 // 256 MiB sparse per table (test-sized)
	tb, err := OpenPersistTablesWithReserves(dir, small, small, small, 1<<20)
	if err != nil {
		t.Fatal(err)
	}

	// "Apply blocks 1..1000": each touches an account, a comment, and a vote.
	for i := 1; i <= 1000; i++ {
		name := auth.AccountName(fmt.Sprintf("acc-%04d", i%300))
		a := mkAccount(string(name), i)
		tb.Accounts.Put(&a)
		c := mkComment(name, fmt.Sprintf("p-%04d", i%400), name, fmt.Sprintf("p-%04d", i%400), i)
		tb.Comments.Put(&c)
		v := mkVote(auth.AccountName(fmt.Sprintf("vt-%03d", i%200)), name, fmt.Sprintf("p-%04d", i%400), i)
		tb.Votes.Put(&v)
		tb.SetHeadBlock(uint32(i))
	}

	// A failed tx at "block 1001": savepoint across all tables, mutate, roll back.
	tb.SetUndoEnabledAll(true)
	sp := tb.SnapshotAll()
	for i := 0; i < 200; i++ {
		a := mkAccount(fmt.Sprintf("ghost-%03d", i), 999000+i)
		tb.Accounts.Put(&a)
	}
	tb.SetHeadBlock(1001)
	tb.RestoreAll(sp)
	tb.SetUndoEnabledAll(false)
	if _, ok := tb.Accounts.Get("ghost-000"); ok {
		t.Fatal("rollback failed: ghost account survived")
	}
	if h, _ := tb.HeadBlock(); h != 1000 {
		t.Fatalf("rollback failed: head got %d want 1000", h)
	}

	accN, comN, votN := tb.Accounts.Len(), tb.Comments.Len(), tb.Votes.Len()

	// === CLEAN SHUTDOWN ===
	if err := tb.CloseAll(); err != nil {
		t.Fatalf("CloseAll: %v", err)
	}

	// === RESTART: reopen — must resume at head 1000 with all state, no replay ===
	tb2, err := OpenPersistTablesWithReserves(dir, small, small, small, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer tb2.CloseAll()
	h, ok := tb2.HeadBlock()
	if !ok || h != 1000 {
		t.Fatalf("restart head: got %d (ok=%v) want 1000 → node would resume at 1001, no replay", h, ok)
	}
	if tb2.Accounts.Len() != accN || tb2.Comments.Len() != comN || tb2.Votes.Len() != votN {
		t.Fatalf("restart counts: acc %d/%d com %d/%d vot %d/%d",
			tb2.Accounts.Len(), accN, tb2.Comments.Len(), comN, tb2.Votes.Len(), votN)
	}
	// Spot-check a value survived intact.
	if a, ok := tb2.Accounts.Get("acc-0042"); !ok || acctBytes(a) == "" {
		t.Fatal("restart: acc-0042 missing")
	}
	t.Logf("RESTART AT HEAD ok: head=%d, %d accounts / %d comment-keys / %d vote-keys restored — NO REPLAY",
		h, tb2.Accounts.Len(), tb2.Comments.Len(), tb2.Votes.Len())
}
