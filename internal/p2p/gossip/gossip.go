// Package gossip is the OUTBOUND broadcast half of the daemon's p2p
// wiring. It hands inbound transactions to the local mempool and
// re-fans-out blocks once they've been applied locally — but it does
// NOT itself drain inbound blocks into chain.Database.ApplyBlock.
//
// Why not? chain.Database.ApplyBlock is single-writer (one shared
// *state.Session). The agent-sync delegate already owns the only
// goroutine that may legally call ApplyBlock; gossip used to have its
// own apply loop too, which raced sync's on every block (concurrent map
// writes against state.Session.Aux). The fix is to delete gossip's
// apply path: sync's MultiDelegate sibling drains EVERY inbound block,
// gossip's OnBlockReceived is a no-op, and gossip only fires on the
// OUTBOUND side — local production (witness plugin) and post-apply
// re-broadcast (driven by sync's applyLoop).
//
// Responsibilities now:
//
//   - Implement node.Delegate. Inbound TrxMessage bodies hit
//     OnTransactionReceived (mempool + re-broadcast). Inbound blocks
//     are claimed by sync; gossip's OnBlockReceived is a no-op.
//   - Originate. The witness plugin calls BroadcastBlock once it has
//     locally applied its produced block; gossip marks the block ID
//     as seen (so the inevitable peer-echo of OUR own block is dropped
//     by the per-peer dedup) and fans the body out to every peer.
//   - Re-broadcast. After sync's applyLoop successfully applies an
//     inbound block, it invokes BroadcastAppliedBlock here (wired via
//     sync.Config.OnBlockApplied). We fan the block out to every peer
//     OTHER than the one we got it from. The recipient's gossip layer
//     dedups its own seenSet; even without per-block source tracking
//     loops would still terminate at the recipient's chain head check,
//     but excluding the source saves one hop of bandwidth per peer.
//   - Tx dedup. Inbound transaction IDs go through seenTx so we don't
//     re-broadcast the same tx forever. Block dedup is now done in two
//     places: gossip's seenBlk (for locally-produced blocks, so we
//     don't double-broadcast on the apply hook) and sync's per-peer
//     known set (for inbound).
package gossip

import (
	"time"

	"hived-go/internal/chain"
	"hived-go/internal/crypto"
	p2pnode "hived-go/internal/p2p/node"
	"hived-go/internal/p2p/proto"
	"hived-go/internal/protocol/block"
)

// liveBroadcastWindowSec bounds how far behind wall-clock a block may be
// and still be gossiped. Blocks are 3s apart; a node receiving live
// blocks is at most a few seconds behind, while catch-up (initial-sync)
// blocks are hours-to-years old. Re-broadcasting catch-up blocks to peers
// — who are at the live head — gets us dropped ("you offered me a block
// deemed invalid"), so we relay only blocks within this window. 120s
// (~40 blocks) is a generous live margin that still excludes every
// catch-up block.
const liveBroadcastWindowSec = 120

// nowUnix is overridable in tests that feed historical-timestamped blocks
// through the live broadcast path.
var nowUnix = func() int64 { return time.Now().Unix() }

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
	// Chain is retained for source-compatibility with callers that still
	// pass it; gossip no longer reads from it (the apply path lives in
	// p2p/sync now). Safe to leave nil.
	Chain *chain.Database
	// Node is the p2p Node to broadcast / re-broadcast on. Required.
	Node *p2pnode.Node
	// Mempool optional — if nil, inbound tx messages are dropped
	// (still ack'd as "seen" so they don't echo forever).
	Mempool MempoolAdder
	// Logger optional — if nil, falls back to Node.Logf via NewService.
	Logger Logger
	// DedupCapacity caps the number of remembered block IDs / tx IDs.
	// When the cap is reached, oldest entries are evicted. Default 4096.
	DedupCapacity int
}

// Service is the live gossip wiring. One per daemon.
type Service struct {
	cfg    Config
	logger Logger

	seenBlk *seenSet
	seenTx  *seenSet
}

// New builds a Service. There are no background goroutines to start;
// the apply path now lives in p2p/sync.
func New(cfg Config) *Service {
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
		seenBlk: newSeenSet(cfg.DedupCapacity),
		seenTx:  newSeenSet(cfg.DedupCapacity),
	}
}

// Start is a no-op preserved for backward compatibility with callers
// that still wrap Service in a Start/Stop lifecycle. Gossip no longer
// owns any goroutines.
func (s *Service) Start() {}

// Stop is a no-op preserved for backward compatibility.
func (s *Service) Stop() {}

// BroadcastBlock is called by the witness plugin once it has locally
// applied its newly-produced block. The block ID is marked seen so we
// don't double-broadcast when the chain.SetOnBlockApplied hook fires
// from inside ApplyBlock for the same body, and so peers' inevitable
// echo of our own block doesn't trigger a re-apply.
//
// rawBlock is JUST the signed_block bytes (no trailing block_id);
// the gossip layer derives the block_id from the header itself.
func (s *Service) BroadcastBlock(rawBlock []byte, blockNum uint32) {
	id, ok := blockIDFromHeader(rawBlock, blockNum)
	if !ok {
		s.logger.Logf("gossip: broadcast — local block header parse failed")
		return
	}
	if !s.seenBlk.add(id) {
		// Already broadcast (e.g. apply hook fired first).
		return
	}
	body := buildBlockMessageBody(rawBlock, id)
	s.cfg.Node.Broadcast(proto.BlockMessageType, body, nil)
}

// BroadcastAppliedBlock is wired into sync.Config.OnBlockApplied. It
// fires from sync's applyLoop AFTER a successful ApplyBlock; we fan
// the block out to every peer other than `source`. `source == nil`
// indicates a locally-produced block (in which case BroadcastBlock has
// usually already broadcast it; this call short-circuits via the
// seenBlk dedup).
func (s *Service) BroadcastAppliedBlock(rawBlock []byte, blockNum uint32, source *p2pnode.Peer) {
	id, ok := blockIDFromHeader(rawBlock, blockNum)
	if !ok {
		s.logger.Logf("gossip: re-broadcast block %d — header parse failed", blockNum)
		return
	}
	// Only relay LIVE blocks. During initial catch-up sync we apply
	// millions of historical blocks fetched from peers; re-broadcasting
	// them to peers already at the live head gets us dropped. Locally
	// produced blocks (source==nil) are live by definition and skip the
	// gate; received blocks (source!=nil) must be within the live window.
	if source != nil {
		if ts := blockTimestampHeader(rawBlock); ts != 0 && int64(ts) < nowUnix()-liveBroadcastWindowSec {
			return
		}
	}
	if !s.seenBlk.add(id) {
		// We've already broadcast this body (e.g. local witness path
		// already fired BroadcastBlock above).
		return
	}
	body := buildBlockMessageBody(rawBlock, id)
	s.cfg.Node.Broadcast(proto.BlockMessageType, body, source)
}

// blockTimestampHeader extracts the timestamp field (offset 20, 4 bytes
// LE) from a signed_block_header prefix. Returns 0 if the buffer is
// too short. Used as a cheap "is this block historical?" gate.
func blockTimestampHeader(rawBlock []byte) uint32 {
	if len(rawBlock) < 24 {
		return 0
	}
	return uint32(rawBlock[20]) |
		uint32(rawBlock[21])<<8 |
		uint32(rawBlock[22])<<16 |
		uint32(rawBlock[23])<<24
}

// blockIDFromHeader derives the canonical 20-byte block_id from the
// signed_block bytes' header range and the consumer-known block_num.
func blockIDFromHeader(rawBlock []byte, blockNum uint32) (block.ID, bool) {
	_, hdrLen, err := block.UnmarshalSignedHeader(rawBlock)
	if err != nil {
		return block.ID{}, false
	}
	return block.ConstructID(rawBlock[:hdrLen], blockNum), true
}

// buildBlockMessageBody returns a fresh BlockMessage body =
// signed_block_bytes || 20-byte block_id, ready to hand to Node.Broadcast.
func buildBlockMessageBody(rawBlock []byte, id block.ID) []byte {
	body := make([]byte, 0, len(rawBlock)+block.IDSize)
	body = append(body, rawBlock...)
	body = append(body, id[:]...)
	return body
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

// OnBlockReceived is a NO-OP. Inbound blocks are claimed by the sync
// delegate's OnBlockReceived (sibling under the same MultiDelegate),
// which drains them through its single applyLoop. Having gossip ALSO
// apply blocks would race sync on chain.Database's shared session.
//
// On apply success, sync invokes BroadcastAppliedBlock to fan the
// block out to peers — that re-broadcast is the only outbound role
// gossip plays for inbound traffic now.
func (s *Service) OnBlockReceived(p *p2pnode.Peer, body []byte) {}

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
