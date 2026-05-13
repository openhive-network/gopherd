package crypto

import "testing"

// Skipped ports from hive/tests/unit/tests/crypto_memo_tests.cpp.
//
// The C++ suite exercises `hive::protocol::crypto_memo`'s
// encrypt/decrypt/serialize round-trip, including a regression for the
// "buggy" field order shipped in hived 1.27.5-1.28.6.
//
// hived-go has no memo encrypt/decrypt path yet — internal/crypto only
// covers secp256k1, ECDH primitives, base58/WIF, and CityHash. Memo
// encryption is a client-side feature (wallets); the replay /
// validation engines never touch encrypted memos.
//
// If a memo crypto port lands later, the upstream `memo_crypto::encrypt`
// /`decrypt` / `dump_to_string` / `load_from_string` helpers are the
// reference implementation and the buggy-format compatibility test
// (commit 3af2709be) MUST be preserved.

// TestSkippedCryptoMemoEncodingRoundtrip — crypto_memo_encoding_roundtrip:
// encrypts/decrypts via memo_crypto with a fixed nonce, asserts the '#'
// marker prefix, verifies serialization round-trip and the (from,to,
// nonce,check,encrypted) field order.
func TestSkippedCryptoMemoEncodingRoundtrip(t *testing.T) {
	t.Skip("no Go memo crypto implementation; covered by C++ memo_crypto only")
}

// TestSkippedCryptoMemoDecryptBuggyFormat — crypto_memo_decrypt_buggy_format:
// regression test that memos encrypted with the 1.27.5-1.28.6 buggy
// field order (nonce,check,encrypted,from,to) still decrypt under the
// current decoder.
func TestSkippedCryptoMemoDecryptBuggyFormat(t *testing.T) {
	t.Skip("no Go memo crypto implementation; 1.27.5-1.28.6 buggy-format compat unneeded")
}

// TestSkippedCryptoMemoVariousMessages — crypto_memo_various_messages:
// encrypt/decrypt round-trip across short/long/Unicode/numeric/special
// character messages and alternate sender/recipient pairs.
func TestSkippedCryptoMemoVariousMessages(t *testing.T) {
	t.Skip("no Go memo crypto implementation")
}

// TestSkippedCryptoMemoErrorHandling — crypto_memo_error_handling: wrong
// key returns the input unchanged; missing '#' marker is passed
// through.
func TestSkippedCryptoMemoErrorHandling(t *testing.T) {
	t.Skip("no Go memo crypto implementation")
}
