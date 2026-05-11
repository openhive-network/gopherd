package objects

import (
	"encoding/binary"
	"fmt"
	"io"

	"hived-go/internal/state"
)

// WitnessScheduleStore is the in-memory typed container for the
// singleton witness_schedule_object. Replaces the K/V row at
// (WitnessScheduleTable, [0]).
//
// Why typed-store: every block's per-witness-schedule update calls
// PutWitnessSchedule. The previous lazy K/V path serialized the schedule
// (~2 KB) at every commit and copied into the overlay — accounting for
// 1.84 GB of the residual Session.Put alloc on a 4M-block run. Routing
// through a typed singleton with Snapshot/Restore eliminates both the
// serialize step and the overlay copy.
type WitnessScheduleStore struct {
	db    *state.Database
	val   WitnessSchedule
	valid bool

	undoLog []witnessScheduleUndo
}

type witnessScheduleUndo struct {
	prev      WitnessSchedule
	prevValid bool
}

const witnessScheduleStoreSubsystemKey = "witness_schedule_store"

// EnsureWitnessScheduleStore returns the database's WitnessScheduleStore.
func EnsureWitnessScheduleStore(s *state.Session) *WitnessScheduleStore {
	db := s.Database()
	if existing := db.GetSubsystem(witnessScheduleStoreSubsystemKey); existing != nil {
		if t, ok := existing.(*WitnessScheduleStore); ok {
			return t
		}
	}
	t := &WitnessScheduleStore{
		db:      db,
		undoLog: make([]witnessScheduleUndo, 0, 8),
	}
	db.RegisterSubsystem(witnessScheduleStoreSubsystemKey, t, t)
	return t
}

func (s *WitnessScheduleStore) SnapshotPoint() int { return len(s.undoLog) }

func (s *WitnessScheduleStore) RestoreToPoint(snap int) {
	if snap < 0 {
		snap = 0
	}
	if snap > len(s.undoLog) {
		return
	}
	if snap < len(s.undoLog) {
		e := s.undoLog[snap]
		s.val = e.prev
		s.valid = e.prevValid
	}
	s.undoLog = s.undoLog[:snap]
}

func (s *WitnessScheduleStore) CommitPoint() { s.undoLog = s.undoLog[:0] }

// Put writes the singleton. Caller's value is deep-copied via
// value-assignment of the struct (note: the RecentBlockProducers slice
// is shared by reference — callers must not mutate it after Put). Skips
// the pre-mutation snap when db.UndoEnabled is false.
func (s *WitnessScheduleStore) Put(w *WitnessSchedule) {
	if s.db.UndoEnabled {
		s.undoLog = append(s.undoLog, witnessScheduleUndo{prev: s.val, prevValid: s.valid})
	}
	s.val = *w
	s.valid = true
}

// Get returns (value, true) when populated. Note: the returned
// WitnessSchedule's RecentBlockProducers slice aliases the store's
// internal slice — callers must treat it as read-only.
func (s *WitnessScheduleStore) Get() (WitnessSchedule, bool) {
	return s.val, s.valid
}

// witnessScheduleStoreSnapshotMagic identifies a witness_schedule_store snapshot.
var witnessScheduleStoreSnapshotMagic = [4]byte{'W', 'S', 'C', 'H'}

const witnessScheduleStoreSnapshotVersion uint32 = 1

// MarshalSnapshot writes the singleton.
//
//	magic | version | valid_u8 | varuint(rec)|AppendWitnessSchedule(rec)?
func (s *WitnessScheduleStore) MarshalSnapshot(w io.Writer) error {
	if _, err := w.Write(witnessScheduleStoreSnapshotMagic[:]); err != nil {
		return err
	}
	var hdr [5]byte
	binary.LittleEndian.PutUint32(hdr[0:4], witnessScheduleStoreSnapshotVersion)
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
	recBuf = AppendWitnessSchedule(recBuf[:0], &s.val)
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
func (s *WitnessScheduleStore) UnmarshalSnapshot(r io.Reader) error {
	br := asByteReader(r)
	var magic [4]byte
	if _, err := io.ReadFull(br, magic[:]); err != nil {
		return fmt.Errorf("witness_schedule_store: read magic: %w", err)
	}
	if magic != witnessScheduleStoreSnapshotMagic {
		return fmt.Errorf("witness_schedule_store: bad magic %q", magic)
	}
	var hdr [5]byte
	if _, err := io.ReadFull(br, hdr[:]); err != nil {
		return fmt.Errorf("witness_schedule_store: read header: %w", err)
	}
	ver := binary.LittleEndian.Uint32(hdr[0:4])
	if ver != witnessScheduleStoreSnapshotVersion {
		return fmt.Errorf("witness_schedule_store: unsupported version %d", ver)
	}
	s.val = WitnessSchedule{}
	s.valid = hdr[4] != 0
	s.undoLog = s.undoLog[:0]
	if !s.valid {
		return nil
	}
	recLen, err := binary.ReadUvarint(br)
	if err != nil {
		return fmt.Errorf("witness_schedule_store: read rec len: %w", err)
	}
	rec := make([]byte, recLen)
	if _, err := io.ReadFull(br, rec); err != nil {
		return fmt.Errorf("witness_schedule_store: read rec: %w", err)
	}
	wv, err := ReadWitnessSchedule(rec)
	if err != nil {
		return fmt.Errorf("witness_schedule_store: decode rec: %w", err)
	}
	s.val = wv
	return nil
}
