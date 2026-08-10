package witness

import (
	"bytes"
	"errors"
	"crypto/sha256"
	"testing"

	"hived-go/internal/crypto"
	"hived-go/internal/fcraw"
	"hived-go/internal/protocol/asset"
	"hived-go/internal/protocol/op"
	"hived-go/internal/protocol/tx"
	"hived-go/internal/state/genesis"
)

// rawTx builds a serialized signed_transaction carrying a transfer, plus its id.
// Content does not need to be executable for the mempool-mechanics tests below;
// what matters is that it is a well-formed signed_transaction with ≥1 operation.
func rawTx(t *testing.T, memo string, expiration uint32) ([]byte, [20]byte) {
	t.Helper()
	st := tx.SignedTransaction{
		Transaction: tx.Transaction{
			RefBlockNum:    1,
			RefBlockPrefix: 2,
			Expiration:     expiration,
			Operations: []op.Op{op.FromOperation(&op.Transfer{
				From:   genesis.MainnetInitminer,
				To:     "alice",
				Amount: asset.Asset{Amount: 1, Symbol: asset.Symbol{Num: asset.HiveSymbol, WasLegacyEncoded: true}},
				Memo:   memo,
			})},
		},
	}
	raw := tx.AppendSignedTransaction(nil, &st)
	res, err := tx.ReadSignedTransaction(fcraw.NewReader(raw))
	if err != nil {
		t.Fatalf("re-read: %v", err)
	}
	id := tx.IDFromDigest(tx.DigestFromBytes(raw[res.UnsignedStart:res.UnsignedEnd]))
	return raw, id
}

func errorsAs(err error, target any) bool { return errors.As(err, target) }

func newMempoolPlugin(t *testing.T) *Plugin {
	t.Helper()
	seed := sha256.Sum256([]byte("mempool-fixture"))
	priv, err := crypto.PrivateKeyFromBytes(seed[:])
	if err != nil {
		t.Fatal(err)
	}
	cdb, db := newSigningFixture(t, priv)
	return New(cdb, db, []Producer{{
		Account: genesis.MainnetInitminer, PrivateKey: priv, PublicKey33: priv.PublicKey(),
	}})
}

// TestMempoolStoresTransactionsVerbatim is the regression test for the defect
// that made every produced block empty AND unusable.
//
// The mempool used to be a flat []op.Op: inbound transactions were torn apart,
// their operations re-packed into ONE new transaction at build time, and the
// original signatures thrown away. A transaction's signatures cover its own
// serialized bytes, so the repacked transaction carried other accounts'
// operations with no authority whatsoever — any block containing one would be
// rejected by consensus. Bytes in must equal bytes out.
func TestMempoolStoresTransactionsVerbatim(t *testing.T) {
	p := newMempoolPlugin(t)
	raw, id := rawTx(t, "verbatim", 1<<30)

	if !p.AddPendingTx(raw, id, 1<<30) {
		t.Fatal("AddPendingTx refused a fresh transaction")
	}
	p.mu.Lock()
	stored := p.pending[0].raw
	p.mu.Unlock()

	if !bytes.Equal(stored, raw) {
		t.Fatalf("stored bytes differ from input: got %x want %x — signatures cover these exact bytes, so any mutation invalidates the transaction", stored, raw)
	}
	// And the stored copy must be independent of the caller's buffer: the p2p
	// layer reuses receive buffers, so aliasing would corrupt the mempool.
	raw[len(raw)-1] ^= 0xff
	p.mu.Lock()
	stored = p.pending[0].raw
	p.mu.Unlock()
	if bytes.Equal(stored, raw) {
		t.Fatal("mempool aliased the caller's buffer; a reused p2p receive buffer would silently corrupt queued transactions")
	}
}

func TestMempoolDedupesAndCaps(t *testing.T) {
	p := newMempoolPlugin(t)
	raw, id := rawTx(t, "dup", 1<<30)

	if !p.AddPendingTx(raw, id, 1<<30) {
		t.Fatal("first insert refused")
	}
	if p.AddPendingTx(raw, id, 1<<30) {
		t.Fatal("duplicate transaction accepted; it would occupy a block slot twice and fail dedup at apply")
	}
	if n := p.PendingCount(); n != 1 {
		t.Fatalf("PendingCount=%d want 1", n)
	}

	// Cap: mempool contents are attacker-controlled, so growth must be bounded.
	p.mu.Lock()
	p.pending = make([]pendingTx, MaxMempoolTx)
	p.mu.Unlock()
	other, oid := rawTx(t, "overflow", 1<<30)
	if p.AddPendingTx(other, oid, 1<<30) {
		t.Fatalf("accepted a transaction past the %d cap — unbounded mempool growth is a trivial memory exhaustion of a witness", MaxMempoolTx)
	}
}

// TestMempoolDropsExpired pins the ageing sweep. Without it, transactions the
// chain will never accept again pile up forever and are re-offered to the
// selector every single slot.
func TestMempoolDropsExpired(t *testing.T) {
	p := newMempoolPlugin(t)
	live, liveID := rawTx(t, "live", 2000)
	dead, deadID := rawTx(t, "dead", 1000)
	p.AddPendingTx(live, liveID, 2000)
	p.AddPendingTx(dead, deadID, 1000)

	if got := p.dropExpired(1500); got != 1 {
		t.Fatalf("dropExpired removed %d, want 1", got)
	}
	if n := p.PendingCount(); n != 1 {
		t.Fatalf("PendingCount=%d want 1", n)
	}
	p.mu.Lock()
	remaining := p.pending[0].id
	p.mu.Unlock()
	if remaining != liveID {
		t.Fatal("dropExpired removed the wrong transaction")
	}
	// Boundary: expiration == now is expired (the chain rejects it too).
	if got := p.dropExpired(2000); got != 1 {
		t.Fatalf("expiration == now should be dropped, removed %d", got)
	}
}

func TestMempoolRemoveIncluded(t *testing.T) {
	p := newMempoolPlugin(t)
	a, aid := rawTx(t, "a", 1<<30)
	b, bid := rawTx(t, "b", 1<<30)
	p.AddPendingTx(a, aid, 1<<30)
	p.AddPendingTx(b, bid, 1<<30)

	p.removeIncluded(map[[20]byte]struct{}{aid: {}})
	if n := p.PendingCount(); n != 1 {
		t.Fatalf("PendingCount=%d want 1", n)
	}
	p.mu.Lock()
	left := p.pending[0].id
	p.mu.Unlock()
	if left != bid {
		t.Fatal("removeIncluded dropped the wrong transaction")
	}
}

// TestRemoveIncludedTxIDs pins the eviction that turns the mempool from
// write-only into a working queue.
//
// Without it, every transaction we admit is mined by another witness within
// seconds and NOTHING removes it. By our slot the whole queue is duplicates,
// pre-flight rejects all of them, and we produce an EMPTY block while the
// network is busy — observed on mainnet 2026-08-10 as depth climbing
// 598→1082 monotonically, with blocks 108891828 and 108907971 both empty
// beside neighbours carrying 13-41 transactions.
func TestRemoveIncludedTxIDs(t *testing.T) {
	p := newMempoolPlugin(t)
	a, aid := rawTx(t, "mined-by-someone-else", 1<<30)
	b, bid := rawTx(t, "still-pending", 1<<30)
	c, cid := rawTx(t, "also-mined", 1<<30)
	p.AddPendingTx(a, aid, 1<<30)
	p.AddPendingTx(b, bid, 1<<30)
	p.AddPendingTx(c, cid, 1<<30)

	// A block from another witness carrying two of ours, plus one we never saw.
	_, other := rawTx(t, "never-in-our-mempool", 1<<30)
	n := p.RemoveIncludedTxIDs([][20]byte{aid, cid, other})
	if n != 2 {
		t.Fatalf("evicted %d, want 2 (unknown ids must be ignored, not counted)", n)
	}
	if got := p.PendingCount(); got != 1 {
		t.Fatalf("PendingCount=%d want 1", got)
	}
	p.mu.Lock()
	left := p.pending[0].id
	p.mu.Unlock()
	if left != bid {
		t.Fatal("eviction removed the wrong transaction — the still-pending one must survive")
	}

	// Empty input is a no-op, not a wipe: an empty block must not clear the queue.
	if n := p.RemoveIncludedTxIDs(nil); n != 0 || p.PendingCount() != 1 {
		t.Fatalf("empty eviction changed the mempool (n=%d depth=%d) — an empty block must leave it intact", n, p.PendingCount())
	}
}

// TestMempoolIndexStaysInLockstep guards the O(1) dedup index against the
// classic parallel-structure bug: an id left in the index after its transaction
// leaves the queue would permanently REJECT that transaction if it were offered
// again — silently shrinking what we can include, and doing so more the longer
// the node runs.
//
// Exercises every removal path (expiry, our-block inclusion, another witness's
// block) and then re-offers the removed transactions, which must be accepted.
func TestMempoolIndexStaysInLockstep(t *testing.T) {
	p := newMempoolPlugin(t)
	a, aid := rawTx(t, "expire-me", 1000)
	b, bid := rawTx(t, "mined-elsewhere", 1<<30)
	c, cid := rawTx(t, "included-by-us", 1<<30)

	for _, x := range []struct {
		raw []byte
		id  [20]byte
		exp uint32
	}{{a, aid, 1000}, {b, bid, 1 << 30}, {c, cid, 1 << 30}} {
		if !p.AddPendingTx(x.raw, x.id, x.exp) {
			t.Fatal("insert refused")
		}
	}
	if p.AddPendingTx(a, aid, 1000) {
		t.Fatal("index failed to dedup")
	}

	p.dropExpired(1500)                                    // removes a
	p.RemoveIncludedTxIDs([][20]byte{bid})                 // removes b
	p.removeIncluded(map[[20]byte]struct{}{cid: {}})       // removes c

	if n := p.PendingCount(); n != 0 {
		t.Fatalf("PendingCount=%d want 0", n)
	}
	// The index must be empty too: re-offering any of them must now succeed.
	for i, x := range []struct {
		raw []byte
		id  [20]byte
	}{{a, aid}, {b, bid}, {c, cid}} {
		if !p.AddPendingTx(x.raw, x.id, 1<<30) {
			t.Fatalf("re-offer %d REJECTED — a stale id survived in the dedup index; over time this silently starves the mempool", i)
		}
	}
	if n := p.PendingCount(); n != 3 {
		t.Fatalf("PendingCount=%d want 3", n)
	}
}

// TestAddPendingRawReportsDuplicates pins that a duplicate is distinguishable
// from a genuine admission. Counting duplicates as accepted showed acc=68767
// beside depth=6266 and made a queue that never drained look healthy.
func TestAddPendingRawReportsDuplicates(t *testing.T) {
	p := newMempoolPlugin(t)
	// AddPendingRaw validates authority BEFORE dedup, and the fixture
	// transactions are unsigned. Drop the chain reference so admission skips
	// validation and we exercise the duplicate path this test is about.
	p.Chain = nil
	raw, id := rawTx(t, "dup-report", 1<<30)
	if !p.AddPendingTx(raw, id, 1<<30) {
		t.Fatal("seed insert refused")
	}
	err := p.AddPendingRaw(raw)
	if err == nil {
		t.Fatal("duplicate reported as accepted")
	}
	var dup interface{ IsDuplicateTx() bool }
	if !errorsAs(err, &dup) || !dup.IsDuplicateTx() {
		t.Fatalf("duplicate error not recognisable by the p2p layer: %v", err)
	}
}
