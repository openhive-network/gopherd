package objects

import (
	"encoding/binary"
	"path/filepath"
)

// PersistTables is the chainbase-in-Go restart orchestrator (phase 6): it owns
// every persiststore-backed table under one state directory and gives the node
// the lifecycle it needs to RESUME AT HEAD without replay:
//
//   - OpenPersistTables(dir): open (and thereby restore) all tables. If the dir
//     holds a prior clean run, every table comes back at head — no replay. The
//     head block is read from the head singleton (in production: the DGPO).
//   - FlushAll(): clean-shutdown flush of every table (msync + index sidecar).
//     Call from the SIGTERM handler.
//   - CloseAll(): FlushAll + unmap.
//   - SnapshotAll/RestoreAll/CommitAll/SetUndoEnabledAll: fan the chain's
//     per-transaction savepoint across all tables (the SnapshotSubsystem
//     contract) so rollback is atomic.
//
// With this in place, snapshots revert to their proper role — a CASCADE-DEBUG
// tool to jump back many blocks — rather than the restart mechanism.
//
// Reserve sizes are sparse (only touched pages cost disk), so they can be
// generous; comments/votes get the large ones.
type PersistTables struct {
	dir      string
	Accounts *PersistAccountStore
	Comments *PersistCommentStore
	Votes    *PersistCommentVoteStore
	Head     *PersistSingleton // production: the DGPO row (carries head_block_number)
}

// Reserve sizes (sparse). Comments/votes dominate; accounts ~1.4M; head trivial.
const (
	ReserveComments = 256 << 30 // 256 GiB
	ReserveVotes    = 256 << 30
	ReserveAccounts = 16 << 30 // 16 GiB
	ReserveHead     = 1 << 20  // 1 MiB
)

// OpenPersistTables opens all tables with the production (large, sparse)
// reserves.
func OpenPersistTables(dir string) (*PersistTables, error) {
	return OpenPersistTablesWithReserves(dir, ReserveAccounts, ReserveComments, ReserveVotes, ReserveHead)
}

// OpenPersistTablesWithReserves opens all tables with explicit sparse reserves
// (tests pass small ones).
func OpenPersistTablesWithReserves(dir string, accR, comR, votR, headR uint64) (*PersistTables, error) {
	acc, err := OpenPersistAccountStore(filepath.Join(dir, "accounts"), accR)
	if err != nil {
		return nil, err
	}
	com, err := OpenPersistCommentStore(filepath.Join(dir, "comments"), comR)
	if err != nil {
		acc.Close()
		return nil, err
	}
	vot, err := OpenPersistCommentVoteStore(filepath.Join(dir, "votes"), votR)
	if err != nil {
		acc.Close()
		com.Close()
		return nil, err
	}
	head, err := OpenPersistSingleton(filepath.Join(dir, "head"), headR)
	if err != nil {
		acc.Close()
		com.Close()
		vot.Close()
		return nil, err
	}
	return &PersistTables{dir: dir, Accounts: acc, Comments: com, Votes: vot, Head: head}, nil
}

// SetHeadBlock records the resume point (production: written as part of the DGPO
// row; here a 4-byte big-endian block number suffices for the orchestration).
func (t *PersistTables) SetHeadBlock(n uint32) {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], n)
	t.Head.Put(b[:])
}

// HeadBlock returns the persisted head (0,false if none → start from genesis).
func (t *PersistTables) HeadBlock() (uint32, bool) {
	b, ok := t.Head.Get()
	if !ok || len(b) < 4 {
		return 0, false
	}
	return binary.BigEndian.Uint32(b[:4]), true
}

func (t *PersistTables) SetUndoEnabledAll(b bool) {
	t.Accounts.SetUndoEnabled(b)
	t.Comments.SetUndoEnabled(b)
	t.Votes.SetUndoEnabled(b)
	t.Head.SetUndoEnabled(b)
}

// snapPoint bundles each table's savepoint marker for an atomic restore.
type snapPoint struct{ acc, com, vot, head int }

func (t *PersistTables) SnapshotAll() snapPoint {
	return snapPoint{t.Accounts.SnapshotPoint(), t.Comments.SnapshotPoint(), t.Votes.SnapshotPoint(), t.Head.SnapshotPoint()}
}
func (t *PersistTables) RestoreAll(sp snapPoint) {
	t.Accounts.RestoreToPoint(sp.acc)
	t.Comments.RestoreToPoint(sp.com)
	t.Votes.RestoreToPoint(sp.vot)
	t.Head.RestoreToPoint(sp.head)
}
func (t *PersistTables) CommitAll() {
	t.Accounts.CommitPoint()
	t.Comments.CommitPoint()
	t.Votes.CommitPoint()
	t.Head.CommitPoint()
}

// FlushAll persists every table for a clean restart (call on SIGTERM).
func (t *PersistTables) FlushAll() error {
	for _, f := range []func() error{t.Accounts.Flush, t.Comments.Flush, t.Votes.Flush, t.Head.Flush} {
		if err := f(); err != nil {
			return err
		}
	}
	return nil
}

// CloseAll flushes + unmaps every table.
func (t *PersistTables) CloseAll() error {
	var firstErr error
	for _, c := range []func() error{t.Accounts.Close, t.Comments.Close, t.Votes.Close, t.Head.Close} {
		if err := c(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
