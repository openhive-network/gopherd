package sync

import (
	"sync"

	"hived-go/internal/crypto"
	"hived-go/internal/protocol/block"
)

// signerCache caches the result of RecoverBlockSigner keyed by block.ID.
// During catch-up the SAME block frequently arrives from multiple peers
// (we let duplicate fetch_items through to avoid stalling on the
// slowest peer). Without this cache, every duplicate triggers another
// expensive secp256k1 recovery on the per-peer receive goroutine,
// wasting ~5x CPU when all 8 peers deliver the same chunk.
//
// Capacity is bounded — we evict the oldest half whenever we cross
// signerCacheCap entries. The sync layer's applyLoop will see each
// block exactly once anyway; the cache is purely to dedupe the
// pre-recovery work across peer-receive goroutines.
type signerCache struct {
	mu    sync.RWMutex
	cap   int
	order []block.ID
	keys  map[block.ID]crypto.PublicKey
}

const signerCacheCap = 8192

func newSignerCache() *signerCache {
	return &signerCache{
		cap:   signerCacheCap,
		order: make([]block.ID, 0, signerCacheCap),
		keys:  make(map[block.ID]crypto.PublicKey, signerCacheCap),
	}
}

func (c *signerCache) get(id block.ID) (crypto.PublicKey, bool) {
	c.mu.RLock()
	k, ok := c.keys[id]
	c.mu.RUnlock()
	return k, ok
}

func (c *signerCache) put(id block.ID, k crypto.PublicKey) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.keys[id]; exists {
		return
	}
	if len(c.keys) >= c.cap {
		// Evict the oldest 1/4. Keeps the eviction batched so we don't
		// rebalance on every insert.
		drop := c.cap / 4
		if drop < 1 {
			drop = 1
		}
		for i := 0; i < drop && i < len(c.order); i++ {
			delete(c.keys, c.order[i])
		}
		c.order = append(c.order[:0], c.order[drop:]...)
	}
	c.order = append(c.order, id)
	c.keys[id] = k
}

// RecoverBlockSigner parses the signed_block header, derives the witness
// signing digest, and recovers the public key from the witness signature.
// Returns the zero PublicKey on any parse or recovery error — callers
// (handleBlock) treat that as "fall through to chain.ApplyBlock's own
// recovery path", which will reproduce the same failure with a clean
// error message.
//
// This is the look-ahead pre-recovery hook called from per-peer receive
// goroutines. With N connected peers we get N-way parallelism on the
// expensive secp256k1 recovery, leaving the single applyLoop free to
// focus on the actual state mutation. Without this pre-recovery, sig
// verification was the dominant CPU cost in the apply path
// (~47 % of CPU samples on a fast catch-up).
func RecoverBlockSigner(rawBlock []byte) crypto.PublicKey {
	return recoverBlockSignerCached(rawBlock, nil, block.ID{})
}

func recoverBlockSignerCached(rawBlock []byte, cache *signerCache, hintID block.ID) crypto.PublicKey {
	if cache != nil && hintID != (block.ID{}) {
		if k, ok := cache.get(hintID); ok {
			return k
		}
	}
	hdr, hdrLen, err := block.UnmarshalSignedHeader(rawBlock)
	if err != nil {
		return crypto.PublicKey{}
	}
	if hdrLen < block.SignatureSize {
		return crypto.PublicKey{}
	}
	unsignedHeader := rawBlock[:hdrLen-block.SignatureSize]
	digest := block.BlockSigningDigest(unsignedHeader)
	var sig crypto.CompactSignature
	copy(sig[:], hdr.WitnessSignature[:])
	if !sig.IsCanonical() {
		return crypto.PublicKey{}
	}
	got, err := crypto.RecoverPublicKey(digest, sig)
	if err != nil {
		return crypto.PublicKey{}
	}
	if cache != nil && hintID != (block.ID{}) {
		cache.put(hintID, got)
	}
	return got
}
