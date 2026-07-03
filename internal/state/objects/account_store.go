package objects

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"runtime"
	"sort"
	"strings"

	"hived-go/internal/protocol/auth"
	"hived-go/internal/state"
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

	// delayedVoteSet indexes accounts that (might) have pending HF24+
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
	delayedVoteSet map[string]struct{}

	// minFrontTime is a conservative LOWER BOUND on the oldest (front)
	// delayed-vote bucket time across all candidates. 0 = unknown (force a
	// scan). Since a bucket matures at front.Time + INTERVAL, if
	// minFrontTime + INTERVAL > blockTime then NOTHING is due and
	// processDelayedVoting can skip the O(candidates) scan entirely — the
	// common case, since maturations are spread over the 30-day window.
	// Kept a lower bound: adding a candidate lowers it toward the true min;
	// removals leave it loose (still ≤ true min = safe, just triggers an
	// occasional extra scan). RecomputeDelayedVoteMin refreshes it to the
	// exact min after each scan. Not consensus state; rebuilt on load.
	minFrontTime uint32
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
		primary:        make(map[string]*Account, 4096),
		undoLog:        make([]accountUndo, 0, 64),
		delayedVoteSet: make(map[string]struct{}),
	}
}

// noteDelayedVotes adds name to the delayed-vote index if the account has any
// pending buckets. Add-only (the index is a superset; processDelayedVoting
// prunes empties). Cheap: a slice-len check on every Put/Mutate, a map insert
// only for the rare account that actually has delayed votes.
func (s *AccountStore) noteDelayedVotes(name string, a *Account) {
	if len(a.DelayedVotes) > 0 {
		s.delayedVoteSet[name] = struct{}{}
		// Keep minFrontTime a lower bound on all candidates' front-bucket
		// times so processDelayedVoting can O(1)-skip when nothing is due.
		if front := a.DelayedVotes[0].Time; s.minFrontTime == 0 || front < s.minFrontTime {
			s.minFrontTime = front
		}
	}
}

// DelayedVoteNoneDue reports whether NO candidate's front bucket can have
// matured by blockTime (based on the minFrontTime lower bound), letting
// processDelayedVoting skip its scan. intervalSec = maturation interval.
// Conservative: a false negative (returns false when nothing is actually
// due) only causes an extra scan; it never skips a due account.
func (s *AccountStore) DelayedVoteNoneDue(blockTime, intervalSec uint32) bool {
	if len(s.delayedVoteSet) == 0 {
		return true
	}
	if s.minFrontTime == 0 {
		return false // unknown bound — must scan
	}
	// due iff front + interval <= blockTime  ⟺  front <= blockTime - interval.
	// Nothing due iff minFrontTime (lower bound) > blockTime - interval.
	return blockTime >= intervalSec && s.minFrontTime > blockTime-intervalSec
}

// RecomputeDelayedVoteMin refreshes minFrontTime to the exact minimum
// front-bucket time across candidates (dropping any whose queue emptied).
// Called after processDelayedVoting scans, so the next blocks can skip.
func (s *AccountStore) RecomputeDelayedVoteMin() {
	var min uint32
	for n := range s.delayedVoteSet {
		a := s.primary[n]
		if a == nil || len(a.DelayedVotes) == 0 {
			delete(s.delayedVoteSet, n)
			continue
		}
		if front := a.DelayedVotes[0].Time; min == 0 || front < min {
			min = front
		}
	}
	s.minFrontTime = min
}

// DelayedVoteCandidates returns the indexed account names (those that may have
// pending delayed votes) in ascending order — the small set processDelayedVoting
// iterates instead of all accounts. Sorted for deterministic processing order.
func (s *AccountStore) DelayedVoteCandidates() []string {
	out := make([]string, 0, len(s.delayedVoteSet))
	for n := range s.delayedVoteSet {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// DropDelayedVoteCandidate removes name from the index (called by
// processDelayedVoting once the account's queue is confirmed empty).
func (s *AccountStore) DropDelayedVoteCandidate(name string) {
	delete(s.delayedVoteSet, name)
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
	for i := len(s.undoLog) - 1; i >= snap; i-- {
		e := &s.undoLog[i]
		switch e.op {
		case 0: // undo insert
			if cur, ok := s.primary[e.name]; ok {
				s.freeAccount = append(s.freeAccount, cur)
				delete(s.primary, e.name)
			}
		case 1: // undo overwrite
			if cur, ok := s.primary[e.name]; ok {
				*cur = e.snap
			}
		case 2: // undo delete
			*e.slot = e.snap
			s.primary[e.name] = e.slot
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
	s.delayedVoteSet = make(map[string]struct{}) // reseeded from the scan below
	s.minFrontTime = 0                           // unknown until first scan recomputes
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
		if len(cp.DelayedVotes) > 0 {
			s.delayedVoteSet[nm] = struct{}{} // reseed the delayed-vote index
		}
	}
	return nil
}
