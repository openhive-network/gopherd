// Package witness implements the daemon-side block-production loop —
// the equivalent of hived's libraries/plugins/witness/witness_plugin.cpp.
//
// On every wall-clock 3-second slot boundary, the loop:
//
//   1. Asks chain.Database.ScheduledWitness(1) who's supposed to produce
//      the next slot.
//   2. Checks if that witness name is in our local key registry.
//   3. If yes: builds the block (collect mempool tx, compute merkle,
//      build signed_block_header), signs with the configured key,
//      submits to chain.Database.ApplyBlock.
//   4. If no: nothing to do — wait for the next slot.
//
// The keys are loaded from config.ini at process start. Each "witness =
// alice" + "private-key = 5K..." pair becomes a Producer entry.
package witness

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"hived-go/internal/chain"
	"hived-go/internal/crypto"
	"hived-go/internal/fcraw"
	"hived-go/internal/protocol/auth"
	"hived-go/internal/protocol/block"
	"hived-go/internal/protocol/op"
	"hived-go/internal/protocol/tx"
	"hived-go/internal/state"
	"hived-go/internal/state/objects"
)

// Broadcaster is the slice of the gossip layer the witness plugin uses
// to push freshly-produced blocks at the network. The daemon hands in a
// *gossip.Service (whose BroadcastBlock method satisfies this interface).
//
// Kept as a small interface here so the witness package never imports
// the gossip / p2p packages directly — production-only callers (e.g.
// the unit-test that just exercises buildBlock) can leave it nil.
type Broadcaster interface {
	BroadcastBlock(rawBlock []byte, blockNum uint32)
}

// BlockInterval — HIVE_BLOCK_INTERVAL = 3 seconds.
const BlockInterval = 3 * time.Second

// DryRunStaleHeadSec is how old the head block may be before the dry run stops
// measuring. Beyond this we are catching up, not following the tip, and a block
// built against a stale head measures the wrong thing. 60s ≈ 20 slots — enough
// slack that ordinary lag still gets measured.
const DryRunStaleHeadSec = 60

// DryRunStats accumulates timing + outcome for --produce-dry-run. All fields
// are guarded by Plugin.mu.
type DryRunStats struct {
	Slots      int   // slots evaluated
	Ours       int   // slots where the schedule actually named one of our producers
	Built      int   // blocks assembled + signed
	VerifyOK   int   // blocks whose signature recovered the expected key
	VerifyFail int   // blocks that failed self-verification
	Errors     int   // build failures
	TotalNs    int64 // cumulative build+sign+verify time
	MaxNs      int64 // slowest single production
	LastTxs    int   // transactions in the most recent block
	LastBytes  int   // serialized size of the most recent block
	ChainKeyOK int   // blocks signed with the witness's REGISTERED key (network would accept)
	Imperson   int   // blocks signed with a key the chain does not have for that witness
}

// Producer is one (witness account, private key) pair we can sign for.
type Producer struct {
	Account     string
	PrivateKey  *crypto.PrivateKey // secp256k1 secret
	PublicKey33 crypto.PublicKey
}

// Plugin holds the witness daemon state.
type Plugin struct {
	Chain       *chain.Database
	State       *state.Database
	Producers   []Producer
	Broadcaster Broadcaster // optional; nil = no gossip

	// DryRun exercises the whole production pipeline — schedule lookup,
	// assembly, merkle, signing, self-verification, timing — and then THROWS
	// THE BLOCK AWAY. It is never applied and never broadcast.
	//
	// Not applying is the important half. A locally produced block applied to
	// our own state would move our head to a block no one else has, i.e. fork
	// this node off the chain it is tracking. Dry run must leave state
	// untouched, so it is safe to run against mainnet.
	DryRun bool

	// DryRunEverySlot makes the dry run build a block EVERY slot instead of
	// only when the schedule names one of our producers. On mainnet we are
	// never scheduled, so without this the pipeline would never execute and
	// there would be nothing to measure. The simulated block is built for our
	// OWN producer account (header witness = us, signed by our key) so it is
	// internally consistent and self-verifiable; it is simply not the block
	// the schedule called for. Slots where the schedule really did name us are
	// counted separately (DryRunStats.Ours).
	DryRunEverySlot bool

	mu      sync.Mutex
	pending []op.Op // mempool — ops to include in our next block
	dry     DryRunStats
	// lastDrySlot dedupes dry-run production: the loop ticks every 500ms but a
	// slot is 3s, so without this one slot would yield ~6 identical blocks.
	lastDrySlot uint32

	// skippedNoKey counts slots we declined because the chain's signing key for
	// that witness is not the key we hold. Surfaced so a misconfigured witness
	// shows up as a number rather than only as a log line.
	skippedNoKey int

	// DryRunMaxHeadAgeSec overrides DryRunStaleHeadSec. 0 = use the constant.
	// Exists so tests can drive the pipeline against a genesis fixture, whose
	// head is years old and would otherwise be (correctly) skipped as stale.
	DryRunMaxHeadAgeSec int64

	logMu       sync.Mutex
	lastLogTime time.Time
	lastLogMsg  string

	stopCh chan struct{}
	doneCh chan struct{}
}

// logEvery rate-limits a stderr line to once per `interval` per distinct msg.
// Used for the heartbeat trace ("scheduled=X not owned") so a 500ms tick
// loop doesn't flood the log.
func (p *Plugin) logEvery(interval time.Duration, format string, args ...interface{}) {
	msg := fmt.Sprintf(format, args...)
	p.logMu.Lock()
	if msg != p.lastLogMsg || time.Since(p.lastLogTime) > interval {
		fmt.Fprintf(os.Stderr, "witness: %s\n", msg)
		p.lastLogMsg = msg
		p.lastLogTime = time.Now()
	}
	p.logMu.Unlock()
}

// New returns a witness plugin attached to a chain.Database.
// Producers may be empty (the daemon will follow blocks but never
// produce). Producers are usually loaded from config.ini's
// `witness = "alice"` + `private-key = "..."` lines.
func New(c *chain.Database, s *state.Database, producers []Producer) *Plugin {
	return &Plugin{
		Chain:     c,
		State:     s,
		Producers: producers,
		stopCh:    make(chan struct{}),
		doneCh:    make(chan struct{}),
	}
}

// AddPending queues a tx for inclusion in our next produced block.
// The mempool is shared across all configured producers.
func (p *Plugin) AddPending(o op.Operation) {
	p.mu.Lock()
	p.pending = append(p.pending, op.FromOperation(o))
	p.mu.Unlock()
}

// AddPendingRaw decodes a raw signed_transaction body and queues each of
// its operations into the mempool. This is the entry point used by the
// gossip layer for inbound TrxMessage bodies.
//
// LIMITATION (2.2 milestone scope): the mempool is currently a flat
// []op.Operation, not a []SignedTransaction. We re-pack the inbound ops
// into a single tx at block-build time (see buildBlock). Multi-op txs
// arriving from the network DO survive this round-trip but lose their
// original (ref_block_*, expiration, signatures) — they're re-signed
// under the producer's key. This is fine for testnet smoke flows where
// the wallet signs locally and broadcasts only to the producer's own
// daemon; it WILL need a proper mempool refactor before mainnet sync
// can run.
func (p *Plugin) AddPendingRaw(body []byte) error {
	r := fcraw.NewReader(body)
	res, err := tx.ReadSignedTransaction(r)
	if err != nil {
		return fmt.Errorf("witness.AddPendingRaw: decode: %w", err)
	}
	if !r.AtEnd() {
		return fmt.Errorf("witness.AddPendingRaw: %d trailing bytes", r.Remaining())
	}
	if len(res.SignedTx.Operations) == 0 {
		return errors.New("witness.AddPendingRaw: tx has no operations")
	}
	p.mu.Lock()
	p.pending = append(p.pending, res.SignedTx.Operations...)
	p.mu.Unlock()
	return nil
}

// Start launches the production loop in a goroutine. Returns immediately.
func (p *Plugin) Start() {
	go p.run()
}

// Stop signals the loop to exit and waits for it to finish.
func (p *Plugin) Stop() {
	close(p.stopCh)
	<-p.doneCh
}

func (p *Plugin) run() {
	defer close(p.doneCh)
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-p.stopCh:
			return
		case <-tick.C:
			if err := p.maybeProduceBlock(); err != nil {
				fmt.Fprintf(os.Stderr, "witness: %v\n", err)
			}
		}
	}
}

// maybeProduceBlock checks whether we own the next slot, and if so
// builds + applies a block. Returns nil even when we're not scheduled
// (no error); returns an actual error only when production failed.
func (p *Plugin) maybeProduceBlock() error {
	if len(p.Producers) == 0 {
		return nil // observer mode
	}
	scheduled, err := p.Chain.ScheduledWitness(1)
	if err != nil {
		return fmt.Errorf("scheduled-witness lookup: %w", err)
	}
	if scheduled == "" {
		// One-shot trace; the silent path was the #1 bug source on
		// fresh testnet (no schedule populated, scheduling formula off).
		p.logEvery(5*time.Second, "scheduled witness for slot 1 is empty")
		return nil
	}
	prod := p.findProducer(scheduled)
	scheduledOurs := prod != nil
	if prod == nil {
		// Not our turn. Under DryRunEverySlot we still exercise the pipeline,
		// building for our own producer so the block is self-consistent (see
		// the field comment). Otherwise there is nothing to do.
		if !p.DryRun || !p.DryRunEverySlot {
			p.logEvery(5*time.Second, "scheduled=%q (not owned by this node)", scheduled)
			return nil
		}
		prod = &p.Producers[0]
	}
	// All DGPO reads go through chain.Database accessors so they
	// serialize against in-flight ApplyBlock via the chain-level
	// applyMu. Direct state.NewSession(p.State) bypasses that lock and
	// races the cross-Session Aux cache that ApplyBlock mutates on
	// commit.
	headBN, _ := p.Chain.HeadBlock()
	fmt.Fprintf(os.Stderr, "witness: producing for %s (head=%d)\n", scheduled, headBN)

	// KEY GATE — the only thing standing between "log and skip" and publishing a
	// real consensus block. Compare the key we hold against the witness's
	// REGISTERED block-signing key before producing anything.
	//
	// Mirrors hived, which declines to produce when it lacks the private key for
	// the witness's signing key rather than emitting a block the network will
	// reject. Producing with the wrong key is worse than not producing: the
	// block is rejected, the slot is missed anyway, and we have spent the slot.
	//
	// Dry run skips this gate deliberately — measuring the pipeline against a
	// witness whose key we do not hold is its whole purpose, and it reports the
	// mismatch as chain-key=IMPERSONATED instead.
	if !p.DryRun && scheduledOurs {
		w, werr := objects.GetWitness(state.NewSession(p.State), auth.AccountName(prod.Account))
		switch {
		case werr != nil:
			p.logEvery(10*time.Second, "our turn (%s) but no witness object on chain — NOT producing", prod.Account)
			return nil
		case w.BlockSigningKey != prod.PublicKey33:
			// hived's equivalent: "Not producing block because I don't have the
			// private key for <key>".
			p.logEvery(10*time.Second,
				"it's our turn to produce (%s) but NO KEY MATCHES — chain expects %s, we hold %s. NOT producing.",
				prod.Account, w.BlockSigningKey, prod.PublicKey33)
			p.mu.Lock()
			p.skippedNoKey++
			p.mu.Unlock()
			return nil
		}
	}

	// Time-of-block: align to the next 3-second boundary AFTER head.
	// On a fresh chain (block 1) dgpo.Time is genesis (years ago);
	// fall back to wall-clock aligned to the next 3-second boundary so
	// produced blocks carry sane timestamps.
	dgpo, err := p.Chain.GetDGPO()
	if err != nil {
		return err
	}
	now := uint32(time.Now().Unix())
	nextSlotTime := dgpo.Time + 3
	if nextSlotTime+3600 < now {
		// More than an hour stale (fresh testnet, restart after pause):
		// jump to the CURRENT wall-clock 3-second boundary so the
		// witness immediately produces instead of waiting for the future.
		nextSlotTime = (now / 3) * 3
	}
	if nextSlotTime > now && !p.DryRun {
		return nil // not yet — wait for clock to catch up
	}
	// DRY RUN does not wait for the slot boundary. Against a live chain the
	// window where now >= nextSlotTime is only the sliver between the slot
	// instant and us RECEIVING the real block for it (~0.5s), after which
	// dgpo.Time advances and the target moves. With a 500ms tick we lose that
	// race every time, so the pipeline would never execute and there would be
	// nothing to measure — observed as 198 detected gtg slots and 0 blocks
	// built. Building the moment the schedule names us measures exactly the
	// same work; only the wall-clock trigger differs.
	//
	// Only measure at the TIP. Removing the boundary wait above also removed
	// the only thing that had been keeping the dry run off a stale head, so a
	// warm-started node built blocks during catch-up against a head minutes-to-
	// hours old, on the schedule as it stood back then — not representative of
	// live production, and it polluted the timing averages. Observed: the first
	// two gtg "slots" of a run were catch-up artifacts.
	//
	// Staleness is judged from the head block's timestamp, the same signal the
	// sync layer uses; DryRunStaleHeadSec is generous because being a few slots
	// behind is normal and still worth measuring.
	maxHeadAge := int64(DryRunStaleHeadSec)
	if p.DryRunMaxHeadAgeSec > 0 {
		maxHeadAge = p.DryRunMaxHeadAgeSec
	}
	if p.DryRun && int64(now)-int64(dgpo.Time) > maxHeadAge {
		p.logEvery(30*time.Second, "dry run idle: head is %ds old (catching up) — not measuring until at the tip",
			int64(now)-int64(dgpo.Time))
		return nil
	}
	// Dedupe by slot so a 500ms tick produces ONE block per slot, not six.
	if p.DryRun {
		p.mu.Lock()
		already := p.lastDrySlot == nextSlotTime
		if !already {
			p.lastDrySlot = nextSlotTime
		}
		p.mu.Unlock()
		if already {
			return nil
		}
	}

	// DRY RUN: build + sign + self-verify + time it, then discard. Returns
	// BEFORE any apply or broadcast — the block never touches our state and
	// never reaches the network. See Plugin.DryRun.
	if p.DryRun {
		p.dryRunProduce(prod, dgpo, nextSlotTime, scheduled, scheduledOurs)
		return nil
	}

	rawBlock, err := p.buildBlock(prod, dgpo, nextSlotTime)
	if err != nil {
		return fmt.Errorf("witness build_block: %w", err)
	}
	stats := chain.Stats{}
	postDgpo, err := p.Chain.ApplyBlock(rawBlock, dgpo.HeadBlockNumber+1, chain.ApplyOptions{
		Sync:                      true,
		SkipTransactionSignatures: true, // we just produced; no need to verify
		SkipBlockSignature:        true,
	}, &stats)
	if err != nil {
		return fmt.Errorf("witness apply_block: %w", err)
	}
	fmt.Fprintf(os.Stderr, "witness: APPLIED block %d (head=%d, time=%d)\n",
		dgpo.HeadBlockNumber+1, postDgpo.HeadBlockNumber, postDgpo.Time)
	// Drain the mempool.
	p.mu.Lock()
	p.pending = p.pending[:0]
	p.mu.Unlock()
	// Broadcast to peers. The gossip layer marks the block ID as seen
	// before fanning out so peers' echo of OUR block doesn't trigger a
	// re-apply on this node.
	if p.Broadcaster != nil {
		p.Broadcaster.BroadcastBlock(rawBlock, dgpo.HeadBlockNumber+1)
	}
	return nil
}

// dryRunProduce builds + signs a block and verifies it WITHOUT applying or
// broadcasting, recording timing. `scheduledOurs` says whether the schedule
// genuinely named this producer (vs a simulated slot under DryRunEverySlot).
//
// What this actually proves:
//   - the schedule lookup runs and names a witness
//   - a block assembles from current head + mempool, with a correct merkle root
//   - signing works and the signature recovers the producer's own key
//   - the whole thing fits inside the 3s slot budget, with the margin measured
//
// What it deliberately does NOT prove: that the block would APPLY. Validating
// that needs a state mutation we must not perform here — see DryRun.
func (p *Plugin) dryRunProduce(prod *Producer, dgpo objects.DynamicGlobalProperty, blockTime uint32, scheduled string, scheduledOurs bool) {
	start := time.Now()
	rawBlock, hdrLen, err := p.buildBlockParts(prod, dgpo, blockTime)
	if err != nil {
		p.mu.Lock()
		p.dry.Slots++
		p.dry.Errors++
		p.mu.Unlock()
		fmt.Fprintf(os.Stderr, "witness dry-run: BUILD FAILED for %s at slot %d: %v\n", prod.Account, blockTime, err)
		return
	}

	// Self-verify: recover the signer from the block signature exactly the way
	// a peer would, and confirm it is the key we signed with. A block whose
	// signature does not recover our own key is one every peer would reject.
	verifyOK := false
	verifyErr := ""
	if len(rawBlock) < hdrLen+block.SignatureSize {
		verifyErr = "block shorter than header+signature"
	} else {
		var sig crypto.CompactSignature
		copy(sig[:], rawBlock[hdrLen:hdrLen+block.SignatureSize])
		digest := block.BlockSigningDigest(rawBlock[:hdrLen])
		pub, rerr := crypto.RecoverPublicKey(digest, sig)
		switch {
		case rerr != nil:
			verifyErr = "recover: " + rerr.Error()
		case pub != prod.PublicKey33:
			verifyErr = "recovered key != producer key"
		default:
			verifyOK = true
		}
	}

	// Would the NETWORK accept this block? Self-verification above only proves
	// the signature matches the key we hold. Consensus checks it against the
	// witness's registered signing key (validate_block_header), so a key we do
	// not actually own produces a block every peer rejects.
	//
	// This is the difference between "the pipeline works" and "this is a valid
	// block", and it must be reported explicitly — impersonating a real witness
	// to exercise its schedule slots is a legitimate dry-run technique, but it
	// must never look like a successful production.
	chainKeyOK, chainKeyKnown := false, false
	if w, werr := objects.GetWitness(state.NewSession(p.State), auth.AccountName(prod.Account)); werr == nil {
		chainKeyKnown = true
		chainKeyOK = w.BlockSigningKey == prod.PublicKey33
	}

	elapsed := time.Since(start)
	// Margin against the slot budget: a block must be produced AND propagated
	// within the 3s interval, so anything approaching that is a red flag.
	margin := BlockInterval - elapsed

	p.mu.Lock()
	p.dry.Slots++
	p.dry.Built++
	if scheduledOurs {
		p.dry.Ours++
	}
	if verifyOK {
		p.dry.VerifyOK++
	} else {
		p.dry.VerifyFail++
	}
	if chainKeyOK {
		p.dry.ChainKeyOK++
	} else {
		p.dry.Imperson++
	}
	ns := elapsed.Nanoseconds()
	p.dry.TotalNs += ns
	if ns > p.dry.MaxNs {
		p.dry.MaxNs = ns
	}
	p.dry.LastBytes = len(rawBlock)
	p.dry.LastTxs = len(p.pending)
	st := p.dry
	p.mu.Unlock()

	kind := "SIMULATED"
	if scheduledOurs {
		kind = "OUR SLOT"
	}
	status := "verify=OK"
	if !verifyOK {
		status = "verify=FAIL (" + verifyErr + ")"
	}
	// Consensus verdict, kept separate from self-verification on purpose.
	switch {
	case chainKeyOK:
		status += " chain-key=OK(network would accept)"
	case chainKeyKnown:
		status += " chain-key=IMPERSONATED(network would REJECT: not this witness's signing key)"
	default:
		status += " chain-key=UNKNOWN(no witness object)"
	}
	fmt.Fprintf(os.Stderr,
		"witness dry-run: %s block %d for %s (scheduled=%s) built+signed in %.2fms (margin %.0fms of %ds) size=%dB txs=%d %s — DISCARDED [slots=%d built=%d ours=%d okay=%d fail=%d err=%d chainkey_ok=%d impersonated=%d avg=%.2fms max=%.2fms]\n",
		kind, dgpo.HeadBlockNumber+1, prod.Account, scheduled,
		float64(elapsed.Microseconds())/1000, float64(margin.Milliseconds()), int(BlockInterval/time.Second),
		len(rawBlock), st.LastTxs, status,
		st.Slots, st.Built, st.Ours, st.VerifyOK, st.VerifyFail, st.Errors, st.ChainKeyOK, st.Imperson,
		float64(st.TotalNs)/float64(max64(st.Built, 1))/1e6, float64(st.MaxNs)/1e6)
}

// SkippedNoKey returns how many slots were declined for a key mismatch.
func (p *Plugin) SkippedNoKey() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.skippedNoKey
}

// DryRunSnapshot returns a copy of the dry-run counters.
func (p *Plugin) DryRunSnapshot() DryRunStats {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.dry
}

func max64(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// findProducer returns the producer entry for `name`, or nil.
func (p *Plugin) findProducer(name string) *Producer {
	for i := range p.Producers {
		if p.Producers[i].Account == name {
			return &p.Producers[i]
		}
	}
	return nil
}

// buildBlock packs the mempool into a signed_block. The signing
// implementation is a stub that writes 65 zero bytes (replay-mode
// validity); a real signer would compute ripemd160(serialized header)
// and call secp256k1 Sign with the producer's private key.
// buildBlock assembles + signs a block. Thin wrapper over buildBlockParts for
// callers that don't need the header boundary.
func (p *Plugin) buildBlock(prod *Producer, dgpo objects.DynamicGlobalProperty, blockTime uint32) ([]byte, error) {
	raw, _, err := p.buildBlockParts(prod, dgpo, blockTime)
	return raw, err
}

// buildBlockParts is buildBlock plus the length of the UNSIGNED header, which
// the dry-run verifier needs to recompute the signing digest. The header is
// variable-length (witness name, extensions), so the boundary cannot be derived
// from the serialized block after the fact — there is no header parser.
func (p *Plugin) buildBlockParts(prod *Producer, dgpo objects.DynamicGlobalProperty, blockTime uint32) ([]byte, int, error) {
	if blockTime == 0 || prod == nil {
		return nil, 0, errors.New("buildBlock: invalid args")
	}
	hdr := block.Header{
		Previous:   dgpo.HeadBlockID,
		Timestamp:  blockTime,
		Witness:    prod.Account,
		Extensions: []byte{0},
	}

	p.mu.Lock()
	pending := append([]op.Op(nil), p.pending...)
	p.mu.Unlock()

	var txBytes []byte
	var digests []tx.Digest
	if len(pending) > 0 {
		signedTx := tx.SignedTransaction{
			Transaction: tx.Transaction{
				RefBlockNum:    uint16(dgpo.HeadBlockNumber & 0xFFFF),
				RefBlockPrefix: refBlockPrefix(dgpo.HeadBlockID),
				Expiration:     blockTime + 60,
				Operations:     pending,
				Extensions:     nil,
			},
		}
		signed := tx.AppendSignedTransaction(nil, &signedTx)
		txBytes = signed
		digests = []tx.Digest{tx.MerkleDigestFromBytes(signed)}
	}
	hdr.TransactionMerkleRoot = tx.MerkleRoot(digests)

	headerBytes := block.MarshalHeader(nil, &hdr)

	if prod.PrivateKey == nil {
		return nil, 0, errors.New("buildBlock: producer has no private key")
	}
	// Hived's digest formula (libraries/chain/full_block.cpp:142-149) is
	// SHA256 over the unsigned block-header bytes — NO chain_id prefix
	// (only transactions splice in chain_id; blocks don't). The recovered
	// pubkey from the resulting compact signature is checked against
	// witness_object::signing_key in validate_block_header.
	digest := block.BlockSigningDigest(headerBytes)
	sig, err := crypto.SignCompact(prod.PrivateKey, digest)
	if err != nil {
		return nil, 0, fmt.Errorf("buildBlock: sign: %w", err)
	}

	signedHeader := make([]byte, 0, len(headerBytes)+block.SignatureSize)
	signedHeader = append(signedHeader, headerBytes...)
	signedHeader = append(signedHeader, sig[:]...)

	out := make([]byte, 0, len(signedHeader)+1+len(txBytes))
	out = append(out, signedHeader...)
	out = appendVarUint(out, uint32(len(digests)))
	out = append(out, txBytes...)
	return out, len(headerBytes), nil
}

// refBlockPrefix extracts the prefix from a block ID — the 32-bit
// integer at offset 4 of the 20-byte SHA256 of the previous block.
// Mirrors transaction::set_reference_block.
func refBlockPrefix(blockID block.ID) uint32 {
	if blockID == (block.ID{}) {
		return 0
	}
	// Hived uses block_id_type's [4..8] as ref_block_prefix; our
	// block.ID is the 20-byte ripemd160(header) so we mirror the
	// same offset.
	return binary.LittleEndian.Uint32(blockID[4:8])
}

// appendVarUint writes a LEB128-style unsigned int to dst.
func appendVarUint(dst []byte, v uint32) []byte {
	for v >= 0x80 {
		dst = append(dst, byte(v)|0x80)
		v >>= 7
	}
	return append(dst, byte(v))
}

