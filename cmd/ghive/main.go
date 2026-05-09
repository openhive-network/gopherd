package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"hived-go/internal/chain"
	"hived-go/internal/config"
	"hived-go/internal/crypto"
	"hived-go/internal/plugins/witness"
	"hived-go/internal/protocol/auth"
	"hived-go/internal/protocol/network"
	"hived-go/internal/state"
	"hived-go/internal/state/genesis"
	"hived-go/internal/state/objects"
)

func main() {
	var (
		showVersion bool
		showNetwork bool
		configPath  string
		dataDir     string
		replay      bool
	)
	flag.BoolVar(&showVersion, "version", false, "print version and exit")
	flag.BoolVar(&showNetwork, "network", false, "print compiled-in network constants and exit")
	flag.StringVar(&configPath, "config", "", "path to config.ini")
	flag.StringVar(&dataDir, "data-dir", "", "data directory (overrides config.ini)")
	flag.BoolVar(&replay, "replay-blockchain", false, "replay the block_log into a fresh state dir before starting")
	flag.Parse()

	if showVersion {
		fmt.Println("ghive 0.1.0 (P0 → P1 in progress)")
		return
	}
	if showNetwork {
		fmt.Printf("network=%s address-prefix=%s chain-id=%s\n",
			network.Name, network.AddressPrefix, network.ChainID.Hex())
		return
	}

	var cfg *config.Config
	if configPath != "" {
		c, err := config.Load(configPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "ghive: config: %v\n", err)
			os.Exit(1)
		}
		cfg = c
	} else {
		cfg = &config.Config{}
	}
	if dataDir != "" {
		cfg.DataDir = dataDir
	}
	if cfg.DataDir == "" {
		fmt.Fprintln(os.Stderr, "ghive: --data-dir or 'data-dir = ...' in config.ini is required")
		os.Exit(2)
	}

	stateDB, err := state.Open(cfg.DataDir, 256<<20)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ghive: open state: %v\n", err)
		os.Exit(1)
	}
	defer stateDB.Close()

	// Bootstrap: if the state dir is empty, apply genesis.
	if err := genesis.Apply(stateDB); err != nil {
		// genesis.Apply tolerates an already-bootstrapped dir
		// (writes are idempotent).
		_ = err
	}

	cdb := chain.New(stateDB)

	// Load witness producers from config. Each (witness, private-key)
	// pair becomes a Producer entry. Without producers the daemon is
	// observer-only.
	var producers []witness.Producer
	for i := 0; i < len(cfg.Witnesses) && i < len(cfg.PrivateKeys); i++ {
		priv, err := parsePrivateKey(cfg.PrivateKeys[i])
		if err != nil {
			fmt.Fprintf(os.Stderr, "ghive: witness %q: %v\n", cfg.Witnesses[i], err)
			os.Exit(1)
		}
		pub := priv.PublicKey()
		producers = append(producers, witness.Producer{
			Account:     cfg.Witnesses[i],
			PrivateKey:  priv,
			PublicKey33: pub,
		})
	}
	wp := witness.New(cdb, stateDB, producers)
	wp.Start()
	defer wp.Stop()

	// Sanity-check producer pubkeys against the witness_object on chain
	// so misconfiguration is caught at startup rather than every block.
	if len(producers) > 0 {
		sess := state.NewSession(stateDB)
		for _, p := range producers {
			w, err := objects.GetWitness(sess, auth.AccountName(p.Account))
			if err != nil {
				fmt.Fprintf(os.Stderr, "ghive: warning: witness %q has no on-chain witness object yet (%v); produced blocks will be rejected\n", p.Account, err)
				continue
			}
			if w.BlockSigningKey != p.PublicKey33 {
				fmt.Fprintf(os.Stderr, "ghive: warning: configured key for %q (%s) does not match on-chain BlockSigningKey (%s); produced blocks will be rejected\n",
					p.Account, p.PublicKey33.EncodeBase58(), w.BlockSigningKey.EncodeBase58())
			}
		}
	}

	if len(producers) > 0 {
		fmt.Printf("ghive: starting with %d witness producer(s)\n", len(producers))
	} else {
		fmt.Println("ghive: starting in observer mode (no witnesses configured)")
	}

	// Wait for interrupt.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh
	fmt.Println("ghive: shutting down...")
}

// parsePrivateKey decodes a config.ini `private-key` string in WIF format
// (base58check, 0x80-prefix, 32-byte secret) into a *crypto.PrivateKey.
// Mirrors hived's `fc::ecc::private_key::wif_to_key`.
func parsePrivateKey(wif string) (*crypto.PrivateKey, error) {
	if wif == "" {
		return nil, fmt.Errorf("config: empty private-key")
	}
	priv, err := crypto.DecodeWIF(wif)
	if err != nil {
		return nil, fmt.Errorf("config: parse WIF private-key: %w", err)
	}
	return priv, nil
}
