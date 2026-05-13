package chain

import "testing"

// Skipped ports from hive/tests/unit/tests/transaction_util_tests.cpp.
//
// The C++ suite exercises `hive::protocol::signing_keys_collector`'s
// auto-detect-keys algorithm over a fully-instantiated chain DB:
//
//   - it creates accounts with various authority threshold / key-weight
//     combinations (single key, 1-of-2, 2-of-2, indirect via account_auths,
//     circular & deep nesting, invalid),
//   - pushes operations referencing those accounts,
//   - asserts that collect_signing_keys() walks the authority graph
//     up to HIVE_MAX_SIG_CHECK_DEPTH levels and returns the minimal
//     set of public keys that satisfies every required authority.
//
// hived-go's equivalent helpers live in:
//
//   - internal/chain/verify_authority.go  (RequiredAuthorities + VerifyAuthority)
//   - internal/protocol/op/authorities.go (per-op required_active/owner/posting)
//
// The basic per-op required-authority categorization is already covered
// by:
//
//   - internal/protocol/op/authorities_test.go
//   - internal/protocol/op/authorities_cpp_parity_test.go
//
// What hived-go does NOT have is an exported "collect minimal signing
// key set" helper that walks indirect account_auths recursion to
// HIVE_MAX_SIG_CHECK_DEPTH and returns the public-key set the wallet
// should sign with. That helper is wallet/CLI-facing in hived; the Go
// replay/validation engine only needs the inverse direction (given the
// recovered keys + tx, does this satisfy every authority? → covered by
// VerifyAuthority). Until a wallet-side autodetect lands, these stay
// skipped.

// TestSkippedAutodetectKeysSimple — autodetect_keys_simple: one key per
// authority, single owner/active/posting key each; transfers,
// decline_voting_rights, comments; expects collect_signing_keys to
// return exactly the {alice_active}/{alice_owner}/{alice_posting}
// subset that matches each operation.
func TestSkippedAutodetectKeysSimple(t *testing.T) {
	t.Skip("no Go signing_keys_collector::collect_signing_keys (wallet-side); inverse path in VerifyAuthority is covered")
}

// TestSkippedAutodetectKeys1Of2 — autodetect_keys_1_of_2: each authority
// has two keys with weight 1 and threshold 1 (1-of-2 multisig);
// collector should pick the FIRST satisfying key.
func TestSkippedAutodetectKeys1Of2(t *testing.T) {
	t.Skip("no Go signing_keys_collector; wallet-side helper not ported")
}

// TestSkippedAutodetectKeys2Of2 — autodetect_keys_2_of_2: two keys at
// weight 1 and threshold 2; both must be returned to satisfy the
// authority.
func TestSkippedAutodetectKeys2Of2(t *testing.T) {
	t.Skip("no Go signing_keys_collector; wallet-side helper not ported")
}

// TestSkippedAutodetectKeysIndirect1Of2 — autodetect_keys_indirect_1_of_2:
// authorities reference OTHER accounts (account_auths) at weight 1 of
// threshold 1; collector walks indirect refs.
func TestSkippedAutodetectKeysIndirect1Of2(t *testing.T) {
	t.Skip("no Go account_auths recursion in collect path; VerifyAuthority handles the inverse case")
}

// TestSkippedAutodetectKeysIndirect2Of2 — autodetect_keys_indirect_2_of_2:
// 2-of-2 with one or both legs being indirect account_auth references.
func TestSkippedAutodetectKeysIndirect2Of2(t *testing.T) {
	t.Skip("no Go account_auths recursion in collect path")
}

// TestSkippedAutodetectKeysCircularAndDeep — autodetect_keys_circular_and_deep:
// account A's authority references B; B's references A; also tests an
// indirection chain at depth HIVE_MAX_SIG_CHECK_DEPTH. Collector must
// terminate and report failure on cycle / exceeded depth.
func TestSkippedAutodetectKeysCircularAndDeep(t *testing.T) {
	t.Skip("no Go signing_keys_collector cycle / depth-limit walker")
}

// TestSkippedAutodetectKeysInvalid — autodetect_keys_invalid: operations
// referencing unknown accounts / impossible thresholds; collector
// surfaces a clean error.
func TestSkippedAutodetectKeysInvalid(t *testing.T) {
	t.Skip("no Go signing_keys_collector error reporting; VerifyAuthority surfaces missing-account errors instead")
}
