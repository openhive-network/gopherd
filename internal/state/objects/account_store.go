package objects

import (
	"container/heap"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"os"
	"runtime"
	"sort"
	"strings"

	"github.com/tidwall/btree"

	"gopherd/internal/protocol/auth"
	"gopherd/internal/state"
)

// envBalanceTrace is a comma-separated list of account names whose
// Balance / RewardHive changes get printed (with a 2-frame call-site
// hint) on every Put / Mutate. Cached at process start. Replays set
// ACCOUNT_TRACE=dantheman to chase the 2-sat HIVE drift.
var (
	envBalanceTrace     = os.Getenv("ACCOUNT_TRACE")
	envBalanceTraceList = func() []string {
		if envBalanceTrace == "" {
			return nil
		}
		out := strings.Split(envBalanceTrace, ",")
		for i := range out {
			out[i] = strings.TrimSpace(out[i])
		}
		return out
	}()
)

func balanceTraceWanted(name string) bool {
	for _, n := range envBalanceTraceList {
		if n == name {
			return true
		}
	}
	return false
}

// emitBalanceTrace prints when Balance, HBDBalance, RewardHive or
// RewardHBD changed on a traced account. Reports old → new and a
// shallow stack hint so we can pin the call site without instrumenting
// every Balance += / -= site individually.
func emitBalanceTrace(name string, oldA, newA *Account) {
	if oldA.Balance == newA.Balance &&
		oldA.HBDBalance == newA.HBDBalance &&
		oldA.RewardHive == newA.RewardHive &&
		oldA.RewardHBD == newA.RewardHBD {
		return
	}
	pcs := make([]uintptr, 8)
	n := runtime.Callers(3, pcs)
	frames := runtime.CallersFrames(pcs[:n])
	var sb strings.Builder
	for i := 0; i < 4; i++ {
		f, more := frames.Next()
		fmt.Fprintf(&sb, " %s:%d", f.Function, f.Line)
		if !more {
			break
		}
	}
	fmt.Fprintf(os.Stderr,
		"GHIVEDIFF ACCT-DIFF name=%s bal=%d->%d hbd=%d->%d rhive=%d->%d rhbd=%d->%d stack=%s\n",
		name,
		oldA.Balance, newA.Balance,
		oldA.HBDBalance, newA.HBDBalance,
		oldA.RewardHive, newA.RewardHive,
		oldA.RewardHBD, newA.RewardHBD,
		sb.String(),
	)
}

// AccountStore is the in-memory typed container for the Account table.
// It replaces the generic K/V state path for accounts: mutations
// happen in-place against the primary map, with no serialize → overlay
// copy → K/V commit chain. Mirrors the chainbase-style WitnessStore.
//
// Why typed-store: at 3.5M blocks the chain holds ~400K accounts, each
// touched on every vote / transfer / cashout / witness op. The previous
// K/V path serialized each Put (~150 bytes / account) and re-parsed each
// Get into a fresh Account struct. Profiling showed Account serde
// dominating the cashout path past block 3M; the typed-store eliminates
// that overhead and is ~3-5× faster across the board.
//
// Invariants
//
//   - primary[name] points to the canonical *Account for `name`. Get
//     returns a value-copy so callers never mutate the stored record
//     except via Put.
//   - undoLog captures the prior pointer for every Put. RestoreToPoint
//     replays in reverse to undo a tx's writes.
//
// Account has no secondary indexes (the by-name primary is the only
// one used in consensus-critical paths). Cross-account scans (e.g.
// removeExpiredGovernanceVotes) walk RangePrimary in name order.
type AccountStore struct {
	// db is consulted only for db.UndoEnabled in Put/Delete to decide
	// whether to capture an undo snapshot.
	db      *state.Database
	primary map[string]*Account
	undoLog []accountUndo

	// freeAccount is a freelist of *Account slots reclaimed at
	// CommitPoint / RestoreToPoint when the prev pointers in undo
	// entries become unreachable. Put pulls from here before
	// allocating a fresh struct, eliminating the per-Put heap
	// allocation that ranked third in the alloc profile.
	freeAccount []*Account

	// delayedVoteFront/delayedVoteByTime index accounts that have pending HF24+
	// delayed-vote buckets, so the per-block processDelayedVoting hook
	// iterates only THOSE accounts instead of sorting+scanning all ~1.4M
	// every block (which collapsed replay from 8000 to ~2 blk/s once HF24
	// activated). It is a conservative SUPERSET: every Put/Mutate that
	// leaves an account with non-empty DelayedVotes adds it (so no account
	// with pending votes is ever missed — accounts are fully resident, and
	// UnmarshalSnapshot reseeds from a full scan); entries are removed only
	// by processDelayedVoting once an account's queue is confirmed empty.
	// Stale entries (e.g. from a rolled-back tx) are harmless — the hook
	// rechecks the account's actual DelayedVotes and drops empties. Not
	// persisted in the snapshot (rebuilt on load); not consensus state.
	//
	// Since 2026-10-10 the set is ORDERED: delayedVoteByTime keys every
	// candidate by its front bucket's time (hived's by_delayed_voting index),
	// and delayedVoteFront remembers the key each name is filed under so a
	// change of front re-keys it. processDelayedVoting then visits only the
	// accounts whose front has matured instead of every candidate (~100k
	// accounts per block by 2021, 10% of the apply loop). The index is kept
	// exact through Put/Mutate/RestoreToPoint/UnmarshalSnapshot, so a stale
	// key can only be for an account that no longer exists.
	delayedVoteFront  map[string]uint32
	delayedVoteByTime *btree.BTreeG[delayedVoteKey]

	// govExpLB is a conservative LOWER BOUND on the earliest non-zero
	// GovernanceVoteExpiration across all accounts — the O(1)-skip gate for
	// removeExpiredGovernanceVotes (see noteDelayedVotes). 0 = unknown
	// (forces a scan: fresh store, snapshot load, post-rollback);
	// math.MaxUint32 = nothing pending. Not consensus state, not persisted.
	govExpLB uint32

	// govExpHeap + govExpKnown form a lazy-deletion min-heap over
	// (expiration, name) — the Go analogue of hived's
	// by_governance_vote_expiration_ts index. Once seeded (govExpSeeded)
	// the sweep pops due entries in O(log n) instead of full-scanning
	// ~2.4M accounts; that scan was fine while expiries were rare, but
	// past the HF25+365d onset (~65.7M) expiries fire every ~40 blocks
	// and the scans ate 77% of replay CPU (run79: 172 blocks/s).
	// Entries are validated against the account's CURRENT stamp on pop
	// (stale entries from restamps/rollbacks are discarded); govExpKnown
	// dedups pushes so the heap stays ~#stamped-accounts. Not consensus
	// state; reset to unseeded (one reseeding scan) on rollback.
	govExpHeap   govExpHeap
	govExpKnown  map[string]uint32
	govExpSeeded bool
}

// govExpEntry is one (expiration, name) pair in the lazy min-heap.
type govExpEntry struct {
	exp  uint32
	name string
}

type govExpHeap []govExpEntry

func (h govExpHeap) Len() int      { return len(h) }
func (h govExpHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h govExpHeap) Less(i, j int) bool {
	if h[i].exp != h[j].exp {
		return h[i].exp < h[j].exp
	}
	return h[i].name < h[j].name
}
func (h *govExpHeap) Push(x any) { *h = append(*h, x.(govExpEntry)) }
func (h *govExpHeap) Pop() any {
	old := *h
	n := len(old)
	e := old[n-1]
	*h = old[:n-1]
	return e
}

type accountUndo struct {
	name string
	// op encodes how Restore inverts the mutation:
	//   0 = insert       → Restore: delete primary[name], freelist slot
	//   1 = overwrite    → Restore: *slot = snap (slot stays in primary)
	//   2 = delete       → Restore: primary[name] = slot, *slot = snap
	op   uint8
	slot *Account
	snap Account
}

const accountStoreSubsystemKey = "account_store"

// EnsureAccountStore returns the database's AccountStore, lazily
// creating + registering it on first call. Domain code calls this
// instead of building stores by hand so the snapshot/commit hooks are
// always wired.
func EnsureAccountStore(s *state.Session) *AccountStore {
	db := s.Database()
	if x := db.HotSubsystem(hotSlotAccount); x != nil {
		return x.(*AccountStore)
	}
	if existing := db.GetSubsystem(accountStoreSubsystemKey); existing != nil {
		if as, ok := existing.(*AccountStore); ok {
			db.SetHotSubsystem(hotSlotAccount, as)
			return as
		}
	}
	as := newAccountStore()
	as.db = db
	db.RegisterSubsystem(accountStoreSubsystemKey, as, as)
	db.SetHotSubsystem(hotSlotAccount, as)
	return as
}

func newAccountStore() *AccountStore {
	return &AccountStore{
		primary:           make(map[string]*Account, 4096),
		undoLog:           make([]accountUndo, 0, 64),
		delayedVoteFront:  make(map[string]uint32),
		delayedVoteByTime: newDelayedVoteTree(),
	}
}

// noteDelayedVotes adds name to the delayed-vote index if the account has any
// pending buckets. Add-only (the index is a superset; processDelayedVoting
// prunes empties). Cheap: a slice-len check on every Put/Mutate, a map insert
// only for the rare account that actually has delayed votes.
func (s *AccountStore) noteDelayedVotes(name string, a *Account) {
	s.indexDelayedVotes(name, a)
	// Same trick for governance-vote expiration: keep govExpLB a lower bound
	// on the earliest non-zero expiration so removeExpiredGovernanceVotes can
	// O(1)-skip blocks where nothing is due (post-HF25 it otherwise sorted +
	// scanned ALL ~1.6M accounts EVERY block — ~5s/block at 55M, the gen60
	// frontier crawl). Within a block the bound only ever DECREASES here, and
	// tx-rollback restores pre-tx values that the block-start bound already
	// covered, so undo keeps the invariant (bound ≤ every non-zero exp).
	if e := a.GovernanceVoteExpiration; e != 0 && s.govExpLB != 0 && e < s.govExpLB {
		s.govExpLB = e
	}
	// Feed the lazy expiration heap: push only when the stamp changed
	// from what the heap last saw for this account (govExpKnown dedup).
	// Setting a stamp to 0 (expiry processed / HF23 clear) needs no push —
	// the stale heap entry is discarded on pop by current-value validation.
	if s.govExpSeeded {
		if e := a.GovernanceVoteExpiration; e != 0 && s.govExpKnown[name] != e {
			s.govExpKnown[name] = e
			heap.Push(&s.govExpHeap, govExpEntry{exp: e, name: name})
		}
	}
}

// GovExpSeedStart begins heap seeding (caller then feeds every stamped
// account via GovExpSeedAdd and finishes with GovExpSeedDone). Used by the
// sweep's one-time full scan after a fresh load or rollback reset.
func (s *AccountStore) GovExpSeedStart(capacityHint int) {
	s.govExpHeap = make(govExpHeap, 0, capacityHint)
	s.govExpKnown = make(map[string]uint32, capacityHint)
	s.govExpSeeded = false
}

// GovExpSeedAdd records one stamped account during seeding (no heap
// ordering yet — heap.Init happens in GovExpSeedDone).
func (s *AccountStore) GovExpSeedAdd(name string, exp uint32) {
	s.govExpHeap = append(s.govExpHeap, govExpEntry{exp: exp, name: name})
	s.govExpKnown[name] = exp
}

// GovExpSeedDone heapifies the seeded entries and enables heap mode.
func (s *AccountStore) GovExpSeedDone() {
	heap.Init(&s.govExpHeap)
	s.govExpSeeded = true
}

// GovExpHeapReady reports whether the lazy expiration heap is usable.
func (s *AccountStore) GovExpHeapReady() bool { return s.govExpSeeded }

// GovExpPopDue pops every heap entry with exp <= blockTime and returns the
// names whose CURRENT GovernanceVoteExpiration is genuinely due (non-zero
// and <= blockTime). Stale entries — the account was restamped later or the
// stamp was cleared — are discarded; a restamped account's newer entry is
// still in the heap (every stamp change pushes). May return duplicates in
// pathological restamp patterns; callers dedup via the due-processing
// itself (processing zeroes the stamp, so the second validation fails).
func (s *AccountStore) GovExpPopDue(blockTime uint32) []string {
	var due []string
	for len(s.govExpHeap) > 0 && s.govExpHeap[0].exp <= blockTime {
		e := heap.Pop(&s.govExpHeap).(govExpEntry)
		a, ok := s.primary[e.name]
		if !ok {
			delete(s.govExpKnown, e.name)
			continue
		}
		cur := a.GovernanceVoteExpiration
		if cur == 0 {
			delete(s.govExpKnown, e.name)
			continue
		}
		if cur != e.exp {
			// Restamped: the newer entry is elsewhere in the heap.
			continue
		}
		due = append(due, e.name)
	}
	return due
}

// GovExpHeapTop returns the earliest pending expiration per the heap
// (MaxUint32 when empty). Only meaningful when GovExpHeapReady.
func (s *AccountStore) GovExpHeapTop() uint32 {
	if len(s.govExpHeap) == 0 {
		return math.MaxUint32
	}
	return s.govExpHeap[0].exp
}

// GovExpNoneDue reports whether NO account's governance-vote expiration can
// have passed by blockTime, based on the govExpLB lower bound. Conservative:
// false only forces a scan, never skips a due account. 0 = unknown → scan.
func (s *AccountStore) GovExpNoneDue(blockTime uint32) bool {
	return s.govExpLB != 0 && s.govExpLB > blockTime
}

// SetGovExpBound installs the exact next-due expiration computed by the
// hook's full scan (math.MaxUint32 when no account has one pending).
func (s *AccountStore) SetGovExpBound(v uint32) { s.govExpLB = v }

// RangePrimaryUnordered visits every account WITHOUT the name sort — for
// scans whose processing order is established by the caller (the governance
// expiry hook collects due names and sorts just those). The name sort of the
// full map is ~5s at 1.6M accounts; unordered iteration is ~100ms.
func (s *AccountStore) RangePrimaryUnordered(fn func(a *Account) (bool, error)) error {
	for _, a := range s.primary {
		ok, err := fn(a)
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}
	}
	return nil
}

// delayedVoteKey orders delayed-vote candidates by front bucket time, then
// name (hived's by_delayed_voting index).
type delayedVoteKey struct {
	front uint32
	name  string
}

func delayedVoteKeyLess(a, b delayedVoteKey) bool {
	if a.front != b.front {
		return a.front < b.front
	}
	return a.name < b.name
}

func newDelayedVoteTree() *btree.BTreeG[delayedVoteKey] {
	return btree.NewBTreeGOptions[delayedVoteKey](delayedVoteKeyLess, btree.Options{NoLocks: true})
}

// indexDelayedVotes files name under its current front bucket time, re-keying
// if the front moved and removing it when the queue is empty. Called from
// every write path (Put, Mutate, undo, snapshot load) so the index is exact.
func (s *AccountStore) indexDelayedVotes(name string, a *Account) {
	if len(a.DelayedVotes) == 0 {
		s.dropDelayedVote(name)
		return
	}
	front := a.DelayedVotes[0].Time
	if old, ok := s.delayedVoteFront[name]; ok {
		if old == front {
			return
		}
		s.delayedVoteByTime.Delete(delayedVoteKey{front: old, name: name})
	}
	s.delayedVoteByTime.Set(delayedVoteKey{front: front, name: name})
	s.delayedVoteFront[name] = front
}

func (s *AccountStore) dropDelayedVote(name string) {
	if old, ok := s.delayedVoteFront[name]; ok {
		s.delayedVoteByTime.Delete(delayedVoteKey{front: old, name: name})
		delete(s.delayedVoteFront, name)
	}
}

// DelayedVoteNoneDue reports whether no candidate's front bucket has matured
// by blockTime (front + intervalSec <= blockTime), i.e. processDelayedVoting
// has nothing to do. O(1): the earliest front is the tree's first key.
func (s *AccountStore) DelayedVoteNoneDue(blockTime, intervalSec uint32) bool {
	first, ok := s.delayedVoteByTime.Min()
	if !ok {
		return true
	}
	return blockTime < intervalSec || first.front > blockTime-intervalSec
}

// RecomputeDelayedVoteMin is kept for callers written against the old
// lower-bound design; the ordered index needs no refresh.
func (s *AccountStore) RecomputeDelayedVoteMin() {}

// DelayedVoteCandidates returns every indexed account name in ascending order.
func (s *AccountStore) DelayedVoteCandidates() []string {
	out := make([]string, 0, len(s.delayedVoteFront))
	for n := range s.delayedVoteFront {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// DelayedVoteCandidatesDue returns, sorted by name, the candidates whose front
// bucket has matured by blockTime. It walks the time-ordered index from the
// front and stops at the first bucket that has not matured.
func (s *AccountStore) DelayedVoteCandidatesDue(blockTime, intervalSec uint32) []string {
	if blockTime < intervalSec {
		return nil
	}
	cutoff := blockTime - intervalSec
	var out []string
	s.delayedVoteByTime.Scan(func(k delayedVoteKey) bool {
		if k.front > cutoff {
			return false
		}
		out = append(out, k.name)
		return true
	})
	sort.Strings(out)
	return out
}

// DropDelayedVoteCandidate removes name from the index (called by
// processDelayedVoting once the account's queue is confirmed empty).
func (s *AccountStore) DropDelayedVoteCandidate(name string) {
	s.dropDelayedVote(name)
}

// SnapshotPoint returns the current undo-log length.
func (s *AccountStore) SnapshotPoint() int { return len(s.undoLog) }

// RestoreToPoint replays the undo log in reverse, restoring the store
// to the state captured at `snap`.
//
// Mutation/overwrite undo (hadPrev=true): copy snap back over
// *primary[name] — preserves pointer identity.
// Insert undo (hadPrev=false): delete the row and return its slot to
// the freelist.
func (s *AccountStore) RestoreToPoint(snap int) {
	if snap < 0 {
		snap = 0
	}
	if snap > len(s.undoLog) {
		return
	}
	if snap < len(s.undoLog) {
		// Rolled-back writes may have fed govExpLB values that no longer
		// exist; and a rollback ACROSS a hook's SetGovExpBound could leave
		// the bound above a still-live expiration. Reset to unknown — one
		// extra scan, never a missed expiration. The lazy heap is dropped
		// too: a rolled-back restamp leaves govExpKnown claiming a value
		// the account no longer has, which would dedup-away a future push
		// of that same value. Reseeding is one full scan — rollbacks are
		// rare (failed tx in sync mode), correctness wins.
		s.govExpLB = 0
		s.govExpSeeded = false
		s.govExpHeap = nil
		s.govExpKnown = nil
	}
	for i := len(s.undoLog) - 1; i >= snap; i-- {
		e := &s.undoLog[i]
		switch e.op {
		case 0: // undo insert
			if cur, ok := s.primary[e.name]; ok {
				s.freeAccount = append(s.freeAccount, cur)
				delete(s.primary, e.name)
			}
			s.dropDelayedVote(e.name)
		case 1: // undo overwrite
			if cur, ok := s.primary[e.name]; ok {
				*cur = e.snap
				s.indexDelayedVotes(e.name, cur)
			}
		case 2: // undo delete
			*e.slot = e.snap
			s.primary[e.name] = e.slot
			s.indexDelayedVotes(e.name, e.slot)
		}
		e.slot = nil
	}
	s.undoLog = s.undoLog[:snap]
}

// CommitPoint releases accumulated undo entries. For Delete undos
// (op=2) the slot becomes unreachable now that the deletion is
// permanent — recycle. For Insert/Overwrite undos the slot stays in
// primary; nothing to free.
func (s *AccountStore) CommitPoint() {
	for i := range s.undoLog {
		e := &s.undoLog[i]
		if e.op == 2 && e.slot != nil {
			s.freeAccount = append(s.freeAccount, e.slot)
		}
		e.slot = nil
	}
	s.undoLog = s.undoLog[:0]
	if cap(s.freeAccount) > 4096 && len(s.freeAccount) > 4096 {
		s.freeAccount = s.freeAccount[:4096]
	}
}

// Has reports whether `name` has an account row.
func (s *AccountStore) Has(name auth.AccountName) bool {
	_, ok := s.primary[string(name)]
	return ok
}

// Get returns a value-copy of the account for `name`. The boolean is
// false if the row doesn't exist; the returned Account in that case
// is the zero value.
func (s *AccountStore) Get(name auth.AccountName) (Account, bool) {
	a, ok := s.primary[string(name)]
	if !ok {
		return Account{}, false
	}
	return *a, true
}

// GetRef returns a read-only pointer to the slot for `name`. The
// returned pointer aliases internal storage — callers MUST treat it
// as read-only (use AccountStore.Mutate to modify) and MUST NOT
// retain it past the next mutation that might delete the row.
//
// Saves the ~hundred-byte Account struct value copy that the regular
// Get path pays per call. Use this on read-only paths where the
// caller only inspects fields.
func (s *AccountStore) GetRef(name auth.AccountName) (*Account, bool) {
	a, ok := s.primary[string(name)]
	return a, ok
}

// Put inserts or replaces the account keyed by a.Name. For an
// overwrite, mutates the existing slot in place — preserves pointer
// identity and avoids the per-Put alloc + double struct-copy of the
// prior pointer-replacing implementation. For an insert, pulls a slot
// from freeAccount or allocates fresh.
func (s *AccountStore) Put(a *Account) {
	name := string(a.Name)
	s.noteDelayedVotes(name, a) // index for processDelayedVoting (superset)
	if cur, ok := s.primary[name]; ok {
		if s.db.UndoEnabled {
			s.undoLog = append(s.undoLog, accountUndo{
				name: name, op: 1, slot: cur, snap: *cur,
			})
		}
		if envBalanceTrace != "" && balanceTraceWanted(name) {
			// Capture pre-write snapshot. When cur == a the in-place
			// mutation already happened, so we can't see the diff here;
			// callers using the Ref+mutate pattern are traced from
			// Mutate() instead.
			if cur != a {
				old := *cur
				*cur = *a
				emitBalanceTrace(name, &old, cur)
				return
			}
		}
		// Same-slot pointer (caller did GetAccountRef + mutate + Put)?
		// Skip the no-op self-copy — the slot is already at the
		// post-mutation state. Saves the ~700-byte struct copy per call
		// for sites that have adopted the Ref-and-mutate pattern.
		if cur != a {
			*cur = *a
		}
		return
	}
	var cp *Account
	if n := len(s.freeAccount); n > 0 {
		cp = s.freeAccount[n-1]
		s.freeAccount = s.freeAccount[:n-1]
	} else {
		cp = new(Account)
	}
	*cp = *a
	s.primary[name] = cp
	if s.db.UndoEnabled {
		s.undoLog = append(s.undoLog, accountUndo{name: name, op: 0, slot: cp})
	}
}

// Mutate fetches the live *Account for `name` and calls fn to apply
// in-place mutations. Captures a value-snapshot for undo when
// UndoEnabled; rolls back via *slot = snap.
//
// Returns false if the row doesn't exist. Saves the Account
// value-copy that the regular Get+modify+Put pattern pays — for
// the per-block hooks (UndoEnabled=false) the only cost is the
// map lookup + the caller's mutation, with no snap capture.
func (s *AccountStore) Mutate(name auth.AccountName, fn func(*Account)) bool {
	n := string(name)
	cur, ok := s.primary[n]
	if !ok {
		return false
	}
	if s.db.UndoEnabled {
		s.undoLog = append(s.undoLog, accountUndo{
			name: n, op: 1, slot: cur, snap: *cur,
		})
	}
	if envBalanceTrace != "" && balanceTraceWanted(n) {
		old := *cur
		fn(cur)
		emitBalanceTrace(n, &old, cur)
		s.noteDelayedVotes(n, cur)
		return true
	}
	fn(cur)
	s.noteDelayedVotes(n, cur)
	return true
}

// Delete removes the account row for `name`. Captures the slot+snap
// so Restore reinstates the row.
func (s *AccountStore) Delete(name auth.AccountName) {
	n := string(name)
	cur, ok := s.primary[n]
	if !ok {
		return
	}
	if s.db.UndoEnabled {
		s.undoLog = append(s.undoLog, accountUndo{
			name: n, op: 2, slot: cur, snap: *cur,
		})
	} else {
		// Caller invoked Delete outside any savepoint — slot is no
		// longer reachable, return it to the freelist immediately.
		s.freeAccount = append(s.freeAccount, cur)
	}
	delete(s.primary, n)
}

// RangePrimary calls fn for every account in ascending name order.
// Used by cross-account hooks (governance vote expiration sweeps,
// VoteRevoked scans). Returning false from fn stops iteration.
func (s *AccountStore) RangePrimary(fn func(a *Account) (bool, error)) error {
	names := make([]string, 0, len(s.primary))
	for n := range s.primary {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		ok, err := fn(s.primary[n])
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}
	}
	return nil
}

// Count returns the number of accounts currently in the store.
func (s *AccountStore) Count() int { return len(s.primary) }

// accountStoreSnapshotMagic identifies an account_store snapshot stream.
var accountStoreSnapshotMagic = [4]byte{'A', 'C', 'C', 'T'}

const accountStoreSnapshotVersion uint32 = 1

// MarshalSnapshot serializes the primary account map to w. Format mirrors
// witness_store: magic | version | count | (varuint(name)|name|varuint(rec)|AppendAccount(rec))*
// Names emitted in ascending order.
func (s *AccountStore) MarshalSnapshot(w io.Writer) error {
	if _, err := w.Write(accountStoreSnapshotMagic[:]); err != nil {
		return err
	}
	var hdr [8]byte
	binary.LittleEndian.PutUint32(hdr[0:4], accountStoreSnapshotVersion)
	binary.LittleEndian.PutUint32(hdr[4:8], uint32(len(s.primary)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	names := make([]string, 0, len(s.primary))
	for n := range s.primary {
		names = append(names, n)
	}
	sort.Strings(names)
	var (
		varBuf [binary.MaxVarintLen64]byte
		recBuf []byte
	)
	for _, n := range names {
		ap := s.primary[n]
		k := binary.PutUvarint(varBuf[:], uint64(len(n)))
		if _, err := w.Write(varBuf[:k]); err != nil {
			return err
		}
		if _, err := io.WriteString(w, n); err != nil {
			return err
		}
		recBuf = AppendAccount(recBuf[:0], ap)
		k = binary.PutUvarint(varBuf[:], uint64(len(recBuf)))
		if _, err := w.Write(varBuf[:k]); err != nil {
			return err
		}
		if _, err := w.Write(recBuf); err != nil {
			return err
		}
	}
	return nil
}

// UnmarshalSnapshot replaces the store's primary map from r. Undo log
// is cleared.
func (s *AccountStore) UnmarshalSnapshot(r io.Reader) error {
	br := asByteReader(r)
	var magic [4]byte
	if _, err := io.ReadFull(br, magic[:]); err != nil {
		return fmt.Errorf("account_store: read magic: %w", err)
	}
	if magic != accountStoreSnapshotMagic {
		return fmt.Errorf("account_store: bad magic %q", magic)
	}
	var hdr [8]byte
	if _, err := io.ReadFull(br, hdr[:]); err != nil {
		return fmt.Errorf("account_store: read header: %w", err)
	}
	ver := binary.LittleEndian.Uint32(hdr[0:4])
	if ver != accountStoreSnapshotVersion {
		return fmt.Errorf("account_store: unsupported version %d", ver)
	}
	count := binary.LittleEndian.Uint32(hdr[4:8])
	s.primary = make(map[string]*Account, count)
	s.undoLog = s.undoLog[:0]
	s.delayedVoteFront = make(map[string]uint32) // reseeded from the scan below
	s.delayedVoteByTime = newDelayedVoteTree()
	for i := uint32(0); i < count; i++ {
		nameLen, err := binary.ReadUvarint(br)
		if err != nil {
			return fmt.Errorf("account_store: read name len[%d]: %w", i, err)
		}
		nameBuf := make([]byte, nameLen)
		if _, err := io.ReadFull(br, nameBuf); err != nil {
			return fmt.Errorf("account_store: read name[%d]: %w", i, err)
		}
		recLen, err := binary.ReadUvarint(br)
		if err != nil {
			return fmt.Errorf("account_store: read rec len[%d]: %w", i, err)
		}
		rec := make([]byte, recLen)
		if _, err := io.ReadFull(br, rec); err != nil {
			return fmt.Errorf("account_store: read rec[%d]: %w", i, err)
		}
		acc, err := ReadAccount(rec)
		if err != nil {
			return fmt.Errorf("account_store: decode rec[%d]: %w", i, err)
		}
		cp := acc
		nm := string(nameBuf)
		s.primary[nm] = &cp
		s.indexDelayedVotes(nm, &cp) // reseed the delayed-vote index
	}
	return nil
}
