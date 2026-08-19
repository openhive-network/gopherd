//go:build !testnet

package main

import (
	"fmt"
	"os"

	"gopherd/internal/protocol/network"
)

// applyChainIDOverride REFUSES on a mainnet build. Mainnet's chain ID is a
// consensus constant; a node that could be pointed at a different one would
// happily sign transactions for another chain.
func applyChainIDOverride(network.Hash32) {
	fmt.Fprintln(os.Stderr, "gopherd node: --chain-id is testnet-only; mainnet's chain ID is fixed")
	os.Exit(2)
}
