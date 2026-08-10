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
	"hived-go/internal/protocol/tx"
	"hived-go/internal/state"
	"hived-go/internal/state/objects"
)

// Publisher hands a freshly produced block to the node's single-writer apply
// path, which applies it (reversibly, through the fork-choice engine) and then
// advertises it to peers by block ID. The daemon hands in the p2p sync
// delegate; *sync.Delegate.PublishBlock satisfies this.
//
// The witness deliberately does NOT apply or broadcast the block itself. Both
// shortcuts were mainnet bugs, diagnosed at block 108887429 (howo's slot,
// 2026-08-10) — see sync.Delegate.PublishBlock for the full write-up:
//
//   - Applying directly (chain.ApplyBlock, non-reversible) made our own block
//     the COMMITTED head, so fork choice could never undo it. When the network
//     chose a different block for that height we wedged on our own orphan.
//   - Broadcasting directly pushed an unsolicited block_message body, which
//     hived rejects and disconnects over, so the block never propagated.
//
// Kept as a small interface here so the witness package never imports the p2p
// packages directly; unit tests that only exercise buildBlock leave it nil.
type Publisher interface {
	PublishBlock(rawBlock []byte, blockNum uint32) error
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
	Chain     *chain.Database
	State     *state.Database
	Producers []Producer
	// Publisher applies + advertises produced blocks. REQUIRED for real
	// production: maybeProduceBlock refuses to build a block it has no way to
	// publish, rather than producing one that never leaves this process.
	Publisher Publisher

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
	// pending is the mempool: whole signed transactions, held as their VERBATIM
	// wire bytes.
	//
	// It used to be a flat []op.Op — inbound transactions were torn apart and
	// their operations re-packed into one new transaction at build time, which
	// DISCARDED THE SIGNATURES. A transaction's signatures cover its own
	// serialized bytes, so the repacked transaction carried other accounts'
	// operations with no authority at all; any block containing one would be
	// rejected outright. (That is why every block we produced was empty: nothing
	// ever reached this field, which masked the defect.)
	pending []pendingTx
	// pendingIDs mirrors pending's ids for O(1) dedup. Kept in lockstep with
	// pending on every add/remove. The linear scan it replaces ran under the
	// mempool mutex on EVERY offered transaction — with 12 peers advertising the
	// same transactions, that was ~68k scans over a queue of ~6k entries, all
	// contending with the production path.
	pendingIDs map[[20]byte]struct{}
	dry        DryRunStats
	// lastDrySlot dedupes dry-run production: the loop ticks every 500ms but a
	// slot is 3s, so without this one slot would yield ~6 identical blocks.
	lastDrySlot uint32

	// lastSlotAnnounced dedupes the "our slot" line to once per head, since the
	// production loop re-enters several times within a single 3s slot.
	lastSlotAnnounced uint32

	// skippedNoKey counts slots we declined because the chain's signing key for
	// that witness is not the key we hold. Surfaced so a misconfigured witness
	// shows up as a number rather than only as a log line.
	skippedNoKey int

	// lastSelect describes the most recent transaction selection, reported with
	// the PRODUCED line so an empty block explains itself on the spot.
	lastSelect string

	// overflowed counts transactions dropped to make room at the cap. Non-zero
	// is normal on a busy chain; a HIGH rate means the cap is too small for the
	// unmineable-transaction backlog and fresh arrivals are being churned.
	overflowed int

	// evicted counts transactions dropped because the chain mined them. Lives
	// HERE rather than in the p2p layer: eviction moved into chain.ApplyBlock,
	// and leaving the counter behind in the retired p2p hook silently reported
	// evict=0 while the mechanism itself was untested — measurement must move
	// with the thing it measures.
	evicted int

	// produced counts blocks we built and successfully handed to the Publisher.
	// This is "submitted for publication", NOT "accepted by the network" — the
	// two diverged silently on 2026-08-10, when a produced block was applied
	// locally, rejected by every peer, and still looked like a success here.
	produced int

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

// pendingTx is one mempool entry: the transaction exactly as it arrived, plus
// the fields we need to age it out and de-duplicate it.
type pendingTx struct {
	raw        []byte   // VERBATIM wire bytes — never re-encode these
	id         [20]byte // transaction id, for dedup
	expiration uint32
}

// MaxMempoolTx bounds how many transactions we hold. Mempool contents are
// attacker-controlled and unbounded growth is a trivial memory exhaustion of a
// witness node. A full block is ~65 KB, so this is far more than any block can
// consume while staying cheap to hold.
const MaxMempoolTx = 8000

// AddPendingTx queues an already-validated raw signed transaction.
// Returns false if it was a duplicate or the mempool is full.
func (p *Plugin) AddPendingTx(raw []byte, id [20]byte, expiration uint32) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.pendingIDs == nil {
		p.pendingIDs = make(map[[20]byte]struct{}, 1024)
	}
	if _, dup := p.pendingIDs[id]; dup {
		return false
	}
	// FULL: drop the OLDEST entry rather than refusing the newest.
	//
	// Refusing was the wrong end. Roughly half the transactions the network
	// gossips are never mined by anyone (failed RC, superseded intent, spam);
	// they are valid at admission, so they sit until expiry, and Hive
	// transactions commonly carry long expirations. Measured live: ~164/min
	// accumulating, which at a 1h expiry settles near 9800 — above this cap.
	//
	// A queue that refuses new arrivals when full holds only STALE transactions,
	// and stale transactions are exactly the ones already mined or unmineable.
	// We would fill up and then produce empty blocks forever. Dropping the
	// oldest keeps the window that actually matters: the most recent arrivals,
	// which are the ones still includable.
	for len(p.pending) >= MaxMempoolTx {
		oldest := p.pending[0]
		p.pending = p.pending[1:]
		delete(p.pendingIDs, oldest.id)
		p.overflowed++
	}
	cp := make([]byte, len(raw))
	copy(cp, raw)
	p.pending = append(p.pending, pendingTx{raw: cp, id: id, expiration: expiration})
	p.pendingIDs[id] = struct{}{}
	return true
}

// RemoveIncludedTxIDs evicts transactions that some other witness has already
// put in a block. Called for every block we apply.
//
// Without this the mempool is write-only: every transaction we admit is mined
// by another witness within seconds, nothing removes it, and by our slot the
// entire queue is duplicates that pre-flight rejects — so we produce an EMPTY
// block while the network is busy. That is exactly what happened on mainnet
// 2026-08-10 (blocks 108891828 and 108907971, both empty beside neighbours
// carrying 13-41 transactions, with depth climbing 598->1082 monotonically).
//
// It also stops the queue reaching MaxMempoolTx, where it would refuse fresh
// transactions and make the emptiness permanent.
func (p *Plugin) RemoveIncludedTxIDs(ids [][20]byte) int {
	if len(ids) == 0 {
		return 0
	}
	dead := make(map[[20]byte]struct{}, len(ids))
	for _, id := range ids {
		dead[id] = struct{}{}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	before := len(p.pending)
	kept := p.pending[:0]
	for _, t := range p.pending {
		if _, gone := dead[t.id]; gone {
			delete(p.pendingIDs, t.id)
			continue
		}
		kept = append(kept, t)
	}
	p.pending = kept
	return before - len(p.pending)
}

// RemoveIncludedTxID drops one transaction that just entered the chain's dedup
// window. Fired from chain.ApplyBlock for EVERY apply path — direct, drained
// from the reorder buffer, or re-applied during a reorg — which is what makes
// eviction complete. The p2p-side hook it replaces only saw blocks that passed
// back through sync and covered ~46% of mined transactions, leaving the queue
// growing ~356/min toward the cap.
//
// Runs under the chain's apply lock: keep it O(1) and never call back into the
// chain.
func (p *Plugin) RemoveIncludedTxID(id [20]byte) {
	p.mu.Lock()
	if _, present := p.pendingIDs[id]; !present {
		p.mu.Unlock()
		return
	}
	p.evicted++
	delete(p.pendingIDs, id)
	kept := p.pending[:0]
	for _, t := range p.pending {
		if t.id == id {
			continue
		}
		kept = append(kept, t)
	}
	p.pending = kept
	p.mu.Unlock()
}

// EvictedCount reports how many queued transactions the chain has mined out
// from under us.
func (p *Plugin) EvictedCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.evicted
}

// OverflowedCount reports how many transactions were dropped at the cap.
func (p *Plugin) OverflowedCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.overflowed
}

// PendingCount reports the current mempool depth.
func (p *Plugin) PendingCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.pending)
}

// dropExpired removes transactions whose expiration has passed relative to
// `now` (the chain head's timestamp). Without this, transactions the network
// will never accept again accumulate forever and are re-offered every slot.
func (p *Plugin) dropExpired(now uint32) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	kept := p.pending[:0]
	dropped := 0
	for _, t := range p.pending {
		if t.expiration <= now {
			delete(p.pendingIDs, t.id)
			dropped++
			continue
		}
		kept = append(kept, t)
	}
	p.pending = kept
	return dropped
}

// removeIncluded drops transactions we just put in a block, plus any the
// selector rejected for a permanent reason (bad signature, failed pre-flight,
// malformed) — retrying those every slot would be pure waste. Transactions
// rejected only for space are kept for the next block.
func (p *Plugin) removeIncluded(dead map[[20]byte]struct{}) {
	if len(dead) == 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	kept := p.pending[:0]
	for _, t := range p.pending {
		if _, gone := dead[t.id]; gone {
			delete(p.pendingIDs, t.id)
			continue
		}
		kept = append(kept, t)
	}
	p.pending = kept
}

// AddPendingRaw admits a raw signed_transaction from the network into the
// mempool. This is the entry point the p2p layer uses for inbound TrxMessage
// bodies.
//
// The transaction is stored VERBATIM. Its signatures cover its own serialized
// bytes, so it must reach the block byte-for-byte as it arrived — the previous
// implementation unpacked the operations and re-signed them under the
// producer's key, which silently stripped the real authorities.
//
// Admission runs the cheap checks (decode, authority, expiration/dedup/TaPOS).
// It deliberately does NOT apply the transaction: state moves between admission
// and our slot, so the decisive check is the trial apply in
// chain.SelectPendingTransactions at build time.
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
	if p.Chain != nil {
		if verr := p.Chain.ValidateIncomingTx(body); verr != nil {
			return fmt.Errorf("witness.AddPendingRaw: rejected: %w", verr)
		}
	}
	txID := tx.IDFromDigest(tx.DigestFromBytes(body[res.UnsignedStart:res.UnsignedEnd]))
	if !p.AddPendingTx(body, txID, res.SignedTx.Expiration) {
		// Duplicate, or the queue is full. Reported distinctly so the accepted
		// counter means UNIQUE admissions: with 12 peers advertising the same
		// transactions, counting duplicates as accepted showed acc=68767 beside
		// depth=6266 and made the queue look far healthier than it was.
		return ErrDuplicateTx
	}
	return nil
}

// duplicateTxError marks "already queued (or queue full)" — ordinary traffic,
// not a fault, since every transaction is offered by several peers.
//
// It reports itself through a METHOD rather than a sentinel so the generic p2p
// layer can recognise it without importing this plugin: p2p/sync is transport
// and must not depend on a consensus plugin.
type duplicateTxError struct{}

func (duplicateTxError) Error() string      { return "witness: transaction already pending" }
func (duplicateTxError) IsDuplicateTx() bool { return true }

// ErrDuplicateTx is the value AddPendingRaw returns for a duplicate.
var ErrDuplicateTx error = duplicateTxError{}

// Start launches the production loop in a goroutine. Returns immediately.
func (p *Plugin) Start() {
	go p.run()
}

// Stop signals the loop to exit and waits for it to finish.
func (p *Plugin) Stop() {
	close(p.stopCh)
	<-p.doneCh
}

// maxProducePoll bounds how long the production loop sleeps when the next slot
// boundary is further away than this. The boundary itself is always hit exactly
// (see nextWake), but we must keep checking in between: the boundary check can
// find us not yet ready — most often because the PREVIOUS block has not landed,
// so dgpo still trails — and a loop that only woke on boundaries would then
// have to wait a further full slot. Retrying costs nothing; missing a slot
// costs a block.
const maxProducePoll = 250 * time.Millisecond

// nextWake returns how long to sleep before the next production check: the time
// remaining to the next block-interval boundary, capped at maxProducePoll.
//
// hived arms a timer for the exact slot boundary. The old loop here polled on a
// fixed 500ms ticker, which could start work up to 500ms late — ~17% of the 3s
// slot budget burned before assembling anything. Capping the sleep preserves
// frequent retries while the final sleep of each slot lands us ON the boundary.
func nextWake(now time.Time) time.Duration {
	slot := int64(BlockInterval)
	ns := now.UnixNano()
	untilBoundary := time.Duration(((ns/slot)+1)*slot - ns)
	if untilBoundary < maxProducePoll {
		return untilBoundary
	}
	return maxProducePoll
}

func (p *Plugin) run() {
	defer close(p.doneCh)
	timer := time.NewTimer(nextWake(time.Now()))
	defer timer.Stop()
	// Sweep expired transactions on a cadence, not only when we build. A backup
	// witness builds every 20-40 minutes, so expiry-at-build alone leaves dead
	// transactions occupying the queue (and being re-validated) for the whole
	// gap — and the queue is capped, so dead entries crowd out live ones.
	sweep := time.NewTicker(30 * time.Second)
	defer sweep.Stop()
	for {
		select {
		case <-p.stopCh:
			return
		case <-sweep.C:
			p.sweepExpired()
		case <-timer.C:
			if err := p.maybeProduceBlock(); err != nil {
				fmt.Fprintf(os.Stderr, "witness: %v\n", err)
			}
			timer.Reset(nextWake(time.Now()))
		}
	}
}

// sweepExpired drops transactions the chain can no longer accept, using the
// head block's timestamp as "now" (the same clock validateTxPreFlight judges
// expiration against — wall-clock would disagree with consensus).
func (p *Plugin) sweepExpired() {
	if p.Chain == nil {
		return
	}
	dgpo, err := p.Chain.GetDGPO()
	if err != nil {
		return
	}
	if n := p.dropExpired(dgpo.Time); n > 0 {
		p.logEvery(time.Minute, "mempool: swept %d expired transaction(s), depth=%d", n, p.PendingCount())
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
	// Announce the slot ONCE, and truthfully.
	//
	// This used to be an unconditional "producing for X" printed right here —
	// before the key gate, before the publisher check, and before the
	// slot-boundary wait, any of which can decline. So it announced production
	// that frequently never happened (e.g. every slot where the chain's
	// registered key is not ours), and because the loop re-enters every 250ms
	// against a 3s slot it printed the same false claim about a dozen times per
	// slot. Report what is actually known at this point — the schedule named us
	// — and let the outcome lines below report what was decided.
	if p.lastSlotAnnounced != headBN {
		p.lastSlotAnnounced = headBN
		fmt.Fprintf(os.Stderr, "witness: our slot at head %d (schedule names %s) — evaluating\n", headBN, scheduled)
	}

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

	// Refuse to build a block we cannot publish. Producing into the void would
	// still consume the slot and — worse, before this was routed through the
	// apply path — would move our head to a block no peer has. An unwired
	// publisher is a configuration error, so say so loudly instead of looking
	// like a working witness.
	if p.Publisher == nil {
		p.logEvery(10*time.Second,
			"our turn (%s) but NO PUBLISHER wired — refusing to produce a block that could not be published",
			prod.Account)
		return nil
	}

	rawBlock, err := p.buildBlock(prod, dgpo, nextSlotTime)
	if err != nil {
		return fmt.Errorf("witness build_block: %w", err)
	}
	blockNum := dgpo.HeadBlockNumber + 1

	// Hand off to the node's single-writer apply path. It applies the block
	// REVERSIBLY via the fork-choice engine (so the network can still out-vote
	// us at this height and we can reorg off it) and advertises it to peers by
	// block ID, which is the only publication mechanism hived accepts.
	//
	// We do NOT apply it here. See the Publisher doc comment: doing either half
	// of this locally is what cost howo block 108887429.
	if err := p.Publisher.PublishBlock(rawBlock, blockNum); err != nil {
		return fmt.Errorf("witness publish_block %d: %w", blockNum, err)
	}
	p.mu.Lock()
	selInfo := p.lastSelect
	p.mu.Unlock()
	if selInfo == "" {
		selInfo = "mempool empty"
	}
	fmt.Fprintf(os.Stderr, "witness: PRODUCED block %d for %s (slot time=%d, %d bytes) [select: %s] — submitted for apply + inventory advertisement\n",
		blockNum, prod.Account, nextSlotTime, len(rawBlock), selInfo)
	p.mu.Lock()
	p.produced++
	p.pending = p.pending[:0] // drain the mempool
	p.mu.Unlock()
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

// Produced returns how many blocks were built and handed to the Publisher.
// Whether the network kept them is a separate question — check the witness's
// last_confirmed_block_num on chain.
func (p *Plugin) Produced() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.produced
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
// maxTxBytes is the byte budget for transactions in a block: the chain's
// current maximum_block_size less headroom for the signed header, the
// transaction-count varint and the header extensions. Overshooting the limit
// produces a block the network rejects, so the headroom is deliberately
// generous — losing a few hundred bytes of capacity is free, losing the slot is
// not.
const blockOverheadBytes = 512

func (p *Plugin) maxTxBytes(_ objects.DynamicGlobalProperty) int {
	max := 0
	if p.State != nil {
		if ws, err := objects.GetWitnessSchedule(state.NewSession(p.State)); err == nil {
			max = int(ws.MedianMaximumBlockSize)
		}
	}
	if max <= blockOverheadBytes {
		// Chain params not populated (fresh fixture) — fall back to the protocol
		// minimum rather than producing a zero-capacity block.
		max = 65536
	}
	return max - blockOverheadBytes
}

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

	// Age out transactions the chain can no longer accept before selecting, so
	// they neither occupy the mempool nor get re-offered every slot.
	p.dropExpired(dgpo.Time)

	// NEWEST FIRST. The queue is FIFO, and ~half of it is transactions nobody
	// will ever mine, which pile up at the OLD end. Evaluating oldest-first
	// spends the bounded selection budget on the least includable candidates
	// and can fill the slot without filling the block.
	p.mu.Lock()
	n := len(p.pending)
	candidates := make([][]byte, n)
	ids := make([][20]byte, n)
	for i := range p.pending {
		candidates[n-1-i] = p.pending[i].raw
		ids[n-1-i] = p.pending[i].id
	}
	p.mu.Unlock()

	var txBytes []byte
	var digests []tx.Digest
	if len(candidates) > 0 && p.Chain != nil {
		// Ask the chain which of these actually APPLY against head state. A
		// block containing a transaction that fails to apply is an invalid
		// block — the network drops it and we miss the slot — so nothing goes
		// in on the strength of admission checks alone.
		selStart := time.Now()
		sel, serr := p.Chain.SelectPendingTransactions(
			candidates, dgpo.HeadBlockNumber+1, blockTime, prod.Account, p.maxTxBytes(dgpo))
		if serr != nil {
			return nil, 0, fmt.Errorf("buildBlock: select pending: %w", serr)
		}
		// Report what selection actually did. Selection trial-APPLIES every
		// candidate while holding the apply lock, inside a 3s slot budget, so
		// both the outcome mix and the cost belong in the log: "0 included out
		// of N" is the empty-block signature, and a slow selection would eat the
		// slot itself.
		byReason := map[string]int{}
		for _, why := range sel.Rejected {
			byReason[why.String()]++
		}
		info := fmt.Sprintf("candidates=%d included=%d bytes=%d in %.1fms rejected=%v",
			len(candidates), len(sel.Accepted), sel.Bytes,
			float64(time.Since(selStart).Microseconds())/1000.0, byReason)
		p.mu.Lock()
		p.lastSelect = info
		p.mu.Unlock()
		for _, raw := range sel.Accepted {
			txBytes = append(txBytes, raw...)
		}
		digests = sel.Digests

		// Evict what we included, plus anything rejected for a PERMANENT
		// reason. Transactions skipped only for space stay for the next block.
		dead := make(map[[20]byte]struct{}, len(sel.Accepted)+len(sel.Rejected))
		accepted := make(map[string]struct{}, len(sel.Accepted))
		for _, raw := range sel.Accepted {
			accepted[string(raw)] = struct{}{}
		}
		for i := range candidates {
			if _, ok := accepted[string(candidates[i])]; ok {
				dead[ids[i]] = struct{}{}
				continue
			}
			if why, rejected := sel.Rejected[i]; rejected && why != chain.PendingTooLarge {
				dead[ids[i]] = struct{}{}
			}
		}
		p.removeIncluded(dead)
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

