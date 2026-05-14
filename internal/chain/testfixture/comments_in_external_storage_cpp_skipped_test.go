package testfixture

import "testing"

// Skipped ports from hive/tests/unit/tests/comments_in_external_storage.cpp.
//
// The C++ suite exercises hived's hybrid comment_object storage: comments
// live in shared-memory until cashout, then are flushed to a RocksDB-backed
// archive at `comments-rocksdb-path`. Tests assert:
//
//   - basic_checks            comment before/after cashout: shm vs rocksdb residency
//   - nested_comments         parent/child comment archival ordering with cashout
//   - fork_reverts_cashout    a fork that pops a cashout block resurrects the comment
//                             into shm, and the archive does NOT keep a stale copy
//   - inconsistent_comment_archive
//                             node startup fails when the archive's last block
//                             disagrees with state's _cached_irreversible_block
//
// The Go side uses a single state backend (tidwall/btree at memory tier,
// mmap on the disk path). Comments live in the same store as everything
// else; cashout zeroes per-comment fields but doesn't migrate the row to
// a separate file. The state-correctness invariants ARE covered:
//
//   - Comment lifecycle, cashout zeroing, vote-on-paid gating:
//     internal/chain/testfixture/op_comment_payout_test.go,
//     internal/chain/testfixture/curation_reward_test.go,
//     internal/chain/testfixture/op_comment_test.go.
//   - Fork-revert preserves comment state pre-cashout: covered indirectly
//     by undo_cpp_parity_test.go (every typed-store object can undo).
//
// What is NOT covered is the storage-impl boundary (shm vs rocksdb), and
// there is no boundary to cover.

func TestSkippedCommentsInExternalStorageBasicChecks(t *testing.T) {
	t.Skip("hived shm↔rocksdb residency boundary has no Go analogue; comment state invariants covered by op_comment_payout_test.go + curation_reward_test.go")
}

func TestSkippedCommentsInExternalStorageNestedComments(t *testing.T) {
	t.Skip("hived shm↔rocksdb residency for nested comments; Go uses one backend, lifecycle covered elsewhere")
}

func TestSkippedCommentsInExternalStorageForkRevertsCashout(t *testing.T) {
	t.Skip("hived archive-vs-fork divergence; Go undo restores per-object state, no archive layer to desync")
}

func TestSkippedCommentsInExternalStorageInconsistentCommentArchive(t *testing.T) {
	t.Skip("hived startup assert on external-archive LIB mismatch; Go has no external comment archive")
}
