//go:build !testnet

package genesis

import "gopherd/internal/state"

// ApplyForNetwork seeds the genesis state for the network this binary was
// built for. Every caller that bootstraps a fresh database must go through
// here: calling Apply directly meant the testnet build seeded MAINNET genesis
// and could never agree with a hived testnet peer.
//
// initWitnesses is ignored on mainnet (hived's init_witnesses is a testnet-only
// configuration knob).
func ApplyForNetwork(db *state.Database, initWitnesses []string) error {
	return Apply(db)
}
