package objects

import (
	"encoding/binary"
	"fmt"
	"io"

	"github.com/tidwall/btree"

	"hived-go/internal/state"
)

// TransactionStore is the in-memory typed container for the dedup-window
// of recently-applied transactions. Replaces the K/V path entirely.
//
// Why typed-store: every applied tx writes two K/V rows (primary +
// by-expiration) and every block prunes expired entries via Range. At
// 4M blocks that was ~8M Session.Put calls and per-block Range scans
// against the K/V overlay — together a large slice of the remaining
// Session.Put alloc pressure (~2 GB).
//
// Invariants
//
//   - primary[trxID] == expiration  (presence = "tx is in the dedup window")
//   - byExp is a btree keyed by (expiration, trxID) for ordered
//     iteration during prune-expired (first non-expired entry stops
//     the scan, matching the C++ by_expiration prune).
//   - undoLog captures pre-mutation state for Snapshot/Restore.
type TransactionStore struct {
	db          *state.Database
	primary     map[[20]byte]uint32
	byExp       *btree.BTreeG[txExpKey]
	hintByExp   btree.PathHint
	undoLog     []txUndo
	// pruneBuf is reused across PruneExpired calls to avoid the
	// per-call slice growth that escaped through the btree.Scan closure
	// (180 MB cum on a 4M-block run).
	pruneBuf []txExpKey
	// pruneCb is a pre-allocated callback context whose closure captures
	// only a stable interior pointer ({pruneBuf, now} on the store) —
	// keeps the .Scan callback alloc-free per call. Initialized lazily
	// on first use to avoid changing the constructor surface.
	pruneCb pruneCallback
}

type pruneCallback struct {
	exp *[]txExpKey
	now uint32
	cb  func(k txExpKey) bool
}

// txExpKey is the by-expiration secondary key. Ordering: expiration
// ascending, then trxID ascending (lex on raw bytes).
type txExpKey struct {
	exp   uint32
	trxID [20]byte
}

func txExpLess(a, b txExpKey) bool {
	if a.exp != b.exp {
		return a.exp < b.exp
	}
	// Lex compare trxIDs as byte arrays.
	for i := 0; i < 20; i++ {
		if a.trxID[i] != b.trxID[i] {
			return a.trxID[i] < b.trxID[i]
		}
	}
	return false
}

type txUndo struct {
	trxID  [20]byte
	exp    uint32 // pre-mutation expiration when prevHad=true
	prevHad bool  // true = the row existed before the mutation
	op      uint8 // 0=Put, 1=Delete
}

const transactionStoreSubsystemKey = "transaction_store"

// EnsureTransactionStore returns the database's TransactionStore.
func EnsureTransactionStore(s *state.Session) *TransactionStore {
	db := s.Database()
	if existing := db.GetSubsystem(transactionStoreSubsystemKey); existing != nil {
		if t, ok := existing.(*TransactionStore); ok {
			return t
		}
	}
	t := newTransactionStore()
	t.db = db
	db.RegisterSubsystem(transactionStoreSubsystemKey, t, t)
	return t
}

func newTransactionStore() *TransactionStore {
	return &TransactionStore{
		primary: make(map[[20]byte]uint32, 8192),
		byExp: btree.NewBTreeGOptions[txExpKey](txExpLess, btree.Options{
			Degree: 64, NoLocks: true,
		}),
		undoLog: make([]txUndo, 0, 64),
	}
}

func (s *TransactionStore) SnapshotPoint() int { return len(s.undoLog) }

func (s *TransactionStore) RestoreToPoint(snap int) {
	if snap < 0 {
		snap = 0
	}
	if snap > len(s.undoLog) {
		return
	}
	for i := len(s.undoLog) - 1; i >= snap; i-- {
		e := s.undoLog[i]
		switch e.op {
		case 0: // undo a Put
			if cur, ok := s.primary[e.trxID]; ok {
				s.byExp.Delete(txExpKey{exp: cur, trxID: e.trxID})
				delete(s.primary, e.trxID)
			}
			if e.prevHad {
				// A Put overwrote an existing row — restore it.
				s.primary[e.trxID] = e.exp
				s.byExp.Set(txExpKey{exp: e.exp, trxID: e.trxID})
			}
		case 1: // undo a Delete
			if e.prevHad {
				s.primary[e.trxID] = e.exp
				s.byExp.Set(txExpKey{exp: e.exp, trxID: e.trxID})
			}
		}
	}
	s.undoLog = s.undoLog[:snap]
}

func (s *TransactionStore) CommitPoint() { s.undoLog = s.undoLog[:0] }

// Has reports whether trxID is in the dedup window.
func (s *TransactionStore) Has(trxID [20]byte) bool {
	_, ok := s.primary[trxID]
	return ok
}

// Put inserts or replaces the (trxID, expiration) row. Maintains the
// by-expiration index. Skips undo when db.UndoEnabled is false.
func (s *TransactionStore) Put(trxID [20]byte, expiration uint32) {
	prev, had := s.primary[trxID]
	if s.db.UndoEnabled {
		s.undoLog = append(s.undoLog, txUndo{
			trxID: trxID, exp: prev, prevHad: had, op: 0,
		})
	}
	if had {
		s.byExp.DeleteHint(txExpKey{exp: prev, trxID: trxID}, &s.hintByExp)
	}
	s.primary[trxID] = expiration
	s.byExp.SetHint(txExpKey{exp: expiration, trxID: trxID}, &s.hintByExp)
}

// Delete removes the trxID row (if present). Captures undo state when
// db.UndoEnabled is true.
func (s *TransactionStore) Delete(trxID [20]byte) {
	prev, had := s.primary[trxID]
	if !had {
		return
	}
	if s.db.UndoEnabled {
		s.undoLog = append(s.undoLog, txUndo{
			trxID: trxID, exp: prev, prevHad: true, op: 1,
		})
	}
	delete(s.primary, trxID)
	s.byExp.DeleteHint(txExpKey{exp: prev, trxID: trxID}, &s.hintByExp)
}

// PruneExpired removes every entry whose expiration ≤ now. Mirrors C++
// database::clear_expired_transactions. Always fires from the per-block
// hook path (UndoEnabled=false in the apply_block flow), so the undo
// gating is consistent with that callsite.
func (s *TransactionStore) PruneExpired(now uint32) {
	s.pruneBuf = s.pruneBuf[:0]
	if s.pruneCb.cb == nil {
		pc := &s.pruneCb
		pc.cb = func(k txExpKey) bool {
			if k.exp > pc.now {
				return false
			}
			*pc.exp = append(*pc.exp, k)
			return true
		}
	}
	s.pruneCb.exp = &s.pruneBuf
	s.pruneCb.now = now
	s.byExp.Scan(s.pruneCb.cb)
	for _, k := range s.pruneBuf {
		if s.db.UndoEnabled {
			s.undoLog = append(s.undoLog, txUndo{
				trxID: k.trxID, exp: k.exp, prevHad: true, op: 1,
			})
		}
		delete(s.primary, k.trxID)
		s.byExp.DeleteHint(k, &s.hintByExp)
	}
}

// Count returns the number of entries in the dedup window.
func (s *TransactionStore) Count() int { return len(s.primary) }

// transactionStoreSnapshotMagic identifies a transaction_store snapshot stream.
var transactionStoreSnapshotMagic = [4]byte{'T', 'R', 'X', 'W'}

const transactionStoreSnapshotVersion uint32 = 1

// MarshalSnapshot serializes the primary map. Entries emitted in trxID
// ascending order for determinism. Format:
//
//	magic | version | count | (trxID[20] | expiration_LE_4) × count
func (s *TransactionStore) MarshalSnapshot(w io.Writer) error {
	if _, err := w.Write(transactionStoreSnapshotMagic[:]); err != nil {
		return err
	}
	var hdr [8]byte
	binary.LittleEndian.PutUint32(hdr[0:4], transactionStoreSnapshotVersion)
	binary.LittleEndian.PutUint32(hdr[4:8], uint32(len(s.primary)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	// Sort by trxID for deterministic output.
	keys := make([][20]byte, 0, len(s.primary))
	for k := range s.primary {
		keys = append(keys, k)
	}
	// Insertion sort would be fine for sortedness — use a simple slice sort.
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && lessTrxID(keys[j], keys[j-1]); j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	var buf [24]byte
	for _, k := range keys {
		copy(buf[:20], k[:])
		binary.LittleEndian.PutUint32(buf[20:24], s.primary[k])
		if _, err := w.Write(buf[:]); err != nil {
			return err
		}
	}
	return nil
}

func lessTrxID(a, b [20]byte) bool {
	for i := 0; i < 20; i++ {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}

// UnmarshalSnapshot replaces the primary map from r.
func (s *TransactionStore) UnmarshalSnapshot(r io.Reader) error {
	br := asByteReader(r)
	var magic [4]byte
	if _, err := io.ReadFull(br, magic[:]); err != nil {
		return fmt.Errorf("transaction_store: read magic: %w", err)
	}
	if magic != transactionStoreSnapshotMagic {
		return fmt.Errorf("transaction_store: bad magic %q", magic)
	}
	var hdr [8]byte
	if _, err := io.ReadFull(br, hdr[:]); err != nil {
		return fmt.Errorf("transaction_store: read header: %w", err)
	}
	ver := binary.LittleEndian.Uint32(hdr[0:4])
	if ver != transactionStoreSnapshotVersion {
		return fmt.Errorf("transaction_store: unsupported version %d", ver)
	}
	count := binary.LittleEndian.Uint32(hdr[4:8])
	s.primary = make(map[[20]byte]uint32, count)
	s.byExp.Clear()
	s.undoLog = s.undoLog[:0]
	var buf [24]byte
	for i := uint32(0); i < count; i++ {
		if _, err := io.ReadFull(br, buf[:]); err != nil {
			return fmt.Errorf("transaction_store: read entry[%d]: %w", i, err)
		}
		var trxID [20]byte
		copy(trxID[:], buf[:20])
		exp := binary.LittleEndian.Uint32(buf[20:24])
		s.primary[trxID] = exp
		s.byExp.Set(txExpKey{exp: exp, trxID: trxID})
	}
	return nil
}
