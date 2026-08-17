package objects

import (
	"encoding/binary"
	"fmt"
	"io"

	"github.com/tidwall/btree"

	"gopherd/internal/protocol/auth"
	"gopherd/internal/state"
)

// CommentCashoutScheduleStore is the in-memory typed container for the
// (cashout_time, comment_id) → (author, permlink) schedule used by
// processCommentCashouts. Replaces the K/V CommentCashoutScheduleTable.
//
// Iteration order mirrors C++ composite_key<cashout_time, comment_id>.
// EarliestDue() returns the earliest scheduled cashout time in
// O(log N); processCommentCashouts uses it to short-circuit when the
// queue is past the head_block_time.
type CommentCashoutScheduleStore struct {
	db *state.Database
	// primary is keyed by commentID alone (IDs are unique). Using uint64
	// keys takes the Go runtime's mapaccess1_fast64 / mapaccess2_fast64
	// path which hashes the integer directly without the generic
	// alg-table indirection the struct-keyed map paid (3.2s combined in
	// the 8M-block profile, ~half of CommentCashoutScheduleStore CPU).
	primary map[uint64]commentCashoutScheduleValueRec
	byTime  *btree.BTreeG[commentCashoutScheduleKey]
	undoLog []commentCashoutScheduleUndo
}

type commentCashoutScheduleKey struct {
	cashoutTime uint32
	commentID   uint64
}

type commentCashoutScheduleValueRec struct {
	author      auth.AccountName
	permlink    string
	cashoutTime uint32 // needed for byTime cleanup on Put-overwrite + Delete
}

func commentCashoutScheduleLess(a, b commentCashoutScheduleKey) bool {
	if a.cashoutTime != b.cashoutTime {
		return a.cashoutTime < b.cashoutTime
	}
	return a.commentID < b.commentID
}

type commentCashoutScheduleUndo struct {
	commentID uint64
	curKey    commentCashoutScheduleKey // for Put: the new key we set, for Delete: the key we removed
	prev      commentCashoutScheduleValueRec
	had       bool
	op        uint8 // 0=Put, 1=Delete
}

const commentCashoutScheduleStoreSubsystemKey = "comment_cashout_schedule_store"

func EnsureCommentCashoutScheduleStore(s *state.Session) *CommentCashoutScheduleStore {
	db := s.Database()
	if x := db.HotSubsystem(hotSlotCommentCashout); x != nil {
		return x.(*CommentCashoutScheduleStore)
	}
	if existing := db.GetSubsystem(commentCashoutScheduleStoreSubsystemKey); existing != nil {
		if t, ok := existing.(*CommentCashoutScheduleStore); ok {
			db.SetHotSubsystem(hotSlotCommentCashout, t)
			return t
		}
	}
	t := &CommentCashoutScheduleStore{
		db:      db,
		primary: make(map[uint64]commentCashoutScheduleValueRec, 16384),
		byTime: btree.NewBTreeGOptions[commentCashoutScheduleKey](commentCashoutScheduleLess, btree.Options{
			Degree: 64, NoLocks: true,
		}),
		undoLog: make([]commentCashoutScheduleUndo, 0, 32),
	}
	db.RegisterSubsystem(commentCashoutScheduleStoreSubsystemKey, t, t)
	db.SetHotSubsystem(hotSlotCommentCashout, t)
	return t
}

func (s *CommentCashoutScheduleStore) SnapshotPoint() int { return len(s.undoLog) }

func (s *CommentCashoutScheduleStore) RestoreToPoint(snap int) {
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
			// Undo a Put: drop the key we set, restore the prev value
			// (which may have a different cashoutTime).
			if cur, ok := s.primary[e.commentID]; ok {
				s.byTime.Delete(commentCashoutScheduleKey{cashoutTime: cur.cashoutTime, commentID: e.commentID})
				delete(s.primary, e.commentID)
			}
			if e.had {
				s.primary[e.commentID] = e.prev
				s.byTime.Set(commentCashoutScheduleKey{cashoutTime: e.prev.cashoutTime, commentID: e.commentID})
			}
		case 1:
			if e.had {
				s.primary[e.commentID] = e.prev
				s.byTime.Set(commentCashoutScheduleKey{cashoutTime: e.prev.cashoutTime, commentID: e.commentID})
			}
		}
	}
	s.undoLog = s.undoLog[:snap]
}

func (s *CommentCashoutScheduleStore) CommitPoint() { s.undoLog = s.undoLog[:0] }

func (s *CommentCashoutScheduleStore) Put(cashoutTime uint32, commentID uint64, author auth.AccountName, permlink string) {
	prev, had := s.primary[commentID]
	if s.db.UndoEnabled {
		s.undoLog = append(s.undoLog, commentCashoutScheduleUndo{
			commentID: commentID,
			prev:      prev, had: had, op: 0,
		})
	}
	if had && prev.cashoutTime != cashoutTime {
		// cashoutTime changed: pull the old by-time entry.
		s.byTime.Delete(commentCashoutScheduleKey{cashoutTime: prev.cashoutTime, commentID: commentID})
	}
	s.primary[commentID] = commentCashoutScheduleValueRec{author: author, permlink: permlink, cashoutTime: cashoutTime}
	if !had || prev.cashoutTime != cashoutTime {
		s.byTime.Set(commentCashoutScheduleKey{cashoutTime: cashoutTime, commentID: commentID})
	}
}

func (s *CommentCashoutScheduleStore) Delete(cashoutTime uint32, commentID uint64) {
	prev, had := s.primary[commentID]
	if !had {
		return
	}
	if s.db.UndoEnabled {
		s.undoLog = append(s.undoLog, commentCashoutScheduleUndo{
			commentID: commentID,
			prev:      prev, had: true, op: 1,
		})
	}
	delete(s.primary, commentID)
	s.byTime.Delete(commentCashoutScheduleKey{cashoutTime: prev.cashoutTime, commentID: commentID})
}

// EarliestDue returns the earliest scheduled cashout time in the queue,
// or (0, false) when no entries exist. O(log N) via btree.Min.
func (s *CommentCashoutScheduleStore) EarliestDue() (uint32, bool) {
	if s.byTime.Len() == 0 {
		return 0, false
	}
	min, ok := s.byTime.Min()
	if !ok {
		return 0, false
	}
	return min.cashoutTime, true
}

// RangeDueBefore iterates every (time, id, author, permlink) with
// time ≤ now in ascending (time, id) order. Returning false stops.
func (s *CommentCashoutScheduleStore) RangeDueBefore(now uint32, fn func(t uint32, id uint64, author auth.AccountName, permlink string) bool) {
	it := s.byTime.Iter()
	for ok := it.First(); ok; ok = it.Next() {
		k := it.Item()
		if k.cashoutTime > now {
			break
		}
		v, has := s.primary[k.commentID]
		if !has {
			continue
		}
		if !fn(k.cashoutTime, k.commentID, v.author, v.permlink) {
			break
		}
	}
	it.Release()
}

func (s *CommentCashoutScheduleStore) Count() int { return s.byTime.Len() }

// MarshalSnapshot / UnmarshalSnapshot

var commentCashoutScheduleStoreSnapshotMagic = [4]byte{'C', 'C', 'O', 'S'}

const commentCashoutScheduleStoreSnapshotVersion uint32 = 1

func (s *CommentCashoutScheduleStore) MarshalSnapshot(w io.Writer) error {
	if _, err := w.Write(commentCashoutScheduleStoreSnapshotMagic[:]); err != nil {
		return err
	}
	var hdr [8]byte
	binary.LittleEndian.PutUint32(hdr[0:4], commentCashoutScheduleStoreSnapshotVersion)
	binary.LittleEndian.PutUint32(hdr[4:8], uint32(len(s.primary)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	var emitErr error
	var fixedBuf [12]byte
	var varBuf [binary.MaxVarintLen64]byte
	s.byTime.Scan(func(k commentCashoutScheduleKey) bool {
		v, ok := s.primary[k.commentID]
		if !ok {
			return true
		}
		binary.LittleEndian.PutUint32(fixedBuf[0:4], k.cashoutTime)
		binary.LittleEndian.PutUint64(fixedBuf[4:12], k.commentID)
		if _, err := w.Write(fixedBuf[:]); err != nil {
			emitErr = err
			return false
		}
		n := binary.PutUvarint(varBuf[:], uint64(len(v.author)))
		if _, err := w.Write(varBuf[:n]); err != nil {
			emitErr = err
			return false
		}
		if _, err := io.WriteString(w, string(v.author)); err != nil {
			emitErr = err
			return false
		}
		n = binary.PutUvarint(varBuf[:], uint64(len(v.permlink)))
		if _, err := w.Write(varBuf[:n]); err != nil {
			emitErr = err
			return false
		}
		if _, err := io.WriteString(w, v.permlink); err != nil {
			emitErr = err
			return false
		}
		return true
	})
	return emitErr
}

func (s *CommentCashoutScheduleStore) UnmarshalSnapshot(r io.Reader) error {
	br := asByteReader(r)
	var magic [4]byte
	if _, err := io.ReadFull(br, magic[:]); err != nil {
		return fmt.Errorf("comment_cashout_schedule_store: read magic: %w", err)
	}
	if magic != commentCashoutScheduleStoreSnapshotMagic {
		return fmt.Errorf("comment_cashout_schedule_store: bad magic %q", magic)
	}
	var hdr [8]byte
	if _, err := io.ReadFull(br, hdr[:]); err != nil {
		return fmt.Errorf("comment_cashout_schedule_store: read header: %w", err)
	}
	ver := binary.LittleEndian.Uint32(hdr[0:4])
	if ver != commentCashoutScheduleStoreSnapshotVersion {
		return fmt.Errorf("comment_cashout_schedule_store: unsupported version %d", ver)
	}
	count := binary.LittleEndian.Uint32(hdr[4:8])
	s.primary = make(map[uint64]commentCashoutScheduleValueRec, count)
	s.byTime.Clear()
	s.undoLog = s.undoLog[:0]
	var fixedBuf [12]byte
	for i := uint32(0); i < count; i++ {
		if _, err := io.ReadFull(br, fixedBuf[:]); err != nil {
			return fmt.Errorf("comment_cashout_schedule_store: read fixed[%d]: %w", i, err)
		}
		k := commentCashoutScheduleKey{
			cashoutTime: binary.LittleEndian.Uint32(fixedBuf[0:4]),
			commentID:   binary.LittleEndian.Uint64(fixedBuf[4:12]),
		}
		authorLen, err := binary.ReadUvarint(br)
		if err != nil {
			return fmt.Errorf("comment_cashout_schedule_store: read author len[%d]: %w", i, err)
		}
		authorBuf := make([]byte, authorLen)
		if _, err := io.ReadFull(br, authorBuf); err != nil {
			return fmt.Errorf("comment_cashout_schedule_store: read author[%d]: %w", i, err)
		}
		permLen, err := binary.ReadUvarint(br)
		if err != nil {
			return fmt.Errorf("comment_cashout_schedule_store: read permlink len[%d]: %w", i, err)
		}
		permBuf := make([]byte, permLen)
		if _, err := io.ReadFull(br, permBuf); err != nil {
			return fmt.Errorf("comment_cashout_schedule_store: read permlink[%d]: %w", i, err)
		}
		v := commentCashoutScheduleValueRec{
			author:      auth.AccountName(authorBuf),
			permlink:    string(permBuf),
			cashoutTime: k.cashoutTime,
		}
		s.primary[k.commentID] = v
		s.byTime.Set(k)
	}
	return nil
}
