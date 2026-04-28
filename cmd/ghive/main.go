package main

import (
	"flag"
	"fmt"
	"os"

	"hived-go/internal/protocol/network"
)

func main() {
	var (
		showVersion bool
		showNetwork bool
	)
	flag.BoolVar(&showVersion, "version", false, "print version and exit")
	flag.BoolVar(&showNetwork, "network", false, "print compiled-in network constants and exit")
	flag.Parse()

	if showVersion {
		fmt.Println("ghive (pre-P0 scaffold)")
		return
	}
	if showNetwork {
		fmt.Printf("network=%s address-prefix=%s chain-id=%s\n",
			network.Name, network.AddressPrefix, network.ChainID.Hex())
		return
	}
	fmt.Fprintln(os.Stderr, "ghive: not implemented yet — this is a scaffold. See plans/ for roadmap.")
	os.Exit(2)
}
