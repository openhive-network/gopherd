package objects

import (
	"encoding/binary"
	"fmt"
	"io"

	"gopherd/internal/state"
)

// FutureWitnessScheduleStore mirrors WitnessScheduleStore but holds the
// HF26+ future_witness_schedule_object — the schedule that will be
// promoted to current at the next HIVE_MAX_WITNESSES boundary.
//
// Created in C++ at the HF26 activation hook (database_hardfork.cpp:492)
// as a copy of the current witness_schedule_object; thereafter each
// round-boundary update_witness_schedule swaps future→current and
// recomputes future via update_witness_schedule4.
//
// Used by:
//   - update_witness_schedule round-boundary swap (witness_schedule.cpp:420)
//   - get_witness_schedule_object_for_irreversibility (database.cpp:2874)
//     — at HF26+, LIB derives from the future schedule (which witnesses
//     will sign the NEXT round, not the current one).
type FutureWitnessScheduleStore struct {
	db    *state.Database
	val   WitnessSchedule
	valid bool

	undoLog []witnessScheduleUndo
}

const futureWitnessScheduleStoreSubsystemKey = "future_witness_schedule_store"

// EnsureFutureWitnessScheduleStore returns the FutureWitnessScheduleStore.
func EnsureFutureWitnessScheduleStore(s *state.Session) *FutureWitnessScheduleStore {
	db := s.Database()
	if x := db.HotSubsystem(hotSlotFutureWitnessSchedule); x != nil {
		return x.(*FutureWitnessScheduleStore)
	}
	if existing := db.GetSubsystem(futureWitnessScheduleStoreSubsystemKey); existing != nil {
		if t, ok := existing.(*FutureWitnessScheduleStore); ok {
			db.SetHotSubsystem(hotSlotFutureWitnessSchedule, t)
			return t
		}
	}
	t := &FutureWitnessScheduleStore{
		db:      db,
		undoLog: make([]witnessScheduleUndo, 0, 8),
	}
	db.RegisterSubsystem(futureWitnessScheduleStoreSubsystemKey, t, t)
	db.SetHotSubsystem(hotSlotFutureWitnessSchedule, t)
	return t
}

func (s *FutureWitnessScheduleStore) SnapshotPoint() int { return len(s.undoLog) }

func (s *FutureWitnessScheduleStore) RestoreToPoint(snap int) {
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

func (s *FutureWitnessScheduleStore) CommitPoint() { s.undoLog = s.undoLog[:0] }

// Put writes the singleton. The RecentBlockProducers slice is shared by
// reference — callers must not mutate it after Put.
func (s *FutureWitnessScheduleStore) Put(w *WitnessSchedule) {
	if s.db.UndoEnabled {
		s.undoLog = append(s.undoLog, witnessScheduleUndo{prev: s.val, prevValid: s.valid})
	}
	s.val = *w
	s.valid = true
}

// Get returns (value, true) when populated.
func (s *FutureWitnessScheduleStore) Get() (WitnessSchedule, bool) {
	return s.val, s.valid
}

// GetRef returns a pointer to the in-place value, or (nil, false) when
// not populated. Caller MUST NOT mutate through the pointer.
func (s *FutureWitnessScheduleStore) GetRef() (*WitnessSchedule, bool) {
	if !s.valid {
		return nil, false
	}
	return &s.val, true
}

// PutFutureWitnessSchedule writes via the typed store.
func PutFutureWitnessSchedule(s *state.Session, w *WitnessSchedule) {
	EnsureFutureWitnessScheduleStore(s).Put(w)
}

// GetFutureWitnessSchedule reads the singleton; returns ErrNotFound when
// HF26 has not yet activated (and therefore the future-schedule object
// has not been seeded).
func GetFutureWitnessSchedule(s *state.Session) (WitnessSchedule, error) {
	w, ok := EnsureFutureWitnessScheduleStore(s).Get()
	if !ok {
		return WitnessSchedule{}, state.ErrNotFound
	}
	return w, nil
}

// HasFutureWitnessSchedule reports whether the HF26 seed has occurred.
func HasFutureWitnessSchedule(s *state.Session) bool {
	_, ok := EnsureFutureWitnessScheduleStore(s).Get()
	return ok
}

var futureWitnessScheduleStoreSnapshotMagic = [4]byte{'F', 'W', 'S', 'C'}

const futureWitnessScheduleStoreSnapshotVersion uint32 = 1

// MarshalSnapshot writes the singleton:
//
//	magic | version | valid_u8 | varuint(rec) | AppendWitnessSchedule(rec)?
func (s *FutureWitnessScheduleStore) MarshalSnapshot(w io.Writer) error {
	if _, err := w.Write(futureWitnessScheduleStoreSnapshotMagic[:]); err != nil {
		return err
	}
	var hdr [5]byte
	binary.LittleEndian.PutUint32(hdr[0:4], futureWitnessScheduleStoreSnapshotVersion)
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
func (s *FutureWitnessScheduleStore) UnmarshalSnapshot(r io.Reader) error {
	br := asByteReader(r)
	var magic [4]byte
	if _, err := io.ReadFull(br, magic[:]); err != nil {
		return fmt.Errorf("future_witness_schedule_store: read magic: %w", err)
	}
	if magic != futureWitnessScheduleStoreSnapshotMagic {
		return fmt.Errorf("future_witness_schedule_store: bad magic %q", magic)
	}
	var hdr [5]byte
	if _, err := io.ReadFull(br, hdr[:]); err != nil {
		return fmt.Errorf("future_witness_schedule_store: read header: %w", err)
	}
	ver := binary.LittleEndian.Uint32(hdr[0:4])
	if ver != futureWitnessScheduleStoreSnapshotVersion {
		return fmt.Errorf("future_witness_schedule_store: unsupported version %d", ver)
	}
	s.val = WitnessSchedule{}
	s.valid = hdr[4] != 0
	s.undoLog = s.undoLog[:0]
	if !s.valid {
		return nil
	}
	recLen, err := binary.ReadUvarint(br)
	if err != nil {
		return fmt.Errorf("future_witness_schedule_store: read rec len: %w", err)
	}
	rec := make([]byte, recLen)
	if _, err := io.ReadFull(br, rec); err != nil {
		return fmt.Errorf("future_witness_schedule_store: read rec: %w", err)
	}
	wv, err := ReadWitnessSchedule(rec)
	if err != nil {
		return fmt.Errorf("future_witness_schedule_store: decode rec: %w", err)
	}
	s.val = wv
	return nil
}
