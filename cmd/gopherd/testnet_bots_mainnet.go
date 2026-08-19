//go:build !testnet

package main

import (
	"fmt"
	"os"
)

// cmdTestnetBots is testnet-only. It signs with keys it derives itself and
// would be meaningless against mainnet, where those accounts do not exist.
func cmdTestnetBots([]string) int {
	fmt.Fprintln(os.Stderr, "gopherd: testnet-bots requires a testnet build (-tags testnet)")
	return 2
}
