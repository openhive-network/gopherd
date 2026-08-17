package blocklog

import "testing"

// Skipped ports from hive/tests/unit/tests/block_storage_tests.cpp.
//
// All 29 cases test C++-hived's compaction/migration tooling for the
// block_log (`compress_block_log` / `block_log_util`) and the
// auto-split + artifact-regeneration policy embedded in
// block_log::open. The Go side has READ + WRITE support for both
// legacy (single-file) and split (block_log_part.NNNN) layouts —
// covered by TestReaderLegacyRoundTrip, TestWriterByteIdenticalToMainnet
// and the parity-loop tests — but does NOT have:
//
//   1. A converter binary (legacy↔split, split↔pruned).
//   2. Auto-split at runtime when crossing the 1M-block boundary.
//   3. An artifacts-regeneration-policy flag (regenerate=true/false).
//   4. A `get_block_range` API on the public Reader surface.
//   5. A snapshot tool that bundles state + block_log together.
//
// These are operational concerns (data-center migrations, on-disk
// compaction) rather than consensus, so the tests are tracked here as
// `t.Skip` stubs for inventory purposes. Each is named so that a
// grep for the upstream BOOST_AUTO_TEST_CASE lands directly on its
// stub, and the rationale records why it isn't ported.
//
// If any of the missing tooling lands, replace the Skip with an
// actual port — the C++ assertions translate fairly directly.

// TestSkippedBlockStorageLegacyToZero — legacy_to_zero. Converts a
// legacy single-file block_log into the same-on-disk-format with the
// compact-then-decompact tool. Verifies the round-trip is bit-identical.
func TestSkippedBlockStorageLegacyToZero(t *testing.T) {
	t.Skip("Go has no compress_block_log/legacy→legacy converter; round-trip covered by TestWriterByteIdenticalToMainnet")
}

func TestSkippedBlockStorageLegacyToSplit(t *testing.T) {
	t.Skip("Go has no legacy→split converter binary")
}
func TestSkippedBlockStorageSplitToZero(t *testing.T) {
	t.Skip("Go has no split→legacy converter binary")
}
func TestSkippedBlockStorageCleanReplayLegacyToZero(t *testing.T) {
	t.Skip("hived replay+convert chain test; Go replay covers the read side only")
}
func TestSkippedBlockStorageCleanReplayLegacyToSplit(t *testing.T) {
	t.Skip("hived replay+convert chain test; no Go-side converter")
}
func TestSkippedBlockStorageCleanReplaySplitToEffectivelyPruned(t *testing.T) {
	t.Skip("Go has no pruning facility (compact_block_log --prune)")
}
func TestSkippedBlockStorageReplayLegacyToZero(t *testing.T) {
	t.Skip("hived replay+convert chain test; no Go-side converter")
}
func TestSkippedBlockStorageReplayLegacyToSplit(t *testing.T) {
	t.Skip("hived replay+convert chain test; no Go-side converter")
}
func TestSkippedBlockStorageSplitOverSplit(t *testing.T) {
	t.Skip("hived split-over-split idempotency test; Go has no converter")
}
func TestSkippedBlockStorageAutoSplit(t *testing.T) {
	t.Skip("Go writer does not auto-split at 1M-block boundary at runtime")
}
func TestSkippedBlockStorageAutoSplit2(t *testing.T) {
	t.Skip("Go writer does not auto-split at 1M-block boundary at runtime")
}
func TestSkippedBlockStorageAutoSplit3(t *testing.T) {
	t.Skip("Go writer does not auto-split at 1M-block boundary at runtime")
}
func TestSkippedBlockStorageAutoSplit4(t *testing.T) {
	t.Skip("Go writer does not auto-split at 1M-block boundary at runtime")
}
func TestSkippedBlockStorageGetBlockRange(t *testing.T) {
	t.Skip("Go Reader has no get_block_range public API; ReadAt loops cover the equivalent")
}
func TestSkippedBlockStorageIncrementalReplayPruned(t *testing.T) {
	t.Skip("Go has no pruned-mode incremental replay tooling")
}
func TestSkippedBlockStorageSnapshotLegacyToZero(t *testing.T) {
	t.Skip("Go state-snapshot tool does not bundle block_log")
}
func TestSkippedBlockStorageSnapshotZeroToZero(t *testing.T) {
	t.Skip("Go state-snapshot tool does not bundle block_log")
}
func TestSkippedBlockStorageSnapshotReplayLegacyToSplit(t *testing.T) {
	t.Skip("Go has no combined state-snapshot+converter flow")
}
func TestSkippedBlockStorageSnapshotReplayLegacyToZero(t *testing.T) {
	t.Skip("Go has no combined state-snapshot+converter flow")
}
func TestSkippedBlockStorageSnapshotReplaySplitToSplit(t *testing.T) {
	t.Skip("Go has no combined state-snapshot+converter flow")
}
func TestSkippedBlockStorageSnapshotReplaySplitToZero(t *testing.T) {
	t.Skip("Go has no combined state-snapshot+converter flow")
}
func TestSkippedBlockStorageSnapshotPrunedToPruned(t *testing.T) {
	t.Skip("Go has no pruned-mode snapshot tooling")
}
func TestSkippedBlockStorageSnapshotReplayPrunedToPruned(t *testing.T) {
	t.Skip("Go has no pruned-mode snapshot tooling")
}
func TestSkippedBlockStorageAllowArtifactsRegenFalseRejectsNonexistentLog(t *testing.T) {
	t.Skip("Go has no allow_artifacts_regeneration policy flag; missing-log is a hard error in NewReader")
}
func TestSkippedBlockStorageAllowArtifactsRegenTrueCreatesNonexistentLog(t *testing.T) {
	t.Skip("Go has no allow_artifacts_regeneration policy flag")
}
func TestSkippedBlockStorageAllowArtifactsRegenFalseOpensExistingLogReadonly(t *testing.T) {
	t.Skip("Go has no allow_artifacts_regeneration policy flag")
}
func TestSkippedBlockStorageAllowArtifactsRegenParameterPassedThroughWrapper(t *testing.T) {
	t.Skip("Go has no allow_artifacts_regeneration policy flag")
}
func TestSkippedBlockStorageAllowArtifactsRegenFalseSuppressesAutoRepair(t *testing.T) {
	t.Skip("Go has no artifact auto-repair flow")
}
func TestSkippedBlockStorageAllowArtifactsRegenTrueRecreatesMissingArtifacts(t *testing.T) {
	t.Skip("Go has no artifact auto-repair flow")
}
