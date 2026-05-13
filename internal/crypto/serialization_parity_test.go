// Tests ported from hive/tests/unit/tests/serialization_tests.cpp covering
// fc::ecc::extended_(private|public)_key serialization. These cases require
// BIP32 extended-key support (HD wallet derivation) which Go's crypto
// package doesn't currently expose — only raw secp256k1 + WIF. Skip until
// extended-key support lands.
package crypto

import "testing"

// TestExtendedPrivateKeyTypeRoundTrip ports
// serialization_tests.cpp::extended_private_key_type_test. C++ generates a
// random extended_private_key (BIP32-style: private key + chain code +
// depth/parent_fp/child_num), formats via std::string conversion (base58 +
// 4-byte checksum), parses back, and asserts equality.
func TestExtendedPrivateKeyTypeRoundTrip(t *testing.T) {
	t.Skip("MISSING: fc::ecc::extended_private_key (BIP32) — only raw secp256k1 + WIF in Go")
}

// TestExtendedPublicKeyTypeRoundTrip ports
// serialization_tests.cpp::extended_public_key_type_test. Same blocker as
// TestExtendedPrivateKeyTypeRoundTrip.
func TestExtendedPublicKeyTypeRoundTrip(t *testing.T) {
	t.Skip("MISSING: fc::ecc::extended_public_key (BIP32)")
}
