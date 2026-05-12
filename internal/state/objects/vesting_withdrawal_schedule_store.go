package objects

import (
	"encoding/binary"
	"fmt"
	"io"

	"github.com/tidwall/btree"

	"hived-go/internal/protocol/auth"
	"hived-go/internal/state"
)

// VestingWithdrawalScheduleStore is the in-memory typed container for
// the (next_withdrawal_time, account) schedule that drives
// processVestingWithdrawals. Replaces the K/V path
// (VestingWithdrawalScheduleTable) which fired one Session.Range per
// block — ~250 bytes/block in the legacy path.
//
// Schema: a btree of (time, account) sorted ascending by time then
// account. Existence is the entire payload (matches the K/V layout
// where the value was always empty bytes).
//
// EarliestDue() returns the earliest scheduled time in O(log N). When
// processVestingWithdrawals sees min > blockTime it can skip the walk
// entirely — the common case for most blocks.
type VestingWithdrawalScheduleStore struct {
	db      *state.Database
	byTime  *btree.BTreeG[vestingWithdrawalScheduleKey]
	undoLog []vestingWithdrawalScheduleUndo
}

type vestingWithdrawalScheduleKey struct {
	time    uint32
	account auth.AccountName
}

func vestingWithdrawalScheduleLess(a, b vestingWithdrawalScheduleKey) bool {
	if a.time != b.time {
		return a.time < b.time
	}
	return a.account < b.account
}

type vestingWithdrawalScheduleUndo struct {
	key  vestingWithdrawalScheduleKey
	had  bool  // pre-mutation presence
	op   uint8 // 0=Put, 1=Delete
}

const vestingWithdrawalScheduleStoreSubsystemKey = "vesting_withdrawal_schedule_store"

func EnsureVestingWithdrawalScheduleStore(s *state.Session) *VestingWithdrawalScheduleStore {
	db := s.Database()
	if x := db.HotSubsystem(hotSlotVestWithdrawal); x != nil {
		return x.(*VestingWithdrawalScheduleStore)
	}
	if existing := db.GetSubsystem(vestingWithdrawalScheduleStoreSubsystemKey); existing != nil {
		if t, ok := existing.(*VestingWithdrawalScheduleStore); ok {
			db.SetHotSubsystem(hotSlotVestWithdrawal, t)
			return t
		}
	}
	t := &VestingWithdrawalScheduleStore{
		db: db,
		byTime: btree.NewBTreeGOptions[vestingWithdrawalScheduleKey](
			vestingWithdrawalScheduleLess, btree.Options{Degree: 32, NoLocks: true},
		),
		undoLog: make([]vestingWithdrawalScheduleUndo, 0, 16),
	}
	db.RegisterSubsystem(vestingWithdrawalScheduleStoreSubsystemKey, t, t)
	db.SetHotSubsystem(hotSlotVestWithdrawal, t)
	return t
}

func (s *VestingWithdrawalScheduleStore) SnapshotPoint() int { return len(s.undoLog) }

func (s *VestingWithdrawalScheduleStore) RestoreToPoint(snap int) {
	if snap < 0 {
		snap = 0
	}
	if snap > len(s.undoLog) {
		return
	}
	for i := len(s.undoLog) - 1; i >= snap; i-- {
		e := s.undoLog[i]
		switch e.op {
		case 0:
			if !e.had {
				s.byTime.Delete(e.key)
			}
		case 1:
			if e.had {
				s.byTime.Set(e.key)
			}
		}
	}
	s.undoLog = s.undoLog[:snap]
}

func (s *VestingWithdrawalScheduleStore) CommitPoint() { s.undoLog = s.undoLog[:0] }

func (s *VestingWithdrawalScheduleStore) Put(time uint32, account auth.AccountName) {
	k := vestingWithdrawalScheduleKey{time: time, account: account}
	_, had := s.byTime.Get(k)
	if s.db.UndoEnabled {
		s.undoLog = append(s.undoLog, vestingWithdrawalScheduleUndo{key: k, had: had, op: 0})
	}
	s.byTime.Set(k)
}

func (s *VestingWithdrawalScheduleStore) Delete(time uint32, account auth.AccountName) {
	k := vestingWithdrawalScheduleKey{time: time, account: account}
	_, had := s.byTime.Get(k)
	if !had {
		return
	}
	if s.db.UndoEnabled {
		s.undoLog = append(s.undoLog, vestingWithdrawalScheduleUndo{key: k, had: true, op: 1})
	}
	s.byTime.Delete(k)
}

// EarliestDue returns the earliest scheduled time, or (0, false) when
// the queue is empty.
func (s *VestingWithdrawalScheduleStore) EarliestDue() (uint32, bool) {
	if s.byTime.Len() == 0 {
		return 0, false
	}
	min, ok := s.byTime.Min()
	if !ok {
		return 0, false
	}
	return min.time, true
}

// RangeDueBefore calls fn for every (time, account) with time ≤ now
// in ascending order. Returning false stops the walk.
func (s *VestingWithdrawalScheduleStore) RangeDueBefore(now uint32, fn func(time uint32, account auth.AccountName) bool) {
	it := s.byTime.Iter()
	for ok := it.First(); ok; ok = it.Next() {
		k := it.Item()
		if k.time > now {
			break
		}
		if !fn(k.time, k.account) {
			break
		}
	}
	it.Release()
}

func (s *VestingWithdrawalScheduleStore) Count() int { return s.byTime.Len() }

// MarshalSnapshot/UnmarshalSnapshot

var vestingWithdrawalScheduleStoreSnapshotMagic = [4]byte{'V', 'W', 'D', 'S'}

const vestingWithdrawalScheduleStoreSnapshotVersion uint32 = 1

func (s *VestingWithdrawalScheduleStore) MarshalSnapshot(w io.Writer) error {
	if _, err := w.Write(vestingWithdrawalScheduleStoreSnapshotMagic[:]); err != nil {
		return err
	}
	var hdr [8]byte
	binary.LittleEndian.PutUint32(hdr[0:4], vestingWithdrawalScheduleStoreSnapshotVersion)
	binary.LittleEndian.PutUint32(hdr[4:8], uint32(s.byTime.Len()))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	var emitErr error
	var varBuf [binary.MaxVarintLen64]byte
	var timeBuf [4]byte
	s.byTime.Scan(func(k vestingWithdrawalScheduleKey) bool {
		binary.LittleEndian.PutUint32(timeBuf[:], k.time)
		if _, err := w.Write(timeBuf[:]); err != nil {
			emitErr = err
			return false
		}
		n := binary.PutUvarint(varBuf[:], uint64(len(k.account)))
		if _, err := w.Write(varBuf[:n]); err != nil {
			emitErr = err
			return false
		}
		if _, err := io.WriteString(w, string(k.account)); err != nil {
			emitErr = err
			return false
		}
		return true
	})
	return emitErr
}

func (s *VestingWithdrawalScheduleStore) UnmarshalSnapshot(r io.Reader) error {
	br := asByteReader(r)
	var magic [4]byte
	if _, err := io.ReadFull(br, magic[:]); err != nil {
		return fmt.Errorf("vesting_withdrawal_schedule_store: read magic: %w", err)
	}
	if magic != vestingWithdrawalScheduleStoreSnapshotMagic {
		return fmt.Errorf("vesting_withdrawal_schedule_store: bad magic %q", magic)
	}
	var hdr [8]byte
	if _, err := io.ReadFull(br, hdr[:]); err != nil {
		return fmt.Errorf("vesting_withdrawal_schedule_store: read header: %w", err)
	}
	ver := binary.LittleEndian.Uint32(hdr[0:4])
	if ver != vestingWithdrawalScheduleStoreSnapshotVersion {
		return fmt.Errorf("vesting_withdrawal_schedule_store: unsupported version %d", ver)
	}
	count := binary.LittleEndian.Uint32(hdr[4:8])
	s.byTime.Clear()
	s.undoLog = s.undoLog[:0]
	var timeBuf [4]byte
	for i := uint32(0); i < count; i++ {
		if _, err := io.ReadFull(br, timeBuf[:]); err != nil {
			return fmt.Errorf("vesting_withdrawal_schedule_store: read time[%d]: %w", i, err)
		}
		t := binary.LittleEndian.Uint32(timeBuf[:])
		nameLen, err := binary.ReadUvarint(br)
		if err != nil {
			return fmt.Errorf("vesting_withdrawal_schedule_store: read account len[%d]: %w", i, err)
		}
		name := make([]byte, nameLen)
		if _, err := io.ReadFull(br, name); err != nil {
			return fmt.Errorf("vesting_withdrawal_schedule_store: read account[%d]: %w", i, err)
		}
		s.byTime.Set(vestingWithdrawalScheduleKey{time: t, account: auth.AccountName(name)})
	}
	return nil
}
