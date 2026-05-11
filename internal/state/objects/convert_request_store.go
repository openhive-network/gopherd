package objects

import (
	"encoding/binary"
	"fmt"
	"io"

	"github.com/tidwall/btree"

	"hived-go/internal/protocol/auth"
	"hived-go/internal/state"
)

// ConvertRequestStore is the in-memory typed container for pending
// HBD→HIVE convert_request_object entries. Replaces the K/V path
// (ConvertRequestTable / ConvertRequestByDateIndex) used by
// processConversions on every block.
//
// Why typed-store: every block calls processConversions which Ranges
// the by-date index. With the K/V path that's one Session.Range setup
// per block + per-call encoded-key + closure boxing (~250 bytes/block,
// ~1 GB cum on a 4M-block run). A typed store with an in-memory
// btree-by-time gives O(log N) "earliest due" plus alloc-free
// iteration via direct Iter().
//
// Mirrors the snapshot/restore semantics of TransactionStore.
type ConvertRequestStore struct {
	db      *state.Database
	primary map[convertRequestPK]*ConvertRequest
	byDate  *btree.BTreeG[convertRequestSecKey]
	undoLog []convertRequestUndo
}

type convertRequestPK struct {
	owner     auth.AccountName
	requestID uint32
}

type convertRequestSecKey struct {
	conversion uint32
	owner      auth.AccountName
	requestID  uint32
}

func convertRequestSecLess(a, b convertRequestSecKey) bool {
	if a.conversion != b.conversion {
		return a.conversion < b.conversion
	}
	if a.owner != b.owner {
		return a.owner < b.owner
	}
	return a.requestID < b.requestID
}

type convertRequestUndo struct {
	pk      convertRequestPK
	prev    ConvertRequest
	prevHad bool
	op      uint8 // 0=Put, 1=Delete
}

const convertRequestStoreSubsystemKey = "convert_request_store"

func EnsureConvertRequestStore(s *state.Session) *ConvertRequestStore {
	db := s.Database()
	if existing := db.GetSubsystem(convertRequestStoreSubsystemKey); existing != nil {
		if t, ok := existing.(*ConvertRequestStore); ok {
			return t
		}
	}
	t := &ConvertRequestStore{
		db:      db,
		primary: make(map[convertRequestPK]*ConvertRequest, 64),
		byDate: btree.NewBTreeGOptions[convertRequestSecKey](convertRequestSecLess, btree.Options{
			Degree: 32, NoLocks: true,
		}),
		undoLog: make([]convertRequestUndo, 0, 16),
	}
	db.RegisterSubsystem(convertRequestStoreSubsystemKey, t, t)
	return t
}

func (s *ConvertRequestStore) SnapshotPoint() int { return len(s.undoLog) }

func (s *ConvertRequestStore) RestoreToPoint(snap int) {
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
			if cur, ok := s.primary[e.pk]; ok {
				s.byDate.Delete(convertRequestSecKey{
					conversion: cur.Conversion, owner: cur.Owner, requestID: cur.RequestID,
				})
				delete(s.primary, e.pk)
			}
			if e.prevHad {
				cp := new(ConvertRequest)
				*cp = e.prev
				s.primary[e.pk] = cp
				s.byDate.Set(convertRequestSecKey{
					conversion: cp.Conversion, owner: cp.Owner, requestID: cp.RequestID,
				})
			}
		case 1: // undo a Delete
			if e.prevHad {
				cp := new(ConvertRequest)
				*cp = e.prev
				s.primary[e.pk] = cp
				s.byDate.Set(convertRequestSecKey{
					conversion: cp.Conversion, owner: cp.Owner, requestID: cp.RequestID,
				})
			}
		}
	}
	s.undoLog = s.undoLog[:snap]
}

func (s *ConvertRequestStore) CommitPoint() { s.undoLog = s.undoLog[:0] }

func (s *ConvertRequestStore) Put(c *ConvertRequest) {
	pk := convertRequestPK{owner: c.Owner, requestID: c.RequestID}
	prev, had := s.primary[pk]
	var prevVal ConvertRequest
	if had {
		prevVal = *prev
		s.byDate.Delete(convertRequestSecKey{
			conversion: prev.Conversion, owner: prev.Owner, requestID: prev.RequestID,
		})
	}
	if s.db.UndoEnabled {
		s.undoLog = append(s.undoLog, convertRequestUndo{
			pk: pk, prev: prevVal, prevHad: had, op: 0,
		})
	}
	if had {
		*prev = *c
	} else {
		cp := new(ConvertRequest)
		*cp = *c
		s.primary[pk] = cp
	}
	s.byDate.Set(convertRequestSecKey{
		conversion: c.Conversion, owner: c.Owner, requestID: c.RequestID,
	})
}

func (s *ConvertRequestStore) Get(owner auth.AccountName, requestID uint32) (ConvertRequest, bool) {
	pk := convertRequestPK{owner: owner, requestID: requestID}
	c, ok := s.primary[pk]
	if !ok {
		return ConvertRequest{}, false
	}
	return *c, true
}

func (s *ConvertRequestStore) Has(owner auth.AccountName, requestID uint32) bool {
	_, ok := s.primary[convertRequestPK{owner: owner, requestID: requestID}]
	return ok
}

func (s *ConvertRequestStore) Delete(owner auth.AccountName, requestID uint32) {
	pk := convertRequestPK{owner: owner, requestID: requestID}
	prev, had := s.primary[pk]
	if !had {
		return
	}
	if s.db.UndoEnabled {
		s.undoLog = append(s.undoLog, convertRequestUndo{
			pk: pk, prev: *prev, prevHad: true, op: 1,
		})
	}
	s.byDate.Delete(convertRequestSecKey{
		conversion: prev.Conversion, owner: prev.Owner, requestID: prev.RequestID,
	})
	delete(s.primary, pk)
}

// EarliestDue returns the earliest pending conversion time, or
// (0, false) when no entries exist. O(log N) via btree.Min.
func (s *ConvertRequestStore) EarliestDue() (uint32, bool) {
	if s.byDate.Len() == 0 {
		return 0, false
	}
	min, ok := s.byDate.Min()
	if !ok {
		return 0, false
	}
	return min.conversion, true
}

// RangeDueBefore iterates every entry whose conversion time is ≤ now,
// in ascending (conversion, owner, requestID) order. Returns at the
// first entry with conversion > now (matching the legacy K/V Range
// stop semantics). Alloc-free per call.
func (s *ConvertRequestStore) RangeDueBefore(now uint32, fn func(c ConvertRequest) bool) {
	it := s.byDate.Iter()
	for ok := it.First(); ok; ok = it.Next() {
		k := it.Item()
		if k.conversion > now {
			break
		}
		c, has := s.primary[convertRequestPK{owner: k.owner, requestID: k.requestID}]
		if !has {
			continue
		}
		if !fn(*c) {
			break
		}
	}
	it.Release()
}

// Count returns the number of pending convert requests.
func (s *ConvertRequestStore) Count() int { return len(s.primary) }

// MarshalSnapshot / UnmarshalSnapshot

var convertRequestStoreSnapshotMagic = [4]byte{'C', 'V', 'R', 'Q'}

const convertRequestStoreSnapshotVersion uint32 = 1

func (s *ConvertRequestStore) MarshalSnapshot(w io.Writer) error {
	if _, err := w.Write(convertRequestStoreSnapshotMagic[:]); err != nil {
		return err
	}
	var hdr [8]byte
	binary.LittleEndian.PutUint32(hdr[0:4], convertRequestStoreSnapshotVersion)
	binary.LittleEndian.PutUint32(hdr[4:8], uint32(len(s.primary)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	// Emit in by-date order for determinism.
	var emitErr error
	s.byDate.Scan(func(k convertRequestSecKey) bool {
		c, ok := s.primary[convertRequestPK{owner: k.owner, requestID: k.requestID}]
		if !ok {
			return true
		}
		buf := AppendConvertRequest(nil, c)
		var lenBuf [binary.MaxVarintLen64]byte
		n := binary.PutUvarint(lenBuf[:], uint64(len(buf)))
		if _, err := w.Write(lenBuf[:n]); err != nil {
			emitErr = err
			return false
		}
		if _, err := w.Write(buf); err != nil {
			emitErr = err
			return false
		}
		return true
	})
	return emitErr
}

func (s *ConvertRequestStore) UnmarshalSnapshot(r io.Reader) error {
	br := asByteReader(r)
	var magic [4]byte
	if _, err := io.ReadFull(br, magic[:]); err != nil {
		return fmt.Errorf("convert_request_store: read magic: %w", err)
	}
	if magic != convertRequestStoreSnapshotMagic {
		return fmt.Errorf("convert_request_store: bad magic %q", magic)
	}
	var hdr [8]byte
	if _, err := io.ReadFull(br, hdr[:]); err != nil {
		return fmt.Errorf("convert_request_store: read header: %w", err)
	}
	ver := binary.LittleEndian.Uint32(hdr[0:4])
	if ver != convertRequestStoreSnapshotVersion {
		return fmt.Errorf("convert_request_store: unsupported version %d", ver)
	}
	count := binary.LittleEndian.Uint32(hdr[4:8])
	s.primary = make(map[convertRequestPK]*ConvertRequest, count)
	s.byDate.Clear()
	s.undoLog = s.undoLog[:0]
	for i := uint32(0); i < count; i++ {
		recLen, err := binary.ReadUvarint(br)
		if err != nil {
			return fmt.Errorf("convert_request_store: read len[%d]: %w", i, err)
		}
		rec := make([]byte, recLen)
		if _, err := io.ReadFull(br, rec); err != nil {
			return fmt.Errorf("convert_request_store: read rec[%d]: %w", i, err)
		}
		c, err := ReadConvertRequest(rec)
		if err != nil {
			return fmt.Errorf("convert_request_store: decode[%d]: %w", i, err)
		}
		cp := new(ConvertRequest)
		*cp = c
		s.primary[convertRequestPK{owner: c.Owner, requestID: c.RequestID}] = cp
		s.byDate.Set(convertRequestSecKey{
			conversion: c.Conversion, owner: c.Owner, requestID: c.RequestID,
		})
	}
	return nil
}
