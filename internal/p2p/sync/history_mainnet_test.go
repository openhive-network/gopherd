package sync

import (
	"bytes"
	"os"
	"testing"
	"time"

	"gopherd/internal/blocklog"
	"gopherd/internal/p2p/proto"
	"gopherd/internal/protocol/block"
)

// Opt-in: drives the serving handlers against a real block_log, which may be
// one a running node is still appending to.
//
//	GOPHERD_TEST_BLOCK_LOG=$HOME/gopherd-sync/data/blockchain/block_log \
//	  go test ./internal/p2p/sync -run Mainnet -v
//
// Checks what the synthetic chain cannot: that ids computed from real blocks
// of every era are the chain's ids (each served block's previous is the id
// listed before it), that multi-round walks stay contiguous, and that a
// pruned range (keep=N) never lists a block it cannot serve.
func TestHistoryAgainstMainnetBlockLog(t *testing.T) {
	path := os.Getenv("GOPHERD_TEST_BLOCK_LOG")
	if path == "" {
		t.Skip("GOPHERD_TEST_BLOCK_LOG not set")
	}
	r, err := blocklog.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	logHead := r.Head()
	if logHead < 3_000_000 {
		t.Fatalf("block_log head %d: want at least 3M blocks", logHead)
	}

	// The serving node's own head: a stand-in block one past the log, stamped
	// now so the catch-up gate reads the node as at the tip. Everything below
	// it is served out of the block_log.
	newNode := func(keep int) (*Delegate, *BlockLogHistory) {
		h, err := NewBlockLogHistory(r, keep)
		if err != nil {
			t.Fatal(err)
		}
		head := logHead + 1
		sh := block.SignedHeader{Header: block.Header{Timestamp: uint32(time.Now().Unix()), Witness: "initminer", Extensions: []byte{0}}}
		raw := append(block.MarshalSignedHeader(nil, &sh), 0)
		id := block.ConstructID(raw[:len(raw)-1], head)
		ring := newStubChain()
		ring.cache[head], ring.idMap[head], ring.byID[id] = raw, id, raw
		ring.order = append(ring.order, head)
		ring.head, ring.headID = head, id
		d := newWithChain(Config{Serve: true, History: h}, ring)
		d.headBlockHint.Store(head)
		if d.isCatchingUp() {
			t.Fatal("harness: the stand-in head must read as at the tip")
		}
		return d, h
	}

	ask := func(d *Delegate, synopsis ...block.ID) proto.BlockchainItemIDsInventoryMessage {
		t.Helper()
		req := proto.FetchBlockchainItemIDsMessage{ItemType: proto.ItemTypeBlock}
		for _, id := range synopsis {
			req.BlockchainSynopsis = append(req.BlockchainSynopsis, proto.ItemHash(id))
		}
		body, err := req.Marshal()
		if err != nil {
			t.Fatal(err)
		}
		fake, sent := captureReplies(d)
		d.handleFetchBlockchainItemIDs(fake, body)
		return decodeInventory(t, *sent)
	}

	// checkLinked: numbers contiguous from first, and every block's previous
	// is the id before it, read back through the same History a peer's
	// fetch_items would hit.
	checkLinked := func(h *BlockLogHistory, inv proto.BlockchainItemIDsInventoryMessage, first uint32) block.ID {
		t.Helper()
		ids := inv.ItemHashesAvailable
		if len(ids) == 0 {
			t.Fatalf("empty reply, want ids from %d", first)
		}
		for i, ih := range ids {
			id := block.ID(ih)
			if id.Num() != first+uint32(i) {
				t.Fatalf("id[%d] is block %d, want %d", i, id.Num(), first+uint32(i))
			}
			raw := h.BlockBytesByID(id)
			if raw == nil {
				t.Fatalf("listed block %d but cannot serve it", id.Num())
			}
			if i > 0 && !bytes.Equal(raw[:20], ids[i-1][:]) {
				t.Fatalf("block %d: previous is not the id listed before it", id.Num())
			}
		}
		return block.ID(ids[len(ids)-1])
	}

	t.Run("from genesis, three rounds", func(t *testing.T) {
		d, h := newNode(-1)
		inv := ask(d)
		last := checkLinked(h, inv, 1)
		for round := 2; round <= 3; round++ {
			inv = ask(d, last)
			if block.ID(inv.ItemHashesAvailable[0]) != last {
				t.Fatalf("round %d does not start at the anchor", round)
			}
			last = checkLinked(h, inv, last.Num())
		}
		if want := logHead + 1 - last.Num(); inv.TotalRemainingItemCount != want {
			t.Fatalf("remaining %d after block %d, want %d", inv.TotalRemainingItemCount, last.Num(), want)
		}
	})

	t.Run("deep synopsis, every 5M blocks", func(t *testing.T) {
		d, h := newNode(-1)
		for n := uint32(5_000_000); n+HistoryMaxBlocksPerInv < logHead; n += 5_000_000 {
			anchor, ok := h.BlockIDForNum(n)
			if !ok {
				t.Fatalf("block %d not readable", n)
			}
			// A real synopsis: older entries first, the anchor last.
			older, _ := h.BlockIDForNum(n / 2)
			last := checkLinked(h, ask(d, older, anchor), n)
			// The anchor counts toward the per-reply cap.
			if want := n + HistoryMaxBlocksPerInv - 1; last.Num() != want {
				t.Fatalf("from %d: reply ends at %d, want %d", n, last.Num(), want)
			}
		}
	})

	t.Run("keep=1 serves only the kept parts", func(t *testing.T) {
		d, h := newNode(1)
		lowest := h.lowestServed(r.Head())
		if lowest <= 1 {
			t.Fatalf("lowest served %d with head %d, want a pruned range", lowest, r.Head())
		}
		if _, ok := h.BlockIDForNum(lowest - 1); ok {
			t.Fatalf("block %d below the kept range is served", lowest-1)
		}
		anchor, ok := h.BlockIDForNum(lowest + 10)
		if !ok {
			t.Fatalf("block %d inside the kept range is not served", lowest+10)
		}
		checkLinked(h, ask(d, anchor), lowest+10)
		// A peer at genesis, below the kept range: "nothing to give".
		gen := ask(d)
		if len(gen.ItemHashesAvailable) != 0 || gen.TotalRemainingItemCount != 0 {
			t.Fatalf("genesis peer, keep=1 (lowest %d): %d ids, %d remaining, want 0 and 0",
				lowest, len(gen.ItemHashesAvailable), gen.TotalRemainingItemCount)
		}
	})

	t.Run("fetch_items returns the block_log bytes", func(t *testing.T) {
		d, h := newNode(-1)
		var want []uint32
		for n := uint32(1); n < logHead; n += logHead / 7 {
			want = append(want, n)
		}
		var req proto.FetchItemsMessage
		req.ItemType = proto.ItemTypeBlock
		for _, n := range want {
			id, ok := h.BlockIDForNum(n)
			if !ok {
				t.Fatalf("block %d not readable", n)
			}
			req.ItemsToFetch = append(req.ItemsToFetch, proto.ItemHash(id))
		}
		body, err := req.Marshal()
		if err != nil {
			t.Fatal(err)
		}
		fake, sent := captureReplies(d)
		d.handleFetchItems(fake, body)
		if len(*sent) != len(want) {
			t.Fatalf("%d replies, want %d", len(*sent), len(want))
		}
		for i, n := range want {
			var bm proto.BlockMessage
			if (*sent)[i].typ != proto.BlockMessageType || bm.Unmarshal((*sent)[i].body) != nil {
				t.Fatalf("block %d: reply %v, want a block_message", n, (*sent)[i].typ)
			}
			raw, err := r.Block(n)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(bm.SignedBlockBytes, raw) {
				t.Fatalf("block %d: served bytes differ from the block_log", n)
			}
		}
	})
}
