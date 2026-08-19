//go:build !testnet

package main

import (
	"fmt"
	"os"
)

// cmdTestnetBootstrap exists only on a testnet build. On mainnet it refuses:
// it would vest and cast governance votes as initminer, which is meaningless
// on a chain where that account is not under our control.
func cmdTestnetBootstrap([]string) int {
	fmt.Fprintln(os.Stderr, "gopherd: testnet-bootstrap requires a testnet build (-tags testnet)")
	return 2
}
