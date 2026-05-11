package objects

import (
	"encoding/binary"
	"fmt"
	"io"

	"hived-go/internal/state"
)

// DGPOStore is the in-memory typed container for the singleton
// dynamic_global_property_object. Replaces the K/V row at
// (DynamicGlobalPropertyTable, [0]). Mirrors C++ chainbase's
// dynamic_global_property_object which is also a singleton row.
//
// Why typed-store: every block does 5+ PutDGPO calls (tx-end, schedule
// update, fund processing, virtual supply, …). The K/V path serialized
// the ~500-byte struct to overlay on every call — accounting for 1.72 GB
// of the residual Session.Put alloc pressure on a 4M-block run.
type DGPOStore struct {
	db    *state.Database
	val   DynamicGlobalProperty
	valid bool

	undoLog []dgpoUndo
}

type dgpoUndo struct {
	prev      DynamicGlobalProperty
	prevValid bool
}

const dgpoStoreSubsystemKey = "dgpo_store"

// EnsureDGPOStore returns the database's DGPOStore.
func EnsureDGPOStore(s *state.Session) *DGPOStore {
	db := s.Database()
	if existing := db.GetSubsystem(dgpoStoreSubsystemKey); existing != nil {
		if t, ok := existing.(*DGPOStore); ok {
			return t
		}
	}
	t := &DGPOStore{
		db:      db,
		undoLog: make([]dgpoUndo, 0, 8),
	}
	db.RegisterSubsystem(dgpoStoreSubsystemKey, t, t)
	return t
}

func (s *DGPOStore) SnapshotPoint() int { return len(s.undoLog) }

func (s *DGPOStore) RestoreToPoint(snap int) {
	if snap < 0 {
		snap = 0
	}
	if snap > len(s.undoLog) {
		return
	}
	// We only need to apply the OLDEST entry from [snap, len) — earlier
	// undo entries already captured the value at that point. But since
	// each entry independently records prev, replaying from the back
	// applies the latest captured "prev" — which is exactly the state at
	// `snap`. Stepping linearly preserves the invariant.
	if snap < len(s.undoLog) {
		// The undo entry at index `snap` recorded the value BEFORE the
		// mutation at position `snap`. Apply that.
		e := s.undoLog[snap]
		s.val = e.prev
		s.valid = e.prevValid
	}
	s.undoLog = s.undoLog[:snap]
}

func (s *DGPOStore) CommitPoint() { s.undoLog = s.undoLog[:0] }

// Put writes the singleton. Caller's value is copied. Skips the
// pre-mutation snap when db.UndoEnabled is false — per-block hooks
// call PutDGPO 5+ times (process_funds, virtual_supply, fund cashout,
// witness schedule update, etc.) and capturing the prior DGPO each
// time was the largest non-witness alloc source on the per-block path.
func (s *DGPOStore) Put(p *DynamicGlobalProperty) {
	if s.db.UndoEnabled {
		s.undoLog = append(s.undoLog, dgpoUndo{prev: s.val, prevValid: s.valid})
	}
	s.val = *p
	s.valid = true
}

// Get returns (value, true) when the singleton has been populated.
func (s *DGPOStore) Get() (DynamicGlobalProperty, bool) {
	return s.val, s.valid
}

// dgpoStoreSnapshotMagic identifies a dgpo_store snapshot stream.
var dgpoStoreSnapshotMagic = [4]byte{'D', 'G', 'P', 'O'}

const dgpoStoreSnapshotVersion uint32 = 1

// MarshalSnapshot writes the singleton.
//
//	magic | version | valid_u8 | (AppendDGPO if valid)
func (s *DGPOStore) MarshalSnapshot(w io.Writer) error {
	if _, err := w.Write(dgpoStoreSnapshotMagic[:]); err != nil {
		return err
	}
	var hdr [5]byte
	binary.LittleEndian.PutUint32(hdr[0:4], dgpoStoreSnapshotVersion)
	if s.valid {
		hdr[4] = 1
	}
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	if !s.valid {
		return nil
	}
	var (
		varBuf [binary.MaxVarintLen64]byte
		recBuf []byte
	)
	recBuf = AppendDGPO(recBuf[:0], &s.val)
	n := binary.PutUvarint(varBuf[:], uint64(len(recBuf)))
	if _, err := w.Write(varBuf[:n]); err != nil {
		return err
	}
	if _, err := w.Write(recBuf); err != nil {
		return err
	}
	return nil
}

// UnmarshalSnapshot replaces the singleton from r.
func (s *DGPOStore) UnmarshalSnapshot(r io.Reader) error {
	br := asByteReader(r)
	var magic [4]byte
	if _, err := io.ReadFull(br, magic[:]); err != nil {
		return fmt.Errorf("dgpo_store: read magic: %w", err)
	}
	if magic != dgpoStoreSnapshotMagic {
		return fmt.Errorf("dgpo_store: bad magic %q", magic)
	}
	var hdr [5]byte
	if _, err := io.ReadFull(br, hdr[:]); err != nil {
		return fmt.Errorf("dgpo_store: read header: %w", err)
	}
	ver := binary.LittleEndian.Uint32(hdr[0:4])
	if ver != dgpoStoreSnapshotVersion {
		return fmt.Errorf("dgpo_store: unsupported version %d", ver)
	}
	s.val = DynamicGlobalProperty{}
	s.valid = hdr[4] != 0
	s.undoLog = s.undoLog[:0]
	if !s.valid {
		return nil
	}
	recLen, err := binary.ReadUvarint(br)
	if err != nil {
		return fmt.Errorf("dgpo_store: read rec len: %w", err)
	}
	rec := make([]byte, recLen)
	if _, err := io.ReadFull(br, rec); err != nil {
		return fmt.Errorf("dgpo_store: read rec: %w", err)
	}
	p, err := ReadDGPO(rec)
	if err != nil {
		return fmt.Errorf("dgpo_store: decode rec: %w", err)
	}
	s.val = p
	return nil
}
