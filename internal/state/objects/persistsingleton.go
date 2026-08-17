package objects

import "gopherd/internal/state/persiststore"

// PersistSingleton is a persiststore-backed single-row table (chainbase-in-Go
// phase 5) for the singleton objects: DynamicGlobalProperty, FeedHistory,
// WitnessSchedule, FutureWitnessSchedule, the reward funds, etc. The caller
// supplies the encoded bytes (its existing Append*/Read* codec); the row
// survives restart without replay like every other persiststore table.
//
// The DGPO singleton in particular doubles as the HEAD-BLOCK marker: after a
// restart, reading it tells the node where to resume — no replay.
//
// Keyed tables (witness, witness_vote, escrow, savings, convert, limit_order,
// RC, …) follow the SAME shape as PersistAccountStore / PersistCommentStore /
// PersistCommentVoteStore over persiststore: a primary key + optional ordered
// secondary table(s). The big three are done with bespoke wrappers; the long
// tail reuses that proven pattern.
type PersistSingleton struct {
	ps *persiststore.Store
}

var singletonKey = string([]byte{0x00}) // the one row

func OpenPersistSingleton(dir string, reserveBytes uint64) (*PersistSingleton, error) {
	ps, err := persiststore.Open(dir, reserveBytes)
	if err != nil {
		return nil, err
	}
	return &PersistSingleton{ps: ps}, nil
}

// Get returns the row's encoded bytes (zero-copy; do not retain across a Put).
func (p *PersistSingleton) Get() ([]byte, bool) { return p.ps.Get(singletonKey) }

// Put stores the row's encoded bytes.
func (p *PersistSingleton) Put(encoded []byte) { _ = p.ps.Set(singletonKey, encoded) }

func (p *PersistSingleton) SetUndoEnabled(b bool) { p.ps.SetUndoEnabled(b) }
func (p *PersistSingleton) SnapshotPoint() int    { return p.ps.SnapshotPoint() }
func (p *PersistSingleton) RestoreToPoint(n int)  { p.ps.RestoreToPoint(n) }
func (p *PersistSingleton) CommitPoint()          { p.ps.CommitPoint() }

func (p *PersistSingleton) Flush() error { return p.ps.Flush() }
func (p *PersistSingleton) Close() error { return p.ps.Close() }
