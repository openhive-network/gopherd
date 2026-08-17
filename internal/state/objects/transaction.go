package objects

import (
	"errors"
	"fmt"

	"gopherd/internal/state"
)

// TransactionTable is the dedup window of recently-applied transactions.
// Mirrors hive::chain::transaction_object (libraries/chain/include/hive/
// chain/detail/state/transaction_object.hpp).
//
// Hived rejects a transaction whose `trx_id` already lives in the
// window — protects against accidental rebroadcasts AND replay attacks.
// Entries are pruned per-block once `expiration < head_block_time`.
const TransactionTable state.TableID = 26

// TransactionByExpirationIndex orders entries by expiration ASC then
// trx_id, so prune-expired iterations can stop at the first non-expired
// row. Key: [expiration_BE_4][trx_id_20]
const TransactionByExpirationIndex state.IndexID = 1

// TransactionRecord is one entry: trx_id is the primary key (20 bytes
// from the SHA-256 of the canonical transaction encoding, truncated as
// per the C++ block_id_type / transaction_id_type alias).
type TransactionRecord struct {
	TrxID      [20]byte
	Expiration uint32
}

func transactionKey(trxID [20]byte) []byte {
	out := make([]byte, 20)
	copy(out, trxID[:])
	return out
}

func transactionByExpirationKey(expiration uint32, trxID [20]byte) []byte {
	out := state.AppendUint32BE(nil, expiration)
	return append(out, trxID[:]...)
}

func AppendTransactionRecord(dst []byte, r *TransactionRecord) []byte {
	dst = append(dst, r.TrxID[:]...)
	dst = appendU32LE(dst, r.Expiration)
	return dst
}

func ReadTransactionRecord(src []byte) (TransactionRecord, error) {
	var r TransactionRecord
	if len(src) < 24 {
		return r, fmt.Errorf("transaction_object: short read (%d bytes)", len(src))
	}
	copy(r.TrxID[:], src[:20])
	r.Expiration = uint32(src[20]) | uint32(src[21])<<8 |
		uint32(src[22])<<16 | uint32(src[23])<<24
	if len(src) > 24 {
		return r, errors.New("transaction_object: trailing bytes")
	}
	return r, nil
}

// PutTransactionRecord inserts (trx_id, expiration) into the dedup
// window. Caller is responsible for checking HasTransaction first if
// duplicates should be rejected. Routes through the typed
// TransactionStore (in-memory map + by-exp btree) — no K/V overlay.
func PutTransactionRecord(s *state.Session, r *TransactionRecord) {
	EnsureTransactionStore(s).Put(r.TrxID, r.Expiration)
}

// HasTransaction reports whether trx_id is already in the window.
func HasTransaction(s *state.Session, trxID [20]byte) (bool, error) {
	return EnsureTransactionStore(s).Has(trxID), nil
}

// DeleteTransactionRecord removes both index entries.
func DeleteTransactionRecord(s *state.Session, r *TransactionRecord) {
	EnsureTransactionStore(s).Delete(r.TrxID)
}

// PruneExpiredTransactions drops every dedup-window entry whose
// expiration ≤ now. Mirrors hive::chain::database::clear_expired_transactions
// (database.cpp::clear_expired_transactions). Bounds the working set
// to the rolling window (typically 1 hour).
func PruneExpiredTransactions(s *state.Session, now uint32) {
	EnsureTransactionStore(s).PruneExpired(now)
}
