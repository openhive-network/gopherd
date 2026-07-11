package api

import (
	"crypto/sha256"
	"encoding/hex"
	"sync"

	"hived-go/internal/protocol/tx"
)

// Mempool is a minimal in-memory pool of accepted-but-unconfirmed transactions.
//
// IMPORTANT: it does NOT relay to peers. broadcast_transaction accepts + parks a
// tx here but deliberately does not push it onto the network — relay is a
// network WRITE that must never happen against live mainnet (see the 2026-07-09
// incident + the never-touch-mainnet rule). Wiring relay to the gossip layer is
// a future, explicitly opt-in step gated on NOT being on mainnet.
// maxMempool bounds the number of pooled transactions. Without a cap a client
// with a single funded account could stream distinct valid (authority-passing)
// transactions and grow the pool without limit — a memory-exhaustion vector.
// When full, the oldest entry is evicted (FIFO); this pool is only consulted by
// is_known_transaction, so exact retention is not important.
const maxMempool = 10000

type Mempool struct {
	mu    sync.Mutex
	txs   map[string]tx.SignedTransaction // tx_id → tx
	order []string                        // insertion order, for FIFO eviction
}

func newMempool() *Mempool {
	return &Mempool{txs: make(map[string]tx.SignedTransaction, 256), order: make([]string, 0, 256)}
}

// Add stores a tx by its id. Returns whether it was new. Evicts the oldest entry
// when the pool is at capacity.
func (m *Mempool) Add(id string, st tx.SignedTransaction) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.txs[id]; ok {
		return false
	}
	if len(m.order) >= maxMempool {
		oldest := m.order[0]
		m.order = m.order[1:]
		delete(m.txs, oldest)
	}
	m.txs[id] = st
	m.order = append(m.order, id)
	return true
}

// Has reports whether a tx id is pooled.
func (m *Mempool) Has(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.txs[id]
	return ok
}

// txID computes the transaction_id = first 20 bytes of sha256(unsigned body), hex.
func txID(unsigned []byte) string {
	h := sha256.Sum256(unsigned)
	return hex.EncodeToString(h[:20])
}
