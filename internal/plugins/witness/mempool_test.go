package witness

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"testing"

	"gopherd/internal/crypto"
	"gopherd/internal/fcraw"
	"gopherd/internal/protocol/asset"
	"gopherd/internal/protocol/op"
	"gopherd/internal/protocol/tx"
	"gopherd/internal/state/genesis"
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

	// At the cap the queue must stay BOUNDED (contents are attacker-controlled)
	// while still admitting the newest arrival — the oldest is dropped instead.
	//
	// Refusing new arrivals was the wrong end: ~half the transactions the network
	// gossips are never mined by anyone, so a full-and-refusing queue holds only
	// stale entries and produces empty blocks forever. The newest transactions
	// are the includable ones.
	p.mu.Lock()
	p.pending = make([]pendingTx, MaxMempoolTx)
	for i := range p.pending {
		p.pending[i].id = [20]byte{byte(i), byte(i >> 8)}
		p.pendingIDs[p.pending[i].id] = struct{}{}
	}
	p.mu.Unlock()
	other, oid := rawTx(t, "overflow", 1<<30)
	if !p.AddPendingTx(other, oid, 1<<30) {
		t.Fatal("refused a FRESH transaction at the cap — a full queue holds stale entries, so refusing new arrivals guarantees empty blocks")
	}
	if got := p.PendingCount(); got > MaxMempoolTx {
		t.Fatalf("depth=%d exceeds cap %d — memory must stay bounded", got, MaxMempoolTx)
	}
	if p.OverflowedCount() == 0 {
		t.Fatal("overflow not counted; a high overflow rate is the signal the cap is too small")
	}
	// The newest must be present, the oldest gone.
	p.mu.Lock()
	_, newestPresent := p.pendingIDs[oid]
	_, oldestGone := p.pendingIDs[[20]byte{0, 0}]
	p.mu.Unlock()
	if !newestPresent {
		t.Fatal("newest transaction not retained")
	}
	if oldestGone {
		t.Fatal("oldest transaction not dropped — index out of lockstep with the queue")
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

	p.dropExpired(1500)                              // removes a
	p.RemoveIncludedTxIDs([][20]byte{bid})           // removes b
	p.removeIncluded(map[[20]byte]struct{}{cid: {}}) // removes c

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

// TestRemoveIncludedTxID covers the single-id eviction fired from
// chain.ApplyBlock for EVERY apply path.
//
// The p2p-side hook it replaces only saw blocks that passed back through sync,
// so blocks applied via drainForward (out-of-order buffering) or re-applied
// during a reorg were missed. Measured live: ~46% coverage, queue still growing
// ~356/min toward the 8000 cap — where admission stops and we produce empty
// blocks again. Partial eviction looks like working eviction until you plot it.
func TestRemoveIncludedTxID(t *testing.T) {
	p := newMempoolPlugin(t)
	a, aid := rawTx(t, "mined", 1<<30)
	b, bid := rawTx(t, "pending", 1<<30)
	p.AddPendingTx(a, aid, 1<<30)
	p.AddPendingTx(b, bid, 1<<30)

	p.RemoveIncludedTxID(aid)
	if n := p.PendingCount(); n != 1 {
		t.Fatalf("PendingCount=%d want 1", n)
	}
	// Unknown id is a cheap no-op — this fires for every transaction on chain,
	// the vast majority of which we never held.
	p.RemoveIncludedTxID([20]byte{0xde, 0xad})
	if n := p.PendingCount(); n != 1 {
		t.Fatalf("unknown id disturbed the queue: PendingCount=%d", n)
	}
	// Index must stay in lockstep: the evicted transaction must be re-admittable.
	if !p.AddPendingTx(a, aid, 1<<30) {
		t.Fatal("re-offer of an evicted transaction REJECTED — stale id left in the dedup index")
	}
	if n := p.PendingCount(); n != 2 {
		t.Fatalf("PendingCount=%d want 2", n)
	}
	p.mu.Lock()
	left := p.pending[0].id
	p.mu.Unlock()
	if left != bid {
		t.Fatal("eviction removed the wrong transaction")
	}
}

// TestEvictedCountTracksMechanism pins the counter to the mechanism.
//
// When eviction moved from the p2p layer into chain.ApplyBlock, the counter was
// left behind in the retired p2p hook — so STATS reported evict=0 while the new
// mechanism ran untested, and "is it working?" became unanswerable. A metric
// that does not move with the code it measures is worse than no metric: it
// reads as a confident zero.
func TestEvictedCountTracksMechanism(t *testing.T) {
	p := newMempoolPlugin(t)
	a, aid := rawTx(t, "e1", 1<<30)
	b, bid := rawTx(t, "e2", 1<<30)
	p.AddPendingTx(a, aid, 1<<30)
	p.AddPendingTx(b, bid, 1<<30)

	if got := p.EvictedCount(); got != 0 {
		t.Fatalf("EvictedCount=%d before any eviction", got)
	}
	p.RemoveIncludedTxID(aid)
	if got := p.EvictedCount(); got != 1 {
		t.Fatalf("EvictedCount=%d after evicting 1", got)
	}
	// An id we never held must NOT count — otherwise the counter tracks chain
	// throughput rather than our own eviction and always looks healthy.
	p.RemoveIncludedTxID([20]byte{0x99})
	if got := p.EvictedCount(); got != 1 {
		t.Fatalf("EvictedCount=%d after an unknown id — it must count OUR evictions, not every mined transaction", got)
	}
	p.RemoveIncludedTxID(bid)
	if got := p.EvictedCount(); got != 2 || p.PendingCount() != 0 {
		t.Fatalf("EvictedCount=%d depth=%d want 2/0", got, p.PendingCount())
	}
}

// TestAdvertIDIsTheLegacyMessageHash pins the p2p inventory identity of a
// transaction.
//
// hived identifies transactions in inventory by ripemd160 over the WHOLE signed
// transaction (full_transaction.cpp:86, get_legacy_transaction_message_hash),
// NOT by transaction id (which is a prefix of sha256 over the UNSIGNED bytes).
// Advertising the transaction id got us dropped 234 times in one run with
// "You sent me a message that I didn't request": peers fetched what we
// advertised, hashed the body their way, and concluded they had never asked.
//
// The two must be DIFFERENT values — if this test ever finds them equal, the
// implementation has collapsed onto the wrong one.
func TestAdvertIDIsTheLegacyMessageHash(t *testing.T) {
	raw, txid := rawTx(t, "advert-identity", 1<<30)

	advert := AdvertIDOf(raw)
	if advert == ([20]byte{}) {
		t.Fatal("advert id is zero")
	}
	if advert == txid {
		t.Fatal("advert id equals the transaction id — they are computed over different bytes with different hashes; equality means we reverted to the identity that got us dropped")
	}
	// Deterministic, and over the FULL body: any byte change moves it.
	if AdvertIDOf(raw) != advert {
		t.Fatal("advert id is not deterministic")
	}
	mutated := append([]byte(nil), raw...)
	mutated[len(mutated)-1] ^= 0xff // last byte is inside the signature area
	if AdvertIDOf(mutated) == advert {
		t.Fatal("advert id ignores trailing bytes; it must cover the whole signed transaction, signatures included")
	}
}

// TestPendingRawServesByAdvertID pins that we can serve what we advertise.
// Advertising something we cannot then provide is precisely what gets a node
// dropped by its peers.
func TestPendingRawServesByAdvertID(t *testing.T) {
	p := newMempoolPlugin(t)
	raw, id := rawTx(t, "serve-me", 1<<30)
	if !p.AddPendingTx(raw, id, 1<<30) {
		t.Fatal("insert refused")
	}
	advert := AdvertIDOf(raw)
	got := p.PendingRaw(advert)
	if got == nil {
		t.Fatal("cannot serve a transaction we would have advertised — the peer that asks gets nothing and drops us")
	}
	if string(got) != string(raw) {
		t.Fatal("served bytes differ from what was queued")
	}
	// Looking up by the TRANSACTION ID must not work: that is the wrong identity.
	if p.PendingRaw(id) != nil {
		t.Fatal("served by transaction id; the serve index must key on the advert identity")
	}
	// Once evicted, we must no longer claim to have it.
	p.RemoveIncludedTxID(id)
	if p.PendingRaw(advert) != nil {
		t.Fatal("still serving an evicted transaction — the advert index is out of lockstep")
	}
}

// TestDrainAfterProduceClearsIndexes: the post-production drain emptied the
// queue but left its id and advert indexes, which no removal path reaches
// once the queue no longer holds the entry. They grew for as long as the node
// produced, and the stale ids refused re-admission.
func TestDrainAfterProduceClearsIndexes(t *testing.T) {
	p := newMempoolPlugin(t)
	a, aid := rawTx(t, "drained", 1<<30)
	if !p.AddPendingTx(a, aid, 1<<30) {
		t.Fatal("seed insert refused")
	}
	p.mu.Lock()
	p.drainPendingLocked()
	n, ids, adverts := len(p.pending), len(p.pendingIDs), len(p.pendingByAdvert)
	p.mu.Unlock()
	if n != 0 || ids != 0 || adverts != 0 {
		t.Fatalf("after drain: pending=%d pendingIDs=%d pendingByAdvert=%d, want all 0", n, ids, adverts)
	}
	if !p.AddPendingTx(a, aid, 1<<30) {
		t.Fatal("drained transaction could not be re-admitted")
	}
}
