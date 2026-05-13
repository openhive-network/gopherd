package op

import "testing"

// Skipped ports from hive/tests/unit/tests/basic_tests.cpp. Grouped here
// (one t.Skip per upstream test case) so that:
//
//   - the inventory is visible in one place,
//   - `go test -run TestSkipped` lists every gap with its rationale,
//   - if any of the missing Go subsystems gains an equivalent later, a
//     grep for the upstream test name lands directly on the stub.
//
// "Portable" basic_tests cases that ARE ported live elsewhere:
//
//   - adjust_balance_test           → internal/chain/testfixture/adjust_balance_test.go
//   - curation_weight_test          → internal/chain/curation_weight_test.go
//   - fc_uint128_to_string,
//     uint128_approx_sqrt portions  → internal/math128/math128_test.go
//   - valid_name_test               → internal/protocol/auth/account_name_test.go
//   - merkle_root                   → internal/protocol/tx/merkle_root_test.go
//     (full 1-..-10-tx incremental growth port matching the C++ tree
//     diagrams). Also exercised every block ingest in replay via
//     internal/blocklog/*_test.go.

// TestSkippedFixedStringVerification — fixed_string_verification:
// exercises C++'s fixed_string<N> truncation toggle
// (account_name_type::set_verify). Go represents account names as
// plain strings + an explicit IsValidAccountName check, so there is no
// equivalent toggle to test. Length validity is already covered by
// internal/protocol/auth/account_name_test.go::TestIsValidAccountName.
func TestSkippedFixedStringVerification(t *testing.T) {
	t.Skip("FC fixed_string<N> truncation has no Go analogue; length covered by auth.TestIsValidAccountName")
}

// TestSkippedPow2Equihash — equihash POW verification. Go skips the PoW
// hash + signing per project memory (PoW deferred). When equihash lands,
// port from basic_tests.cpp:195.
func TestSkippedPow2Equihash(t *testing.T) {
	t.Skip("equihash PoW deferred; see project_full_parity_gaps memo")
}

// TestSkippedParseSize — parses "5G"/"512M" style size strings via
// fc::parse_size. Go has no equivalent helper in the codebase; nothing
// reads sizes from human-readable strings yet.
func TestSkippedParseSize(t *testing.T) {
	t.Skip("no Go equivalent of fc::parse_size in the codebase")
}

// TestSkippedChainObjectSize — asserts sizeof() of every chain object
// matches a frozen reference value. Go's object layout differs from
// C++ (no alignment requirements / different field types), so the
// numerical reference values are inapplicable.
func TestSkippedChainObjectSize(t *testing.T) {
	t.Skip("C++ sizeof() reference values not applicable to Go struct layouts")
}

// TestSkippedChainObjectChecksum — FC type-reflection checksum used to
// detect accidental schema drift. Go does not use FC's reflection
// machinery; schema drift surfaces via the FC raw-encode/decode tests
// in internal/fcraw/.
func TestSkippedChainObjectChecksum(t *testing.T) {
	t.Skip("FC reflection-checksum has no Go analogue; covered by fcraw round-trip tests")
}

// TestSkippedFCStaticVariantAlignment — sizeof/alignof of fc::static_variant.
// Go uses a tagged-union (op.Op with a Tag byte + ptr) — different memory
// layout, different alignment.
func TestSkippedFCStaticVariantAlignment(t *testing.T) {
	t.Skip("FC static_variant layout not applicable to Go op.Op tagged-union")
}

// TestSkippedFCOptionalAlignment — sizeof/alignof of fc::optional. Go
// uses pointers (or sentinel values) instead of fc::optional.
func TestSkippedFCOptionalAlignment(t *testing.T) {
	t.Skip("FC optional layout not applicable to Go pointer-based optionals")
}

// TestSkippedFCJSONIntegerFormatting — fc::json integer-format edge
// cases. Go uses encoding/json + custom shims; covered indirectly by
// api package round-trip tests.
func TestSkippedFCJSONIntegerFormatting(t *testing.T) {
	t.Skip("encoding/json behaves correctly out of the box; not a structural Go concern")
}

// TestSkippedCreateArrayFromFCJSON — fc::json array parsing. Covered
// implicitly wherever the api package decodes blocks/ops.
func TestSkippedCreateArrayFromFCJSON(t *testing.T) {
	t.Skip("Go uses encoding/json; covered indirectly by api tests")
}

// TestSkippedFCJSONIsValid — fc::json::is_valid validator.
func TestSkippedFCJSONIsValid(t *testing.T) {
	t.Skip("Go uses encoding/json's strict-by-default decoder")
}

// TestSkippedDecodingTypesMechanism — FC reflection-based type
// decoder. Go relies on per-tag readers in op.readXxx; covered by
// roundtrip tests in op_test.go.
func TestSkippedDecodingTypesMechanism(t *testing.T) {
	t.Skip("FC type-reflection has no Go analogue; covered by per-tag read tests")
}

// TestSkippedDecodedTypeDataJSONOperations — JSON-encoded reflection
// metadata. Not applicable.
func TestSkippedDecodedTypeDataJSONOperations(t *testing.T) {
	t.Skip("FC reflection metadata not applicable to Go")
}

// TestSkippedAdditionalAllocations — FC allocator bookkeeping. Go's
// garbage collector replaces FC's custom allocators.
func TestSkippedAdditionalAllocations(t *testing.T) {
	t.Skip("Go's GC replaces FC allocator bookkeeping")
}

// TestSkippedGetWordList — BIP39-style wordlist. Go's codebase has no
// BIP39 mnemonic generator in scope.
func TestSkippedGetWordList(t *testing.T) {
	t.Skip("no BIP39 wordlist in Go codebase")
}

// TestSkippedAuthorizationRedirections — has_authorization across
// owner/active/posting redirects through other accounts' authorities.
// Requires the full auth-resolution chain (including witness signing
// key fallback). Not yet exposed as a unit-testable Go function.
func TestSkippedAuthorizationRedirections(t *testing.T) {
	t.Skip("authority redirection resolution not exposed for unit-level testing in Go")
}

// TestSkippedAuthorizationSpeed — performance benchmark for
// has_authorization on deep authority chains. Benchmark, not a
// correctness test.
func TestSkippedAuthorizationSpeed(t *testing.T) {
	t.Skip("performance benchmark; not a correctness test")
}
