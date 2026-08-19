//go:build testnet

package genesis

import "gopherd/internal/state"

// ApplyForNetwork seeds hived-compatible testnet genesis. See
// genesis_testnet.go for why this must mirror hived exactly.
func ApplyForNetwork(db *state.Database, initWitnesses []string) error {
	return ApplyTestnet(db, initWitnesses)
}
