// Package genesis — testnet_keys.go derives the deterministic key
// material used by every ghive testnet bring-up. Same input → same WIF
// every time, on every machine, so debugging / replays are reproducible.
//
// Derivation:
//
//	seed   = sha256("ghive-testnet:" || scope || ":" || name)
//	secret = seed[0:32] (with rejection-sampling reseeding if the scalar
//	         falls outside [1, n-1] — astronomically rare for sha256).
//	pubkey = secret * G  (compressed)
//	wif    = base58check(0x80 || secret)
//
// `scope` is one of "witness" or "operator". Names are lowercase ASCII.
//
// The 21 genesis witnesses are named init-1 ... init-21 (the same scheme
// hived's testnet bring-up uses for its built-in init witnesses, and
// also the convention `agent-testnet-orchestration` reads).
//
// Node operator accounts are node-1 ... node-N (N defaults to 5).
//
// All exported helpers here are pure / deterministic — they allocate but
// don't touch the database, so callers (config.ini generator, tests, the
// genesis bootstrap) can call them in any order.
package genesis

import (
	"fmt"

	"hived-go/internal/crypto"
)

// TestnetWitnessCount is the number of init witnesses minted at genesis.
// Matches HIVE_MAX_WITNESSES so the schedule is fully populated from
// block 1 — no bootstrap window where only initminer can produce.
const TestnetWitnessCount = 21

// DefaultTestnetOperatorCount is the default number of pre-funded node
// operator accounts. cmd/testnet-config defaults to running 5 nodes;
// each node owns ~4 witnesses round-robin, plus 1 operator account.
const DefaultTestnetOperatorCount = 5

// TestnetKey is one deterministic key bundle.
type TestnetKey struct {
	Name      string
	WIF       string
	PublicKey crypto.PublicKey
	// Secret is the 32-byte big-endian scalar. Returned for tests +
	// signing; treat as sensitive material.
	Secret [32]byte
}

// TestnetWitnessName returns the canonical name for the i-th genesis
// witness (1-indexed, panics outside [1, TestnetWitnessCount]).
func TestnetWitnessName(i int) string {
	if i < 1 || i > TestnetWitnessCount {
		panic(fmt.Sprintf("genesis: witness index out of range: %d", i))
	}
	return fmt.Sprintf("init-%d", i)
}

// TestnetOperatorName returns the canonical name for the i-th node
// operator account (1-indexed).
func TestnetOperatorName(i int) string {
	if i < 1 {
		panic(fmt.Sprintf("genesis: operator index out of range: %d", i))
	}
	return fmt.Sprintf("node-%d", i)
}

// TestnetWitnessKeys returns the TestnetWitnessCount-long deterministic
// key bundle for the genesis witnesses, ordered init-1 ... init-21.
func TestnetWitnessKeys() []TestnetKey {
	keys := make([]TestnetKey, TestnetWitnessCount)
	for i := 0; i < TestnetWitnessCount; i++ {
		keys[i] = mustDeriveTestnetKey("witness", TestnetWitnessName(i+1))
	}
	return keys
}

// TestnetOperatorKeys returns n deterministic operator key bundles
// (node-1 ... node-n).
func TestnetOperatorKeys(n int) []TestnetKey {
	if n < 1 {
		return nil
	}
	keys := make([]TestnetKey, n)
	for i := 0; i < n; i++ {
		keys[i] = mustDeriveTestnetKey("operator", TestnetOperatorName(i+1))
	}
	return keys
}

// DeriveTestnetKey is the public form of mustDeriveTestnetKey — returns
// an error rather than panicking. scope must be "witness" or "operator";
// callers outside this package should prefer the typed wrappers above.
func DeriveTestnetKey(scope, name string) (TestnetKey, error) {
	if scope != "witness" && scope != "operator" {
		return TestnetKey{}, fmt.Errorf("genesis: unknown key scope %q (want witness|operator)", scope)
	}
	return deriveTestnetKey(scope, name)
}

func mustDeriveTestnetKey(scope, name string) TestnetKey {
	k, err := deriveTestnetKey(scope, name)
	if err != nil {
		// Cannot happen for sha256 output unless secp256k1 rejects
		// every reseed (probability ~ 2^-128 per attempt). Surface as
		// a panic — process can't continue if its own genesis key is
		// invalid.
		panic(fmt.Sprintf("genesis: derive %s/%s: %v", scope, name, err))
	}
	return k
}

func deriveTestnetKey(scope, name string) (TestnetKey, error) {
	// sha256 output is uniform in [0, 2^256); secp256k1 n is just
	// shy of 2^256. Rejection-sampling against scalar==0 or
	// scalar>=n keeps the distribution flat. We re-seed by appending
	// a counter byte — same approach as fc::ecc::private_key::generate.
	for nonce := 0; nonce < 16; nonce++ {
		var seedInput []byte
		if nonce == 0 {
			seedInput = []byte("ghive-testnet:" + scope + ":" + name)
		} else {
			seedInput = append([]byte("ghive-testnet:"+scope+":"+name+":"), byte(nonce))
		}
		digest := crypto.SHA256(seedInput)
		priv, err := crypto.PrivateKeyFromBytes(digest[:])
		if err != nil {
			continue
		}
		// PrivateKeyFromBytes via dcrd accepts any 32-byte input; if
		// the scalar reduces to zero the underlying multiply will
		// produce the point at infinity. Guard with an explicit
		// zero check.
		zeroes := true
		for _, b := range digest {
			if b != 0 {
				zeroes = false
				break
			}
		}
		if zeroes {
			continue
		}
		wif, err := EncodeWIF(digest[:])
		if err != nil {
			return TestnetKey{}, err
		}
		var secret [32]byte
		copy(secret[:], digest[:])
		return TestnetKey{
			Name:      name,
			WIF:       wif,
			PublicKey: priv.PublicKey(),
			Secret:    secret,
		}, nil
	}
	return TestnetKey{}, fmt.Errorf("genesis: failed to derive valid scalar for %s/%s", scope, name)
}
