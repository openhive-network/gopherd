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
	"hived-go/internal/protocol/block"
	"hived-go/internal/protocol/op"
	"hived-go/internal/protocol/tx"
	"hived-go/internal/state"
	"hived-go/internal/state/objects"
)

// BlockInterval — HIVE_BLOCK_INTERVAL = 3 seconds.
const BlockInterval = 3 * time.Second

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

	mu      sync.Mutex
	pending []op.Operation // mempool — ops to include in our next block

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
	p.pending = append(p.pending, o)
	p.mu.Unlock()
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
	if prod == nil {
		p.logEvery(5*time.Second, "scheduled=%q (not owned by this node)", scheduled)
		return nil // not our turn
	}
	headBN, _ := func() (uint32, error) {
		d, e := objects.GetDGPO(state.NewSession(p.State))
		return d.HeadBlockNumber, e
	}()
	fmt.Fprintf(os.Stderr, "witness: producing for %s (head=%d)\n", scheduled, headBN)

	// Time-of-block: align to the next 3-second boundary AFTER head.
	// On a fresh chain (block 1) dgpo.Time is genesis (years ago);
	// fall back to wall-clock aligned to the next 3-second boundary so
	// produced blocks carry sane timestamps.
	dgpo, err := objects.GetDGPO(state.NewSession(p.State))
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
	if nextSlotTime > now {
		return nil // not yet — wait for clock to catch up
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
	return nil
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
func (p *Plugin) buildBlock(prod *Producer, dgpo objects.DynamicGlobalProperty, blockTime uint32) ([]byte, error) {
	if blockTime == 0 || prod == nil {
		return nil, errors.New("buildBlock: invalid args")
	}
	hdr := block.Header{
		Previous:   dgpo.HeadBlockID,
		Timestamp:  blockTime,
		Witness:    prod.Account,
		Extensions: []byte{0},
	}

	p.mu.Lock()
	pending := append([]op.Operation(nil), p.pending...)
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
		return nil, errors.New("buildBlock: producer has no private key")
	}
	// Hived's digest formula (libraries/chain/full_block.cpp:142-149) is
	// SHA256 over the unsigned block-header bytes — NO chain_id prefix
	// (only transactions splice in chain_id; blocks don't). The recovered
	// pubkey from the resulting compact signature is checked against
	// witness_object::signing_key in validate_block_header.
	digest := block.BlockSigningDigest(headerBytes)
	sig, err := crypto.SignCompact(prod.PrivateKey, digest)
	if err != nil {
		return nil, fmt.Errorf("buildBlock: sign: %w", err)
	}

	signedHeader := make([]byte, 0, len(headerBytes)+block.SignatureSize)
	signedHeader = append(signedHeader, headerBytes...)
	signedHeader = append(signedHeader, sig[:]...)

	out := make([]byte, 0, len(signedHeader)+1+len(txBytes))
	out = append(out, signedHeader...)
	out = appendVarUint(out, uint32(len(digests)))
	out = append(out, txBytes...)
	return out, nil
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

