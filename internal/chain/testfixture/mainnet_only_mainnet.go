//go:build !testnet

package testfixture

import "testing"

// skipIfTestnetSemantics is a no-op on mainnet builds, where these fixtures
// are exactly the parity tests we want to run.
func skipIfTestnetSemantics(t *testing.T) {}
