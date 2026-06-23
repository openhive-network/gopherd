package objects

import (
	"fmt"
	"testing"

	"hived-go/internal/protocol/auth"
)

// TestPersistCold_RestartSurvival is the cutover proof: the cold tier backed by
// PersistCold comes back after a restart, where the default mmap backend (which
// O_TRUNCs on open) loses everything. This is the bulk of state (evicted
// comments/votes) surviving without replay.
func TestPersistCold_RestartSurvival(t *testing.T) {
	dir := t.TempDir()

	// Write a realistic mix: vote-primary (point-op), vote-by-comment (ordered
	// secondary), comment-primary, comment-by-root (ordered secondary).
	c1, err := OpenPersistCold(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{}
	for i := 0; i < 2000; i++ {
		author := auth.AccountName(fmt.Sprintf("auth%02d", i%40))
		permlink := fmt.Sprintf("post-%03d", i%200)
		voter := auth.AccountName(fmt.Sprintf("voter%03d", i%300))
		pk := appendColdVotePrimaryKey(nil, voter, author, permlink)
		bc := appendColdVoteByCommentKey(nil, author, permlink, voter)
		v := []byte(fmt.Sprintf("vote-blob-%d", i))
		c1.bset(pk, v)
		c1.bset(bc, v)
		want[string(pk)] = string(v)
		want[string(bc)] = string(v)
	}
	c1.flushBatch()
	if err := c1.Close(); err != nil { // clean shutdown
		t.Fatal(err)
	}

	// === RESTART ===
	c2, err := OpenPersistCold(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	for k, wv := range want {
		got, ok := c2.get([]byte(k))
		if !ok || string(got) != wv {
			t.Fatalf("restart: key gone/mismatch ok=%v got=%q want=%q", ok, got, wv)
		}
	}

	// Ordered by-comment prefix scan survives + is ascending.
	author := auth.AccountName("auth05")
	permlink := "post-005"
	// the by-comment scan prefix is table | AppendAccountName(author) | permlink | 0
	scanPrefix := append([]byte{coldTableVoteByComment}, CommentVoteByCommentPrefix(author, permlink)...)
	n := 0
	var prev string
	if err := c2.rangePrefix(scanPrefix, func(k, v []byte) bool {
		if n > 0 && string(k) <= prev {
			t.Fatalf("by-comment scan not ascending: %q <= %q", k, prev)
		}
		prev = string(k)
		n++
		return true
	}); err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Fatal("by-comment scan returned nothing after restart")
	}
	t.Logf("RESTART ok: %d cold keys survived (no replay); by-comment scan returned %d for %s/%s",
		len(want), n, author, permlink)
}

// TestMmapCold_LosesDataOnRestart documents WHY the cutover is needed: the
// default mmap backend wipes the cold tier on every open.
func TestMmapCold_LosesDataOnRestart(t *testing.T) {
	dir := t.TempDir()
	c1, err := openMmapColdKV(dir)
	if err != nil {
		t.Fatal(err)
	}
	key := appendColdVotePrimaryKey(nil, "voter000", "auth00", "post-000")
	c1.bset(key, []byte("blob"))
	if _, ok := c1.get(key); !ok {
		t.Fatal("pre-restart get failed")
	}
	c1.Close()

	c2, err := openMmapColdKV(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	if _, ok := c2.get(key); ok {
		t.Fatal("mmapColdKV unexpectedly survived restart — test premise wrong")
	}
	t.Log("confirmed: mmapColdKV loses all cold data on restart (O_TRUNC) → replay required; PersistCold fixes this")
}
