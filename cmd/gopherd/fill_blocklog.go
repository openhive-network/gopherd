package main

// fill-blocklog puts back blocks missing from the middle of a block_log.
//
// The block_log is indexed by position, so one missing block leaves every
// later block one slot early. Each entry names its predecessor, so a gap
// shows as an entry whose previous is not the entry before it; that previous
// id is the missing block, which we fetch from peers by id, check against the
// id and its merkle root, and insert. The entries from the first gap on are
// staged to a file next to the block_log before it is cut back and rewritten,
// so an interrupted run can be redone from the stage.

import (
	"bufio"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"gopherd/internal/blocklog"
	blockzstd "gopherd/internal/blocklog/zstd"
	"gopherd/internal/chain"
	p2pnode "gopherd/internal/p2p/node"
	"gopherd/internal/p2p/proto"
	"gopherd/internal/protocol/block"
	"gopherd/internal/protocol/network"
)

// blocklogGap is a run of blocks missing before block_log position pos: the
// entry at pos follows want, and want is not the block at pos-1.
type blocklogGap struct {
	pos   uint32
	after block.ID // the block at pos-1
	want  block.ID // the entry at pos names this as its previous
}

func cmdFillBlocklog(args []string) int {
	fs := flag.NewFlagSet("fill-blocklog", flag.ContinueOnError)
	from := fs.Uint("from", 0, "first block_log position to check (the entry before it must be correct)")
	seeds := fs.String("seeds", strings.Join(network.MainnetSeeds, ","), "comma-separated peers to fetch the missing blocks from")
	timeout := fs.Duration("timeout", 3*time.Minute, "how long to wait for peers to serve the missing blocks")
	write := fs.Bool("write", false, "rewrite the block_log (without it, only report the gaps and fetch the blocks)")
	verbose := fs.Bool("verbose", false, "log peer connections and messages")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 || *from < 1 {
		fmt.Fprintln(os.Stderr, "usage: gopherd fill-blocklog --from=N [--write] [--seeds=host:port,...] <block_log>")
		return 2
	}
	path := fs.Arg(0)
	r, err := blocklog.Open(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fill-blocklog: %v\n", err)
		return 1
	}
	gaps, err := findBlocklogGaps(r, uint32(*from))
	if err != nil {
		r.Close()
		fmt.Fprintf(os.Stderr, "fill-blocklog: %v\n", err)
		return 1
	}
	if len(gaps) == 0 {
		r.Close()
		fmt.Printf("no gaps in %d..%d\n", *from, r.Head())
		return 0
	}
	for _, g := range gaps {
		fmt.Printf("gap before position %d: blocks %d..%d missing (the entry there follows %x)\n",
			g.pos, g.after.Num()+1, g.want.Num(), g.want[:])
	}
	missing, err := fetchGapBlocks(gaps, strings.Split(*seeds, ","), *timeout, *verbose)
	if err != nil {
		r.Close()
		fmt.Fprintf(os.Stderr, "fill-blocklog: %v\n", err)
		return 1
	}
	if !*write {
		r.Close()
		fmt.Println("fetched every missing block; rerun with --write to rewrite the block_log")
		return 0
	}
	if err := rewriteBlocklogTail(path, r, gaps, missing); err != nil {
		fmt.Fprintf(os.Stderr, "fill-blocklog: %v\n", err)
		return 1
	}
	return 0
}

// findBlocklogGaps walks positions from..head and reports each place where an
// entry does not follow the one before it. An entry that is older than its
// predecessor (a repeat or a fork block) is an error: that is not a gap.
func findBlocklogGaps(r *blocklog.Reader, from uint32) ([]blocklogGap, error) {
	var prev block.ID
	if from > 1 {
		raw, err := r.Block(from - 1)
		if err != nil {
			return nil, err
		}
		sh, hdrLen, err := block.UnmarshalSignedHeader(raw)
		if err != nil {
			return nil, fmt.Errorf("position %d: %w", from-1, err)
		}
		prev = block.ConstructID(raw[:hdrLen], sh.Previous.Num()+1)
	}
	var gaps []blocklogGap
	for n := from; n <= r.Head(); n++ {
		raw, err := r.Block(n)
		if err != nil {
			return nil, err
		}
		sh, hdrLen, err := block.UnmarshalSignedHeader(raw)
		if err != nil {
			return nil, fmt.Errorf("position %d: %w", n, err)
		}
		if sh.Previous != prev {
			if sh.Previous.Num() <= prev.Num() {
				return nil, fmt.Errorf("position %d follows block %d, at or below the block before it (%d): a repeated or forked entry, not a gap",
					n, sh.Previous.Num(), prev.Num())
			}
			gaps = append(gaps, blocklogGap{pos: n, after: prev, want: sh.Previous})
		}
		prev = block.ConstructID(raw[:hdrLen], sh.Previous.Num()+1)
	}
	return gaps, nil
}

// fetchGapBlocks fetches every block of every gap, walking back from each
// gap's want until it links to its after. Returns the raw blocks of each gap
// in chain order, keyed by gap position.
func fetchGapBlocks(gaps []blocklogGap, seeds []string, timeout time.Duration, verbose bool) (map[uint32][][]byte, error) {
	f := newBlockFetcher(seeds, verbose)
	defer f.stop()
	deadline := time.Now().Add(timeout)
	out := make(map[uint32][][]byte, len(gaps))
	for _, g := range gaps {
		var chainBack [][]byte
		want := g.want
		for want != g.after {
			if want.Num() <= g.after.Num() {
				return nil, fmt.Errorf("gap before position %d: the fetched blocks lead to block %d, not to %x", g.pos, want.Num(), g.after[:])
			}
			raw, err := f.get(want, deadline)
			if err != nil {
				return nil, err
			}
			fmt.Printf("fetched block %d (%x, %d bytes)\n", want.Num(), want[:], len(raw))
			chainBack = append(chainBack, raw)
			sh, _, _ := block.UnmarshalSignedHeader(raw)
			want = sh.Previous
		}
		for i, j := 0, len(chainBack)-1; i < j; i, j = i+1, j-1 {
			chainBack[i], chainBack[j] = chainBack[j], chainBack[i]
		}
		out[g.pos] = chainBack
	}
	return out, nil
}

// blockFetcher asks every connected peer for the blocks it is waiting for and
// keeps the first copy that checks out.
type blockFetcher struct {
	node    *p2pnode.Node
	verbose bool
	zstd    *blockzstd.Decompressor
	mu      sync.Mutex
	want    map[block.ID]chan []byte
	got     map[block.ID][]byte
}

func newBlockFetcher(seeds []string, verbose bool) *blockFetcher {
	f := &blockFetcher{verbose: verbose, zstd: blockzstd.NewDecompressor(), want: map[block.ID]chan []byte{}, got: map[block.ID][]byte{}}
	var logger io.Writer = io.Discard
	if verbose {
		logger = os.Stderr
	}
	f.node = p2pnode.New(p2pnode.Config{
		SeedNodes:        seeds,
		UserAgent:        "gopherd/0.1.0 fill-blocklog",
		MaxOutboundPeers: 8,
		Logger:           logger,
	}, headZero{}, f)
	_ = f.node.Start()
	return f
}

func (f *blockFetcher) stop() { f.node.Stop() }

// get returns the block with id, asking the connected peers until deadline.
func (f *blockFetcher) get(id block.ID, deadline time.Time) ([]byte, error) {
	f.mu.Lock()
	if raw, ok := f.got[id]; ok {
		f.mu.Unlock()
		return raw, nil
	}
	ch := make(chan []byte, 1)
	f.want[id] = ch
	f.mu.Unlock()
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	f.ask(id)
	for {
		select {
		case raw := <-ch:
			return raw, nil
		case <-t.C:
			if time.Now().After(deadline) {
				return nil, fmt.Errorf("no peer served block %d (%x) in time", id.Num(), id[:])
			}
			f.ask(id) // peers that connected since
		}
	}
}

func (f *blockFetcher) ask(id block.ID) {
	for _, p := range f.node.Peers() {
		f.sendFetch(p, id)
	}
}

func (f *blockFetcher) sendFetch(p *p2pnode.Peer, ids ...block.ID) {
	m := proto.FetchItemsMessage{ItemType: proto.ItemTypeBlock}
	for _, id := range ids {
		m.ItemsToFetch = append(m.ItemsToFetch, proto.ItemHash(id))
	}
	if body, err := m.Marshal(); err == nil {
		_ = p.Send(proto.FetchItemsMessageType, body)
	}
}

func (f *blockFetcher) OnPeerConnected(p *p2pnode.Peer) {
	f.mu.Lock()
	ids := make([]block.ID, 0, len(f.want))
	for id := range f.want {
		ids = append(ids, id)
	}
	f.mu.Unlock()
	if len(ids) > 0 {
		f.sendFetch(p, ids...)
	}
}

// OnBlockReceived takes a block_message body: the signed block followed by
// its 20-byte id.
func (f *blockFetcher) OnBlockReceived(_ *p2pnode.Peer, body []byte) {
	if len(body) > block.IDSize {
		f.keep(body[:len(body)-block.IDSize])
	}
}

// OnMessage takes the compressed_block_message hived peers answer a block
// fetch with.
func (f *blockFetcher) OnMessage(_ *p2pnode.Peer, t proto.MessageType, body []byte) {
	if f.verbose {
		fmt.Fprintf(os.Stderr, "fill-blocklog: message %d (%d bytes)\n", t, len(body))
	}
	if t != proto.CompressedBlockMessageType {
		return
	}
	var m proto.CompressedBlockMessage
	if err := m.Unmarshal(body); err != nil {
		return
	}
	raw := m.CompressedBytes
	if m.Zstd {
		var err error
		if m.HasDictionary {
			raw, err = f.zstd.Decompress(m.CompressedBytes, m.DictionaryNumber, nil)
		} else {
			raw, err = f.zstd.DecompressNoDict(m.CompressedBytes, nil)
		}
		if err != nil {
			return
		}
	}
	f.keep(raw)
}

// keep stores a block we asked for once its id and merkle root check out.
func (f *blockFetcher) keep(raw []byte) {
	sh, hdrLen, err := block.UnmarshalSignedHeader(raw)
	if err != nil {
		return
	}
	num := sh.Previous.Num() + 1
	id := block.ConstructID(raw[:hdrLen], num)
	f.mu.Lock()
	defer f.mu.Unlock()
	ch, ok := f.want[id]
	if !ok || chain.CheckMerkleRoot(raw, num) != nil {
		return
	}
	delete(f.want, id)
	f.got[id] = append([]byte(nil), raw...)
	ch <- f.got[id]
}

func (f *blockFetcher) OnPeerDisconnected(*p2pnode.Peer, error)     {}
func (f *blockFetcher) OnTransactionReceived(*p2pnode.Peer, []byte) {}
func (f *blockFetcher) OnAddressesReceived(*p2pnode.Peer, []proto.AddressInfo) {
}
func (f *blockFetcher) BuildAddressReply(*p2pnode.Peer) []proto.AddressInfo { return nil }

// headZero is the chain view of a node that holds no blocks.
type headZero struct{}

func (headZero) HeadBlock() (uint32, error) { return 0, nil }

// rewriteBlocklogTail stages the entries from the first gap on, cuts the
// block_log back to just before it, and appends them again with the missing
// blocks in their places.
func rewriteBlocklogTail(path string, r *blocklog.Reader, gaps []blocklogGap, missing map[uint32][][]byte) error {
	first, head := gaps[0].pos, r.Head()
	start, _, err := r.BlockExtent(first)
	if err != nil {
		return err
	}
	stagePath := fmt.Sprintf("%s.fill-%d-%d", path, first, head)
	if err := stageBlocklogEntries(r, first, head, stagePath); err != nil {
		r.Close()
		return err
	}
	r.Close()
	fmt.Printf("staged positions %d..%d in %s\n", first, head, stagePath)

	if err := os.Truncate(path, int64(start)); err != nil {
		return err
	}
	if err := os.Truncate(path+".index", int64(first-1)*8); err != nil {
		return err
	}
	w, err := blocklog.OpenForWrite(path, true)
	if err != nil {
		return err
	}
	if w.HeadBlock() != first-1 {
		w.Close()
		return fmt.Errorf("the cut block_log ends at %d, want %d", w.HeadBlock(), first-1)
	}
	err = forEachStagedEntry(stagePath, func(pos uint32, payload []byte, attr blocklog.Attributes) error {
		for _, raw := range missing[pos] {
			if err := w.Append(raw); err != nil {
				return err
			}
		}
		return w.AppendRaw(payload, attr)
	})
	if cerr := w.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("%w (the original entries are in %s)", err, stagePath)
	}

	r2, err := blocklog.Open(path)
	if err != nil {
		return err
	}
	defer r2.Close()
	if gaps, err := findBlocklogGaps(r2, first); err != nil || len(gaps) > 0 {
		return fmt.Errorf("the rewritten block_log still does not link up (gaps %v, err %v); the original entries are in %s", gaps, err, stagePath)
	}
	fmt.Printf("block_log now ends at %d and links up from %d; the stage %s can be deleted\n", r2.Head(), first-1, stagePath)
	return nil
}

// stageBlocklogEntries writes positions from..to as stored (still compressed)
// records: 1 byte flags, 1 byte dictionary, 4 bytes length, payload.
func stageBlocklogEntries(r *blocklog.Reader, from, to uint32, stagePath string) error {
	f, err := os.Create(stagePath)
	if err != nil {
		return err
	}
	bw := bufio.NewWriterSize(f, 1<<20)
	for n := from; n <= to; n++ {
		payload, attr, err := r.RawBlockBytes(n)
		if err != nil {
			f.Close()
			return err
		}
		var hdr [6]byte
		if attr.Compressed {
			hdr[0] |= 1
		}
		if attr.HasDictNum {
			hdr[0] |= 2
		}
		hdr[1] = attr.DictionaryNum
		binary.LittleEndian.PutUint32(hdr[2:], uint32(len(payload)))
		bw.Write(hdr[:])
		bw.Write(payload)
	}
	if err := bw.Flush(); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// forEachStagedEntry reads a stage back; pos counts from the stage's first
// position, which the file name records.
func forEachStagedEntry(stagePath string, fn func(pos uint32, payload []byte, attr blocklog.Attributes) error) error {
	var first, last uint32
	if i := strings.LastIndex(stagePath, ".fill-"); i < 0 {
		return fmt.Errorf("stage %s: no position range in the name", stagePath)
	} else if _, err := fmt.Sscanf(stagePath[i:], ".fill-%d-%d", &first, &last); err != nil {
		return fmt.Errorf("stage %s: %w", stagePath, err)
	}
	f, err := os.Open(stagePath)
	if err != nil {
		return err
	}
	defer f.Close()
	br := bufio.NewReaderSize(f, 1<<20)
	pos := first
	for ; ; pos++ {
		var hdr [6]byte
		if _, err := io.ReadFull(br, hdr[:]); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return err
		}
		payload := make([]byte, binary.LittleEndian.Uint32(hdr[2:]))
		if _, err := io.ReadFull(br, payload); err != nil {
			return err
		}
		attr := blocklog.Attributes{Compressed: hdr[0]&1 != 0, HasDictNum: hdr[0]&2 != 0, DictionaryNum: hdr[1]}
		if err := fn(pos, payload, attr); err != nil {
			return err
		}
	}
	if pos != last+1 {
		return fmt.Errorf("stage %s holds positions %d..%d, want up to %d", stagePath, first, pos-1, last)
	}
	return nil
}
