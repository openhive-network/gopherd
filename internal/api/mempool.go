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
type Mempool struct {
	mu  sync.Mutex
	txs map[string]tx.SignedTransaction // tx_id → tx
}

func newMempool() *Mempool { return &Mempool{txs: make(map[string]tx.SignedTransaction, 256)} }

// Add stores a tx by its id. Returns the id and whether it was new.
func (m *Mempool) Add(id string, st tx.SignedTransaction) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.txs[id]; ok {
		return false
	}
	m.txs[id] = st
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
