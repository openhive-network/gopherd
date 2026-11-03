package sync

import (
	"bytes"
	"path/filepath"
	"testing"
	"time"

	"gopherd/internal/blocklog"
	"gopherd/internal/p2p/proto"
	"gopherd/internal/protocol/block"
)

// testChain is a linked chain of header-only signed blocks: real enough for
// blockIDFromRaw (previous, timestamp, witness, merkle root, empty extensions,
// signature) plus an empty transaction vector. raws[n] / ids[n] is block n.
type testChain struct {
	raws [][]byte
	ids  []block.ID
}

func buildTestChain(t *testing.T, head uint32, headTS uint32) *testChain {
	t.Helper()
	c := &testChain{raws: make([][]byte, head+1), ids: make([]block.ID, head+1)}
	for n := uint32(1); n <= head; n++ {
		sh := block.SignedHeader{Header: block.Header{
			Previous:   c.ids[n-1],
			Timestamp:  headTS - 3*(head-n),
			Witness:    "initminer",
			Extensions: []byte{0},
		}}
		sh.TransactionMerkleRoot[0] = byte(n)
		sh.TransactionMerkleRoot[1] = byte(n >> 8)
		raw := block.MarshalSignedHeader(nil, &sh)
		hdrLen := len(raw)
		raw = append(raw, 0) // transactions: empty vector
		c.raws[n] = raw
		c.ids[n] = block.ConstructID(raw[:hdrLen], n)
		if got, ok := blockIDFromRaw(raw); !ok || got != c.ids[n] {
			t.Fatalf("test chain block %d: blockIDFromRaw disagrees", n)
		}
	}
	return c
}

// writeLog writes blocks 1..upTo of c to a fresh compressed block_log and
// returns its path and the still-open writer.
func (c *testChain) writeLog(t *testing.T, upTo uint32) (string, blocklog.Writer) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "block_log")
	w, err := blocklog.OpenForWrite(path, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { w.Close() })
	for n := uint32(1); n <= upTo; n++ {
		if err := w.Append(c.raws[n]); err != nil {
			t.Fatal(err)
		}
	}
	return path, w
}

func openHistory(t *testing.T, path string, keep int) *BlockLogHistory {
	t.Helper()
	r, err := blocklog.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	h, err := NewBlockLogHistory(r, keep)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// The history serves a block only under its exact id, never the block that
// happens to sit at the requested height, and follows the writer.
func TestBlockLogHistoryServesExactIDs(t *testing.T) {
	c := buildTestChain(t, 40, uint32(time.Now().Unix()))
	path, w := c.writeLog(t, 30)
	h := openHistory(t, path, -1)

	for n := uint32(1); n < 30; n++ {
		id, ok := h.BlockIDForNum(n)
		if !ok || id != c.ids[n] {
			t.Fatalf("BlockIDForNum(%d) = %v,%v, want %v", n, id, ok, c.ids[n])
		}
		if raw := h.BlockBytesByID(c.ids[n]); !bytes.Equal(raw, c.raws[n]) {
			t.Fatalf("BlockBytesByID(block %d) returned the wrong bytes", n)
		}
	}
	// Same height, different id (another fork's block): nothing.
	forged := c.ids[10]
	forged[19] ^= 0xff
	if raw := h.BlockBytesByID(forged); raw != nil {
		t.Fatal("served a block under an id it does not have")
	}
	// The newest logged block is held back (its end is not settled until the
	// next block is indexed), and nothing past the log exists.
	if _, ok := h.BlockIDForNum(30); ok {
		t.Fatal("served the newest block_log entry")
	}
	if _, ok := h.BlockIDForNum(35); ok {
		t.Fatal("served a block past the block_log head")
	}
	// Blocks the writer appends after open become servable.
	for n := uint32(31); n <= 40; n++ {
		if err := w.Append(c.raws[n]); err != nil {
			t.Fatal(err)
		}
	}
	if id, ok := h.BlockIDForNum(35); !ok || id != c.ids[35] {
		t.Fatalf("block 35 after the writer appended it: %v,%v", id, ok)
	}
	if st := h.Stats(); st.ReadErrors != 0 || st.BlocksServed != 29 {
		t.Fatalf("stats %+v, want 29 blocks served and no read errors", st)
	}
}

// keep mirrors hived's block-log-split: the head's million-block part plus
// the N full parts before it.
func TestBlockLogHistoryRangeMirrorsBlockLogSplit(t *testing.T) {
	cases := []struct {
		keep int
		head uint32
		want uint32
	}{
		{-1, 50_000_000, 1},
		{1, 999_999, 1},
		{1, 2_500_000, 1_000_001},
		{1, 3_000_000, 1_000_001},
		{1, 3_000_001, 2_000_001},
		{2, 2_500_000, 1},
		{9999, 110_000_000, 1}, // hived's default keeps everything
	}
	for _, tc := range cases {
		h := &BlockLogHistory{keep: tc.keep}
		if got := h.lowestServed(tc.head); got != tc.want {
			t.Errorf("keep=%d head=%d: lowest served %d, want %d", tc.keep, tc.head, got, tc.want)
		}
	}
	if _, err := NewBlockLogHistory(nil, 0); err == nil {
		t.Error("NewBlockLogHistory accepted a nil reader")
	}
}

// A serving node at the tip with History answers a from-genesis peer (empty
// synopsis) with ids starting at block 1 out of the block_log, then serves
// those blocks; without History the same node has nothing below its ring.
func TestSyncFromGenesisServedFromBlockLog(t *testing.T) {
	const head = 3000
	c := buildTestChain(t, head, uint32(time.Now().Unix()))
	path, _ := c.writeLog(t, head)

	// The ring holds only the last 200 blocks, as RecentBlocks would.
	ring := newStubChain()
	for n := uint32(head - 199); n <= head; n++ {
		ring.cache[n] = c.raws[n]
		ring.idMap[n] = c.ids[n]
		ring.byID[c.ids[n]] = c.raws[n]
		ring.order = append(ring.order, n)
	}
	ring.head, ring.headID = head, c.ids[head]

	req := proto.FetchBlockchainItemIDsMessage{ItemType: proto.ItemTypeBlock}
	reqBody, err := req.Marshal()
	if err != nil {
		t.Fatal(err)
	}

	// Ring only: a peer at genesis gets an empty list (the ring starts at
	// head-199), which is the gap this feature closes.
	plain := newWithChain(Config{Serve: true, MaxBlocksPerInv: 200}, ring)
	plain.headBlockHint.Store(head)
	fake, sent := captureReplies(plain)
	plain.handleFetchBlockchainItemIDs(fake, reqBody)
	if inv := decodeInventory(t, *sent); len(inv.ItemHashesAvailable) != 0 {
		t.Fatalf("ring-only node listed %d ids for a genesis peer, want 0", len(inv.ItemHashesAvailable))
	}

	d := newWithChain(Config{Serve: true, MaxBlocksPerInv: 200, History: openHistory(t, path, -1)}, ring)
	d.headBlockHint.Store(head)
	if d.isCatchingUp() {
		t.Fatal("harness is wrong: a head stamped now must read as at the tip")
	}
	fake, sent = captureReplies(d)
	d.handleFetchBlockchainItemIDs(fake, reqBody)
	inv := decodeInventory(t, *sent)
	if len(inv.ItemHashesAvailable) != HistoryMaxBlocksPerInv {
		t.Fatalf("listed %d ids, want %d", len(inv.ItemHashesAvailable), HistoryMaxBlocksPerInv)
	}
	// hived demands a reply to an empty synopsis start at block 1.
	for i, h := range inv.ItemHashesAvailable {
		if block.ID(h) != c.ids[i+1] {
			t.Fatalf("id[%d] = block %d's? want block %d's id", i, block.ID(h).Num(), i+1)
		}
	}
	if want := uint32(head - HistoryMaxBlocksPerInv); inv.TotalRemainingItemCount != want {
		t.Fatalf("remaining %d, want %d", inv.TotalRemainingItemCount, want)
	}

	// The peer's next round sends a synopsis ending at the last id it got;
	// the reply anchors there and runs on into the ring.
	next := proto.FetchBlockchainItemIDsMessage{
		ItemType:           proto.ItemTypeBlock,
		BlockchainSynopsis: []proto.ItemHash{proto.ItemHash(c.ids[1]), proto.ItemHash(c.ids[HistoryMaxBlocksPerInv])},
	}
	nextBody, err := next.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	fake, sent = captureReplies(d)
	d.handleFetchBlockchainItemIDs(fake, nextBody)
	inv = decodeInventory(t, *sent)
	if got := len(inv.ItemHashesAvailable); got != head-HistoryMaxBlocksPerInv+1 {
		t.Fatalf("second round listed %d ids, want %d (anchor + the rest to head)", got, head-HistoryMaxBlocksPerInv+1)
	}
	if block.ID(inv.ItemHashesAvailable[0]) != c.ids[HistoryMaxBlocksPerInv] {
		t.Fatal("second round does not start at the common ancestor")
	}
	if last := block.ID(inv.ItemHashesAvailable[len(inv.ItemHashesAvailable)-1]); last != c.ids[head] || inv.TotalRemainingItemCount != 0 {
		t.Fatalf("second round ends at block %d with %d remaining, want head %d and 0", last.Num(), inv.TotalRemainingItemCount, head)
	}

	// fetch_items for blocks in the log, in the ring, and one forged id.
	forged := c.ids[7]
	forged[19] ^= 0xff
	fetch := proto.FetchItemsMessage{ItemType: proto.ItemTypeBlock, ItemsToFetch: []proto.ItemHash{
		proto.ItemHash(c.ids[1]), proto.ItemHash(c.ids[1500]), proto.ItemHash(c.ids[head]), proto.ItemHash(forged),
	}}
	fetchBody, err := fetch.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	fake, sent = captureReplies(d)
	d.handleFetchItems(fake, fetchBody)
	if len(*sent) != 4 {
		t.Fatalf("fetch_items: %d replies, want 4", len(*sent))
	}
	for i, n := range []uint32{1, 1500, head} {
		r := (*sent)[i]
		if r.typ != proto.BlockMessageType {
			t.Fatalf("block %d: reply %v, want block_message", n, r.typ)
		}
		var bm proto.BlockMessage
		if err := bm.Unmarshal(r.body); err != nil {
			t.Fatal(err)
		}
		if block.ID(bm.BlockID) != c.ids[n] || !bytes.Equal(bm.SignedBlockBytes, c.raws[n]) {
			t.Fatalf("block %d: served the wrong block", n)
		}
	}
	if (*sent)[3].typ != proto.ItemNotAvailableMessageType {
		t.Fatalf("forged id: reply %v, want item_not_available", (*sent)[3].typ)
	}
}

func decodeInventory(t *testing.T, sent []capturedReply) proto.BlockchainItemIDsInventoryMessage {
	t.Helper()
	if len(sent) != 1 || sent[0].typ != proto.BlockchainItemIDsInventoryMessageType {
		t.Fatalf("sent %d replies (%v), want one blockchain_item_ids_inventory", len(sent), sent)
	}
	var inv proto.BlockchainItemIDsInventoryMessage
	if err := inv.Unmarshal(sent[0].body); err != nil {
		t.Fatal(err)
	}
	return inv
}
