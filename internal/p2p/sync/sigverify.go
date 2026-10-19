package sync

import (
	"sync"

	"gopherd/internal/chain"
	"gopherd/internal/crypto"
	"gopherd/internal/protocol/block"
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

// txSigCache memoizes the per-tx signer-pubkey set for a freshly-decoded
// block, keyed by block.ID. Mirrors signerCache's role for the witness
// signature: when the same block arrives from multiple peers during
// catch-up, we only fan-out the per-tx secp256k1 recovery once. The
// stored value is the slice slot returned by chain.RecoverBlockTxSignerKeys
// — already shaped to drop straight into ApplyOptions.PreRecoveredTxKeys.
//
// Capacity is bounded by the same eviction strategy as signerCache (drop
// oldest 1/4 when full). Each cached value is a slice of slices of
// 33-byte PublicKeys, up to ~14 KB per block. It is only filled for
// non-historical blocks, where copies arrive within seconds and a fork
// switch re-pushes at most the fork database's window (~66 blocks), so 256
// blocks (~13 min) is plenty. The old 8192 took 6.8 h at the head to fill
// and then held ~100 MB.
type txSigCache struct {
	mu    sync.RWMutex
	cap   int
	order []block.ID
	keys  map[block.ID][][]crypto.PublicKey
}

const txSigCacheCap = 256

func newTxSigCache() *txSigCache {
	return &txSigCache{
		cap:   txSigCacheCap,
		order: make([]block.ID, 0, txSigCacheCap),
		keys:  make(map[block.ID][][]crypto.PublicKey, txSigCacheCap),
	}
}

func (c *txSigCache) get(id block.ID) ([][]crypto.PublicKey, bool) {
	c.mu.RLock()
	v, ok := c.keys[id]
	c.mu.RUnlock()
	return v, ok
}

func (c *txSigCache) put(id block.ID, v [][]crypto.PublicKey) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.keys[id]; exists {
		return
	}
	if len(c.keys) >= c.cap {
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
	c.keys[id] = v
}

// recoverTxKeysCached fans out per-tx signer recovery (parallel, internal
// to chain.RecoverBlockTxSignerKeys) and memoizes the result so duplicate
// block deliveries during catch-up don't re-pay the recovery cost.
func recoverTxKeysCached(rawBlock []byte, blockNum uint32, cache *txSigCache, hintID block.ID) [][]crypto.PublicKey {
	if cache != nil && hintID != (block.ID{}) {
		if v, ok := cache.get(hintID); ok {
			return v
		}
	}
	res := chain.RecoverBlockTxSignerKeys(rawBlock, blockNum)
	if cache != nil && hintID != (block.ID{}) && res.TxKeys != nil {
		cache.put(hintID, res.TxKeys)
	}
	return res.TxKeys
}
