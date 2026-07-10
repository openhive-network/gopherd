package objects

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"

	"github.com/tidwall/btree"

	"hived-go/internal/protocol/auth"
	"hived-go/internal/state"
)

// LimitOrderStore is the in-memory typed container for the
// limit_order_object. Replaces the K/V path with three in-memory
// indexes:
//
//   - primary[owner, orderID] → *LimitOrder       — direct lookup
//   - byPrice[(base, quote)] → btree<*priceNode>  — matching engine
//   - byExp                  → btree<*expNode>    — expirer
//
// Why typed-store: at the HF6 cascade (block 2.89M) and again at
// HF17+ when the matching engine becomes hot, applyOrder iterates the
// by_price index for every taker. The K/V path allocated a fresh
// iterator + per-candidate Range frame on every call (~2.27 GB cum /
// 4M-block run). Typed-store gives O(log N) ordered access with zero
// per-call alloc and preserves pointer identity across overwrites so
// price/expiration index entries never need to be re-inserted on
// partial fills.
//
// LimitOrder is small (~80 bytes) and its sell_price + expiration
// fields are IMMUTABLE after creation — only ForSaleAmount changes.
// Overwrites therefore mutate the underlying struct in place and the
// secondary indexes remain valid; they only get touched on
// insert and delete.
type LimitOrderStore struct {
	db      *state.Database
	primary map[orderPK]*LimitOrder
	byPrice map[pricePair]*btree.BTreeG[*priceNode]
	byExp   *btree.BTreeG[*expNode]

	undoLog []orderUndo
	freeLO  []*LimitOrder
	freePN  []*priceNode
	freeEN  []*expNode
}

// orderPK identifies a LimitOrder by (owner, orderID) — Go hashes
// struct keys natively, eliminating string-concat allocs on lookup.
type orderPK struct {
	owner   string
	orderID uint32
}

func newOrderPK(owner auth.AccountName, orderID uint32) orderPK {
	return orderPK{owner: string(owner), orderID: orderID}
}

// pricePair groups orders by the symbols they trade between.
type pricePair struct {
	base, quote uint32
}

// priceNode is one entry in the by-price btree. priceInv is the
// bit-flipped 16-byte uint128 encoding of sell_price so byte-lex
// ascending order matches numeric DESCENDING price. id is the
// auto-incrementing limit_order_object::id, used as ASC tie-break.
type priceNode struct {
	priceInv [16]byte
	id       uint64
	lo       *LimitOrder
}

func priceNodeLess(a, b *priceNode) bool {
	cmp := bytes.Compare(a.priceInv[:], b.priceInv[:])
	if cmp != 0 {
		return cmp < 0
	}
	return a.id < b.id
}

// expNode is one entry in the by-expiration btree. Ordered by
// expiration ASC, then (owner, orderID) ASC for stable tie-break.
type expNode struct {
	exp     uint32
	pk      orderPK
	lo      *LimitOrder
}

func expNodeLess(a, b *expNode) bool {
	if a.exp != b.exp {
		return a.exp < b.exp
	}
	if a.pk.owner != b.pk.owner {
		return a.pk.owner < b.pk.owner
	}
	return a.pk.orderID < b.pk.orderID
}

type orderUndo struct {
	pk   orderPK
	op   uint8 // 0=insert, 1=mutate, 2=delete
	snap LimitOrder
	lo   *LimitOrder
	pn   *priceNode // captured on insert (so we can drop) and delete (so we can restore)
	en   *expNode
}

const limitOrderStoreSubsystemKey = "limit_order_store"

// EnsureLimitOrderStore returns the database's LimitOrderStore.
func EnsureLimitOrderStore(s *state.Session) *LimitOrderStore {
	db := s.Database()
	if x := db.HotSubsystem(hotSlotLimitOrder); x != nil {
		return x.(*LimitOrderStore)
	}
	if existing := db.GetSubsystem(limitOrderStoreSubsystemKey); existing != nil {
		if t, ok := existing.(*LimitOrderStore); ok {
			db.SetHotSubsystem(hotSlotLimitOrder, t)
			return t
		}
	}
	t := newLimitOrderStore()
	t.db = db
	db.RegisterSubsystem(limitOrderStoreSubsystemKey, t, t)
	db.SetHotSubsystem(hotSlotLimitOrder, t)
	return t
}

func newLimitOrderStore() *LimitOrderStore {
	return &LimitOrderStore{
		primary: make(map[orderPK]*LimitOrder, 256),
		byPrice: make(map[pricePair]*btree.BTreeG[*priceNode], 4),
		byExp: btree.NewBTreeGOptions[*expNode](expNodeLess, btree.Options{
			Degree: 64, NoLocks: true,
		}),
		undoLog: make([]orderUndo, 0, 64),
		freeLO:  make([]*LimitOrder, 0, 64),
		freePN:  make([]*priceNode, 0, 64),
		freeEN:  make([]*expNode, 0, 64),
	}
}

func (s *LimitOrderStore) SnapshotPoint() int { return len(s.undoLog) }

func (s *LimitOrderStore) RestoreToPoint(snap int) {
	if snap < 0 {
		snap = 0
	}
	if snap > len(s.undoLog) {
		return
	}
	for i := len(s.undoLog) - 1; i >= snap; i-- {
		e := s.undoLog[i]
		switch e.op {
		case 0: // undo insert
			delete(s.primary, e.pk)
			if e.pn != nil {
				if tree := s.byPrice[pricePair{base: e.lo.ForSaleSymbol, quote: e.lo.MinReceiveSym}]; tree != nil {
					tree.Delete(e.pn)
				}
				s.freePN = append(s.freePN, e.pn)
			}
			if e.en != nil {
				s.byExp.Delete(e.en)
				s.freeEN = append(s.freeEN, e.en)
			}
			s.freeLO = append(s.freeLO, e.lo)
		case 1: // undo mutate
			*e.lo = e.snap
		case 2: // undo delete
			*e.lo = e.snap
			s.primary[e.pk] = e.lo
			s.byExp.Set(e.en)
			pp := pricePair{base: e.lo.ForSaleSymbol, quote: e.lo.MinReceiveSym}
			tree, ok := s.byPrice[pp]
			if !ok {
				tree = btree.NewBTreeGOptions[*priceNode](priceNodeLess, btree.Options{
					Degree: 64, NoLocks: true,
				})
				s.byPrice[pp] = tree
			}
			tree.Set(e.pn)
		}
	}
	s.undoLog = s.undoLog[:snap]
}

func (s *LimitOrderStore) CommitPoint() {
	// Slots that became unreachable on Commit get recycled. For op==0
	// (insert) the slot lives in primary → no recycle. For op==1
	// (mutate) the snap is value — nothing to recycle. For op==2
	// (delete) the lo/pn/en became unreachable.
	for i := range s.undoLog {
		e := &s.undoLog[i]
		if e.op == 2 {
			s.freeLO = append(s.freeLO, e.lo)
			if e.pn != nil {
				s.freePN = append(s.freePN, e.pn)
			}
			if e.en != nil {
				s.freeEN = append(s.freeEN, e.en)
			}
		}
		e.lo = nil
		e.pn = nil
		e.en = nil
	}
	s.undoLog = s.undoLog[:0]
	if cap(s.freeLO) > 4096 && len(s.freeLO) > 4096 {
		s.freeLO = s.freeLO[:4096]
	}
}

// allocLO pulls a *LimitOrder slot from the freelist or allocates fresh.
func (s *LimitOrderStore) allocLO() *LimitOrder {
	if n := len(s.freeLO); n > 0 {
		lo := s.freeLO[n-1]
		s.freeLO = s.freeLO[:n-1]
		return lo
	}
	return new(LimitOrder)
}

func (s *LimitOrderStore) allocPN() *priceNode {
	if n := len(s.freePN); n > 0 {
		pn := s.freePN[n-1]
		s.freePN = s.freePN[:n-1]
		return pn
	}
	return new(priceNode)
}

func (s *LimitOrderStore) allocEN() *expNode {
	if n := len(s.freeEN); n > 0 {
		en := s.freeEN[n-1]
		s.freeEN = s.freeEN[:n-1]
		return en
	}
	return new(expNode)
}

// Put inserts a new order or overwrites an existing one. For
// overwrites (partial fill) it mutates the underlying *LimitOrder in
// place — preserving pointer identity so secondary index entries stay
// valid. Price and expiration are IMMUTABLE so the indexes never need
// to update on overwrite.
func (s *LimitOrderStore) Put(lo *LimitOrder) {
	pk := newOrderPK(lo.Owner, lo.OrderID)
	prev := s.primary[pk]
	if prev != nil {
		// Overwrite — mutate in place. Capture snapshot for undo when needed.
		if s.db.UndoEnabled {
			s.undoLog = append(s.undoLog, orderUndo{
				pk: pk, op: 1, snap: *prev, lo: prev,
			})
		}
		*prev = *lo
		return
	}
	// Insert path.
	cp := s.allocLO()
	*cp = *lo
	s.primary[pk] = cp

	// Add by-price entry.
	pn := s.allocPN()
	encodePriceDescendingInto(pn.priceInv[:], cp.SellPriceBaseAmount, cp.MinReceiveAmt)
	pn.id = cp.ID
	pn.lo = cp
	pp := pricePair{base: cp.ForSaleSymbol, quote: cp.MinReceiveSym}
	tree, ok := s.byPrice[pp]
	if !ok {
		tree = btree.NewBTreeGOptions[*priceNode](priceNodeLess, btree.Options{
			Degree: 64, NoLocks: true,
		})
		s.byPrice[pp] = tree
	}
	tree.Set(pn)

	// Add by-expiration entry.
	en := s.allocEN()
	en.exp = cp.Expiration
	en.pk = pk
	en.lo = cp
	s.byExp.Set(en)

	if s.db.UndoEnabled {
		s.undoLog = append(s.undoLog, orderUndo{
			pk: pk, op: 0, lo: cp, pn: pn, en: en,
		})
	}
}

// Get returns a value-copy of the order.
func (s *LimitOrderStore) Get(owner auth.AccountName, orderID uint32) (LimitOrder, bool) {
	lo, ok := s.primary[newOrderPK(owner, orderID)]
	if !ok {
		return LimitOrder{}, false
	}
	return *lo, true
}

// RangeOrders calls fn for every open order (unordered). Return false to stop.
// Read-only; used by the API's get_order_book / get_open_orders.
func (s *LimitOrderStore) RangeOrders(fn func(*LimitOrder) bool) {
	for _, lo := range s.primary {
		if !fn(lo) {
			return
		}
	}
}

// Has reports whether the order exists.
func (s *LimitOrderStore) Has(owner auth.AccountName, orderID uint32) bool {
	_, ok := s.primary[newOrderPK(owner, orderID)]
	return ok
}

// Delete removes the order and all its index entries.
func (s *LimitOrderStore) Delete(owner auth.AccountName, orderID uint32) {
	pk := newOrderPK(owner, orderID)
	lo, ok := s.primary[pk]
	if !ok {
		return
	}
	pp := pricePair{base: lo.ForSaleSymbol, quote: lo.MinReceiveSym}
	tree := s.byPrice[pp]
	var pn *priceNode
	if tree != nil {
		// Find the priceNode via a seek with priceInv+id.
		var key priceNode
		encodePriceDescendingInto(key.priceInv[:], lo.SellPriceBaseAmount, lo.MinReceiveAmt)
		key.id = lo.ID
		pn, _ = tree.Get(&key)
		if pn != nil {
			tree.Delete(pn)
			if tree.Len() == 0 {
				delete(s.byPrice, pp)
			}
		}
	}
	var en *expNode
	en, _ = s.byExp.Get(&expNode{exp: lo.Expiration, pk: pk})
	if en != nil {
		s.byExp.Delete(en)
	}
	delete(s.primary, pk)
	if s.db.UndoEnabled {
		s.undoLog = append(s.undoLog, orderUndo{
			pk: pk, op: 2, snap: *lo, lo: lo, pn: pn, en: en,
		})
	} else {
		// Slot becomes unreachable; freelist immediately.
		s.freeLO = append(s.freeLO, lo)
		if pn != nil {
			s.freePN = append(s.freePN, pn)
		}
		if en != nil {
			s.freeEN = append(s.freeEN, en)
		}
	}
}

// RangeByPrice iterates orders for the given (base, quote) symbol pair
// in DESCENDING price (best first), then ASC by id. Returning false
// stops iteration.
func (s *LimitOrderStore) RangeByPrice(base, quote uint32, fn func(lo *LimitOrder) (bool, error)) error {
	tree := s.byPrice[pricePair{base: base, quote: quote}]
	if tree == nil {
		return nil
	}
	var rerr error
	tree.Scan(func(pn *priceNode) bool {
		ok, err := fn(pn.lo)
		if err != nil {
			rerr = err
			return false
		}
		return ok
	})
	return rerr
}

// RangeByOwner iterates every order whose owner matches `owner`. Order
// is not specified (primary map iteration). Used by HF23 clear_account
// to cancel every open order for the cleared account.
func (s *LimitOrderStore) RangeByOwner(owner auth.AccountName, fn func(lo LimitOrder) bool) {
	o := string(owner)
	for pk, lo := range s.primary {
		if pk.owner != o {
			continue
		}
		if !fn(*lo) {
			return
		}
	}
}

// RangeByExpiration iterates orders by expiration ASC. Returning false
// stops iteration (the C++ clear_expired loop stops at the first row
// with expiration > now).
func (s *LimitOrderStore) RangeByExpiration(fn func(lo *LimitOrder) (bool, error)) error {
	var rerr error
	s.byExp.Scan(func(en *expNode) bool {
		ok, err := fn(en.lo)
		if err != nil {
			rerr = err
			return false
		}
		return ok
	})
	return rerr
}

// Count returns the number of live orders.
func (s *LimitOrderStore) Count() int { return len(s.primary) }

// limitOrderStoreSnapshotMagic identifies a limit_order_store snapshot.
var limitOrderStoreSnapshotMagic = [4]byte{'L', 'M', 'O', 'R'}

const limitOrderStoreSnapshotVersion uint32 = 1

// MarshalSnapshot writes the primary set. Format:
//
//	magic | version | count | (varuint(rec) | AppendLimitOrder(rec))*
//
// Orders emitted in (owner, orderID) ASC for determinism. Index trees
// are rebuilt at unmarshal time from the record stream.
func (s *LimitOrderStore) MarshalSnapshot(w io.Writer) error {
	if _, err := w.Write(limitOrderStoreSnapshotMagic[:]); err != nil {
		return err
	}
	var hdr [8]byte
	binary.LittleEndian.PutUint32(hdr[0:4], limitOrderStoreSnapshotVersion)
	binary.LittleEndian.PutUint32(hdr[4:8], uint32(len(s.primary)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	keys := make([]orderPK, 0, len(s.primary))
	for k := range s.primary {
		keys = append(keys, k)
	}
	// Sort by (owner, orderID).
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0; j-- {
			a, b := keys[j-1], keys[j]
			less := a.owner < b.owner || (a.owner == b.owner && a.orderID < b.orderID)
			if less {
				break
			}
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	var (
		varBuf [binary.MaxVarintLen64]byte
		recBuf []byte
	)
	for _, k := range keys {
		lo := s.primary[k]
		recBuf = AppendLimitOrder(recBuf[:0], lo)
		n := binary.PutUvarint(varBuf[:], uint64(len(recBuf)))
		if _, err := w.Write(varBuf[:n]); err != nil {
			return err
		}
		if _, err := w.Write(recBuf); err != nil {
			return err
		}
	}
	return nil
}

// UnmarshalSnapshot rebuilds primary + both secondary indexes.
func (s *LimitOrderStore) UnmarshalSnapshot(r io.Reader) error {
	br := asByteReader(r)
	var magic [4]byte
	if _, err := io.ReadFull(br, magic[:]); err != nil {
		return fmt.Errorf("limit_order_store: read magic: %w", err)
	}
	if magic != limitOrderStoreSnapshotMagic {
		return fmt.Errorf("limit_order_store: bad magic %q", magic)
	}
	var hdr [8]byte
	if _, err := io.ReadFull(br, hdr[:]); err != nil {
		return fmt.Errorf("limit_order_store: read header: %w", err)
	}
	ver := binary.LittleEndian.Uint32(hdr[0:4])
	if ver != limitOrderStoreSnapshotVersion {
		return fmt.Errorf("limit_order_store: unsupported version %d", ver)
	}
	count := binary.LittleEndian.Uint32(hdr[4:8])
	s.primary = make(map[orderPK]*LimitOrder, count)
	s.byPrice = make(map[pricePair]*btree.BTreeG[*priceNode], 4)
	s.byExp.Clear()
	s.undoLog = s.undoLog[:0]
	for i := uint32(0); i < count; i++ {
		recLen, err := binary.ReadUvarint(br)
		if err != nil {
			return fmt.Errorf("limit_order_store: read rec len[%d]: %w", i, err)
		}
		rec := make([]byte, recLen)
		if _, err := io.ReadFull(br, rec); err != nil {
			return fmt.Errorf("limit_order_store: read rec[%d]: %w", i, err)
		}
		lo, err := ReadLimitOrder(rec)
		if err != nil {
			return fmt.Errorf("limit_order_store: decode rec[%d]: %w", i, err)
		}
		cp := lo
		pk := newOrderPK(cp.Owner, cp.OrderID)
		s.primary[pk] = &cp

		pn := new(priceNode)
		encodePriceDescendingInto(pn.priceInv[:], cp.SellPriceBaseAmount, cp.MinReceiveAmt)
		pn.id = cp.ID
		pn.lo = &cp
		pp := pricePair{base: cp.ForSaleSymbol, quote: cp.MinReceiveSym}
		tree, ok := s.byPrice[pp]
		if !ok {
			tree = btree.NewBTreeGOptions[*priceNode](priceNodeLess, btree.Options{
				Degree: 64, NoLocks: true,
			})
			s.byPrice[pp] = tree
		}
		tree.Set(pn)

		en := &expNode{exp: cp.Expiration, pk: pk, lo: &cp}
		s.byExp.Set(en)
	}
	return nil
}
