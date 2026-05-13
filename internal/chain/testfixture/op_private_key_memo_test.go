package testfixture

import "testing"

// TestPrivateKeyMemo would port operation_tests.cpp::private_key_memo_test.
//
// The C++ test exercises witness_plugin's sanity check that a memo
// passed in transfer / recurrent_transfer / transfer_to_savings /
// transfer_from_savings is NOT a private key derived from the
// account's master password (a common phishing-trap pattern). It
// requires:
//
//   1. The witness_plugin's pre-transfer hook (Go has no plugin layer
//      yet — see internal/p2p/sync and internal/chain; no
//      hook/pre-broadcast filter exists).
//   2. ECIES memo decryption to recover candidate keys from encrypted
//      memos (Go has no equivalent of fc::ecc::ecies + the wallet's
//      "from #..." prefix scan).
//   3. A way to instantiate witness_plugin in the testfixture.
//
// All three are out of scope for a 15-minute port pass. Marking
// SKIP for visibility; the gap is plugin-layer infrastructure, not a
// consensus parity issue.
func TestPrivateKeyMemo(t *testing.T) {
	t.Skip("missing: witness_plugin memo-leak hook + ECIES memo decryption — see comment")
}
