// Package gossip wires the p2p Node's per-peer message stream into the
// daemon's chain.Database and witness mempool. It is the "what to do
// with an inbound block / tx" half of the Phase-2.2 milestone.
//
// Responsibilities
//
//   - Implement node.Delegate. Inbound BlockMessage / TrxMessage bodies
//     come in here; outbound peer life-cycle events are forwarded.
//   - Apply inbound blocks SERIALLY. chain.Database.ApplyBlock is not
//     goroutine-safe (single shared *state.Session); a buffered queue
//     plus a single drain goroutine guarantees that.
//   - Dedup. Each block (and each tx) is identified by a 20-byte hash.
//     Once we've seen one, we drop subsequent copies — both for "don't
//     apply twice" and "don't echo forever". The dedup window is a
//     simple LRU map sized at gossipDedupCapacity.
//   - Re-broadcast. On successful apply we forward the block to every
//     OTHER peer (the sender is excluded so traffic doesn't bounce
//     between us and them on every block). For tx messages we also
//     re-broadcast on successful AddPending.
//   - Originate. The witness plugin calls BroadcastBlock once it has
//     locally applied its produced block. The gossip layer marks the
//     block ID as seen so the inevitable peer-echo of OUR OWN block
//     is silently dropped instead of re-applied.
//
// Inventory + catch-up sync (item_ids_inventory exchange, blockchain
// synopsis, fetch_items round-trips) are NOT here — that's agent-sync's
// territory. This layer assumes peers gossip every freshly-produced
// block on its first hop and that catch-up needs are out of scope for
// the 3-node testnet smoke test.
package gossip

import (
	"sync"

	"hived-go/internal/chain"
	"hived-go/internal/crypto"
	p2pnode "hived-go/internal/p2p/node"
	"hived-go/internal/p2p/proto"
	"hived-go/internal/protocol/block"
)

// Logger is a minimal logging surface so the gossip layer doesn't have
// to import os/fmt directly. The daemon hands in *p2pnode.Node.Logf.
type Logger interface {
	Logf(format string, args ...interface{})
}

// MempoolAdder is the slice of *witness.Plugin we use for inbound
// transactions. Kept narrow so circular imports stay impossible.
type MempoolAdder interface {
	// AddPendingRaw queues a raw signed_transaction body for inclusion
	// in the next produced block. May return an error if the tx fails
	// validation; in that case the gossip layer drops the message
	// without re-broadcasting.
	AddPendingRaw(body []byte) error
}

// Config tunes a Service.
type Config struct {
	// Chain is the chain.Database to ApplyBlock against. Required.
	Chain *chain.Database
	// Node is the p2p Node to broadcast / re-broadcast on. Required.
	Node *p2pnode.Node
	// Mempool optional — if nil, inbound tx messages are dropped
	// (still ack'd as "seen" so they don't echo forever).
	Mempool MempoolAdder
	// Logger optional — if nil, falls back to Node.Logf via NewService.
	Logger Logger
	// QueueSize sets the buffered-channel depth between the peer
	// goroutines and the single ApplyBlock drain. Default 256.
	QueueSize int
	// DedupCapacity caps the number of remembered block IDs / tx IDs.
	// When the cap is reached, oldest entries are evicted. Default 4096.
	DedupCapacity int
}

// Service is the live gossip wiring. One per daemon.
type Service struct {
	cfg    Config
	logger Logger

	blockQ chan inboundBlock

	mu       sync.Mutex
	seenBlk  *seenSet
	seenTx   *seenSet
	stopOnce sync.Once
	stopCh   chan struct{}
	doneCh   chan struct{}
}

// inboundBlock pairs a raw block body with the peer it arrived from
// (so we can re-broadcast to everyone EXCEPT the sender). origin == nil
// means "the local witness produced this block".
type inboundBlock struct {
	body   []byte
	origin *p2pnode.Peer
}

// New builds an unstarted *Service. Call Start to launch the apply loop.
func New(cfg Config) *Service {
	if cfg.QueueSize <= 0 {
		cfg.QueueSize = 256
	}
	if cfg.DedupCapacity <= 0 {
		cfg.DedupCapacity = 4096
	}
	logger := cfg.Logger
	if logger == nil {
		logger = cfg.Node
	}
	return &Service{
		cfg:     cfg,
		logger:  logger,
		blockQ:  make(chan inboundBlock, cfg.QueueSize),
		seenBlk: newSeenSet(cfg.DedupCapacity),
		seenTx:  newSeenSet(cfg.DedupCapacity),
		stopCh:  make(chan struct{}),
		doneCh:  make(chan struct{}),
	}
}

// Start launches the single apply goroutine.
func (s *Service) Start() {
	go s.applyLoop()
}

// Stop signals the apply goroutine to exit and waits for it to drain.
func (s *Service) Stop() {
	s.stopOnce.Do(func() {
		close(s.stopCh)
	})
	<-s.doneCh
}

// applyLoop is the single drainer for inbound blocks. ApplyBlock is
// called only from this goroutine, ensuring chain.Database's shared
// session stays single-writer.
func (s *Service) applyLoop() {
	defer close(s.doneCh)
	for {
		select {
		case <-s.stopCh:
			return
		case ib := <-s.blockQ:
			s.applyOne(ib)
		}
	}
}

// applyOne handles one queued inbound block: parse the BlockMessage
// envelope, dedup, ApplyBlock, re-broadcast on success.
func (s *Service) applyOne(ib inboundBlock) {
	rawBlock, blockID, ok := splitBlockMessage(ib.body)
	if !ok {
		s.logger.Logf("gossip: drop malformed block (%d bytes)", len(ib.body))
		return
	}
	// Dedup. Mark the ID seen FIRST so concurrent inbound copies of the
	// same block (which can race past the queue from multiple peers in
	// quick succession) collapse to one apply.
	if !s.seenBlk.add(blockID) {
		// Already seen — silently drop.
		return
	}

	headBN, err := s.cfg.Chain.HeadBlock()
	if err != nil {
		s.logger.Logf("gossip: head_block lookup: %v", err)
		return
	}
	nextBN := headBN + 1
	stats := chain.Stats{}
	_, err = s.cfg.Chain.ApplyBlock(rawBlock, nextBN, chain.ApplyOptions{
		Sync:               true,
		SkipBlockSignature: false, // real verification on the inbound path
	}, &stats)
	if err != nil {
		// Apply failure could be "duplicate block we already had via
		// another path" or "out-of-order arrival" or "real bad block".
		// Don't disconnect — log + drop.
		s.logger.Logf("gossip: apply block %d failed: %v", nextBN, err)
		return
	}
	s.logger.Logf("gossip: applied block %d from peer %s", nextBN, peerName(ib.origin))
	// Re-broadcast to everyone OTHER than the sender. ib.origin == nil
	// means the local witness originated this block — broadcast to all.
	s.cfg.Node.Broadcast(proto.BlockMessageType, ib.body, ib.origin)
}

// splitBlockMessage cracks a BlockMessage body into (signed_block, block_id).
// The body layout (proto.BlockMessage.Marshal) is signed_block_bytes
// concatenated with a trailing 20-byte block_id.
func splitBlockMessage(body []byte) (rawBlock []byte, id block.ID, ok bool) {
	if len(body) < block.IDSize {
		return nil, block.ID{}, false
	}
	bodyEnd := len(body) - block.IDSize
	rawBlock = body[:bodyEnd]
	copy(id[:], body[bodyEnd:])
	return rawBlock, id, true
}

// BroadcastBlock is called by the witness plugin once it has locally
// applied its newly-produced block. The block ID is marked seen so
// peers' inevitable echo of our own block doesn't trigger a re-apply,
// and the body is fanned out to every peer.
//
// rawBlock is JUST the signed_block bytes (no trailing block_id);
// the gossip layer derives the block_id from the header itself.
func (s *Service) BroadcastBlock(rawBlock []byte, blockNum uint32) {
	// Parse the signed-header byte range so we can compute the block_id.
	_, hdrLen, err := block.UnmarshalSignedHeader(rawBlock)
	if err != nil {
		s.logger.Logf("gossip: broadcast — local block header parse: %v", err)
		return
	}
	id := block.ConstructID(rawBlock[:hdrLen], blockNum)
	s.seenBlk.add(id)

	// BlockMessage on the wire = signed_block_bytes + 20-byte block_id.
	body := make([]byte, 0, len(rawBlock)+block.IDSize)
	body = append(body, rawBlock...)
	body = append(body, id[:]...)
	s.cfg.Node.Broadcast(proto.BlockMessageType, body, nil)
}

// --- node.Delegate ---

// OnPeerConnected — no-op for the gossip layer.
func (s *Service) OnPeerConnected(p *p2pnode.Peer) {}

// OnPeerDisconnected — no-op.
func (s *Service) OnPeerDisconnected(p *p2pnode.Peer, err error) {}

// OnMessage — no-op (handshake / heartbeat / inventory are handled
// elsewhere).
func (s *Service) OnMessage(p *p2pnode.Peer, t proto.MessageType, body []byte) {}

// OnAddressesReceived — no-op; the discovery layer feeds the peerdb.
func (s *Service) OnAddressesReceived(p *p2pnode.Peer, addrs []proto.AddressInfo) {}

// BuildAddressReply — no-op; let the Node assemble its own reply
// (loggingDelegate.BuildAddressReply routes through n.buildAddressReply).
func (s *Service) BuildAddressReply(p *p2pnode.Peer) []proto.AddressInfo { return nil }

// OnBlockReceived enqueues the block for application + re-broadcast on
// the apply goroutine. We DO NOT apply inline — peers' read loops would
// block until ApplyBlock returned, freezing heartbeat traffic in the
// process.
func (s *Service) OnBlockReceived(p *p2pnode.Peer, body []byte) {
	// Cheap pre-dedup: if we've already seen this block, drop here so
	// the queue doesn't fill with redundant copies during a fan-out
	// storm. The drain loop's add() is the authoritative gate; this
	// is a load-shedder.
	if _, id, ok := splitBlockMessage(body); ok && s.seenBlk.contains(id) {
		return
	}
	select {
	case s.blockQ <- inboundBlock{body: body, origin: p}:
	default:
		// Queue full — drop. The block will be re-gossipped by some
		// other peer on the next hop. Better to drop than to stall the
		// peer goroutine (which would also stall heartbeats).
		s.logger.Logf("gossip: drop block from %s — apply queue full", peerName(p))
	}
}

// OnTransactionReceived dedupes and pushes the tx body into the local
// mempool, then re-broadcasts to everyone except the sender. Mempool
// validation failures (bad tx) are logged + dropped; the tx is still
// remembered as seen so the network doesn't echo it forever.
func (s *Service) OnTransactionReceived(p *p2pnode.Peer, body []byte) {
	id := txID(body)
	if !s.seenTx.add(id) {
		return
	}
	if s.cfg.Mempool != nil {
		if err := s.cfg.Mempool.AddPendingRaw(body); err != nil {
			s.logger.Logf("gossip: tx from %s rejected: %v", peerName(p), err)
			return
		}
	}
	s.cfg.Node.Broadcast(proto.TrxMessageType, body, p)
}

// txID is the dedup key for a TrxMessage body. We use the first 20
// bytes of SHA-256(body); hived's id() helper does the same kind of
// fixed-prefix derivation off ripemd160. We're not interoperating with
// hived's fetch_items machinery here so any deterministic 20-byte
// digest works.
func txID(body []byte) block.ID {
	d := crypto.SHA256(body)
	var id block.ID
	copy(id[:], d[:block.IDSize])
	return id
}

// peerName produces a short human-readable label for log lines.
// "(local)" indicates a block we produced ourselves.
func peerName(p *p2pnode.Peer) string {
	if p == nil {
		return "(local)"
	}
	return p.Conn().RemoteAddr().String()
}
