//go:build testnet

package main

import "gopherd/internal/protocol/network"

// applyChainIDOverride retargets the node at a testnet started with a custom
// --chain-id, which hived supports and operators use to isolate networks.
func applyChainIDOverride(id network.Hash32) { network.SetChainID(id) }
