package objects

import (
	"encoding/binary"
	"fmt"
	"io"

	"gopherd/internal/state"
)

// BlockSummaryStore is the in-memory typed container for the
// block-summary ring buffer. Replaces the K/V row at
// (BlockSummaryTable, block_num&0xffff). Mirrors C++ chainbase's
// block_summary_object which is also a flat ring.
//
// Why typed-store: every block apply writes one PutBlockSummary row
// (~4M Session.Put calls over 4M blocks), and every transaction
// validate Gets one for TaPOS. The K/V overlay path turned each call
// into a string key + marshal/unmarshal hop. A flat [65536][20]byte
// array gives O(1) access with zero per-call allocation.
type BlockSummaryStore struct {
	db   *state.Database
	ring [65536][20]byte
	// set[i] reports whether ring[i] has been populated. Mirrors the
	// "ErrNotFound" early-chain behavior of the K/V path (before the
	// ring filled on block ~65K).
	set [65536]bool

	undoLog []blockSummaryUndo
}

type blockSummaryUndo struct {
	idx     uint16
	prev    [20]byte
	prevSet bool
}

const blockSummaryStoreSubsystemKey = "block_summary_store"

// EnsureBlockSummaryStore returns the database's BlockSummaryStore.
func EnsureBlockSummaryStore(s *state.Session) *BlockSummaryStore {
	db := s.Database()
	if x := db.HotSubsystem(hotSlotBlockSummary); x != nil {
		return x.(*BlockSummaryStore)
	}
	if existing := db.GetSubsystem(blockSummaryStoreSubsystemKey); existing != nil {
		if t, ok := existing.(*BlockSummaryStore); ok {
			db.SetHotSubsystem(hotSlotBlockSummary, t)
			return t
		}
	}
	t := &BlockSummaryStore{
		db:      db,
		undoLog: make([]blockSummaryUndo, 0, 8),
	}
	db.RegisterSubsystem(blockSummaryStoreSubsystemKey, t, t)
	db.SetHotSubsystem(hotSlotBlockSummary, t)
	return t
}

func (s *BlockSummaryStore) SnapshotPoint() int { return len(s.undoLog) }

func (s *BlockSummaryStore) RestoreToPoint(snap int) {
	if snap < 0 {
		snap = 0
	}
	if snap > len(s.undoLog) {
		return
	}
	for i := len(s.undoLog) - 1; i >= snap; i-- {
		e := s.undoLog[i]
		if e.prevSet {
			s.ring[e.idx] = e.prev
			s.set[e.idx] = true
		} else {
			s.ring[e.idx] = [20]byte{}
			s.set[e.idx] = false
		}
	}
	s.undoLog = s.undoLog[:snap]
}

func (s *BlockSummaryStore) CommitPoint() { s.undoLog = s.undoLog[:0] }

// Put stores blockID into the ring slot for blockNum.
func (s *BlockSummaryStore) Put(blockNum uint32, blockID [20]byte) {
	idx := uint16(blockNum & 0xffff)
	if s.db.UndoEnabled {
		s.undoLog = append(s.undoLog, blockSummaryUndo{
			idx: idx, prev: s.ring[idx], prevSet: s.set[idx],
		})
	}
	s.ring[idx] = blockID
	s.set[idx] = true
}

// Get returns (blockID, true) when the slot is populated.
func (s *BlockSummaryStore) Get(blockNum uint32) ([20]byte, bool) {
	idx := uint16(blockNum & 0xffff)
	return s.ring[idx], s.set[idx]
}

// blockSummaryStoreSnapshotMagic identifies a block_summary_store snapshot.
var blockSummaryStoreSnapshotMagic = [4]byte{'B', 'S', 'U', 'M'}

const blockSummaryStoreSnapshotVersion uint32 = 1

// MarshalSnapshot writes the populated slots in ascending index order:
// magic | version | count | (idx_LE_2 | blockID[20]) × count
func (s *BlockSummaryStore) MarshalSnapshot(w io.Writer) error {
	if _, err := w.Write(blockSummaryStoreSnapshotMagic[:]); err != nil {
		return err
	}
	count := 0
	for i := 0; i < len(s.set); i++ {
		if s.set[i] {
			count++
		}
	}
	var hdr [8]byte
	binary.LittleEndian.PutUint32(hdr[0:4], blockSummaryStoreSnapshotVersion)
	binary.LittleEndian.PutUint32(hdr[4:8], uint32(count))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	var rec [22]byte
	for i := 0; i < len(s.set); i++ {
		if !s.set[i] {
			continue
		}
		binary.LittleEndian.PutUint16(rec[0:2], uint16(i))
		copy(rec[2:22], s.ring[i][:])
		if _, err := w.Write(rec[:]); err != nil {
			return err
		}
	}
	return nil
}

// UnmarshalSnapshot replaces the ring from r.
func (s *BlockSummaryStore) UnmarshalSnapshot(r io.Reader) error {
	br := asByteReader(r)
	var magic [4]byte
	if _, err := io.ReadFull(br, magic[:]); err != nil {
		return fmt.Errorf("block_summary_store: read magic: %w", err)
	}
	if magic != blockSummaryStoreSnapshotMagic {
		return fmt.Errorf("block_summary_store: bad magic %q", magic)
	}
	var hdr [8]byte
	if _, err := io.ReadFull(br, hdr[:]); err != nil {
		return fmt.Errorf("block_summary_store: read header: %w", err)
	}
	ver := binary.LittleEndian.Uint32(hdr[0:4])
	if ver != blockSummaryStoreSnapshotVersion {
		return fmt.Errorf("block_summary_store: unsupported version %d", ver)
	}
	count := binary.LittleEndian.Uint32(hdr[4:8])
	for i := range s.ring {
		s.ring[i] = [20]byte{}
		s.set[i] = false
	}
	s.undoLog = s.undoLog[:0]
	var rec [22]byte
	for k := uint32(0); k < count; k++ {
		if _, err := io.ReadFull(br, rec[:]); err != nil {
			return fmt.Errorf("block_summary_store: read entry[%d]: %w", k, err)
		}
		idx := binary.LittleEndian.Uint16(rec[0:2])
		copy(s.ring[idx][:], rec[2:22])
		s.set[idx] = true
	}
	return nil
}
