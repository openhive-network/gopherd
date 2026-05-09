// Package config parses hived-style config.ini files into structured
// settings. The file format mirrors Boost.program_options:
//
//   key = value          (single value)
//   key = "value"        (quoted, supports embedded spaces)
//   key = value          (repeated key → slice)
//   # comment            (line ignored)
//
// Boost's `multitoken` keys (witness, private-key, plugin) appear
// multiple times, each adding to the list. Single-valued keys take
// the LAST occurrence.
package config

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"
)

// Config holds the parsed config.ini. Single-valued fields use the
// last occurrence; list-valued fields aggregate all.
type Config struct {
	// Witnesses is the list of account names this daemon will produce
	// blocks for. Each must have a matching PrivateKeys entry.
	Witnesses []string
	// PrivateKeys are the WIF-encoded keys (one per Witness).
	PrivateKeys []string
	// Plugins is the list of plugins to load.
	Plugins []string
	// P2PEndpoint, RPCEndpoint, DataDir, etc.
	P2PEndpoint string
	// P2PSeedNodes is the list of "host:port" peers to attempt to dial
	// on startup (boost-style "p2p-seed-node" key, repeats allowed).
	P2PSeedNodes []string
	// P2PMaxOutboundPeers caps active outbound peer count. 0 = p2pnode
	// default (32). Maps to "p2p-max-outbound-peers = N" in config.ini.
	P2PMaxOutboundPeers int
	// P2PMaxConcurrentDials caps in-flight dial attempts. 0 = p2pnode
	// default (24). Maps to "p2p-max-concurrent-dials = N" in config.ini.
	P2PMaxConcurrentDials int
	RPCEndpoint  string
	DataDir      string
	// SharedFileSize is the soft memory bound (Boost INI string like
	// "20G", "8G"). We parse it into bytes for our Pebble cache.
	SharedFileSize string
	// GenesisType selects the genesis bootstrap variant: "" or "mainnet"
	// uses genesis.Apply (mainnet); "testnet" uses genesis.ApplyTestnet
	// (21 init-N witnesses + N operator accounts pre-funded). The
	// testnet-config tool emits "genesis-type = testnet" by default.
	GenesisType string
	// Raw holds every (key, value) pair encountered so callers can
	// access keys we don't promote to typed fields.
	Raw map[string][]string
}

// Load reads a config.ini path. Use Parse to read from any io.Reader.
func Load(path string) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return Parse(f)
}

// Parse reads a config.ini-style stream.
func Parse(r io.Reader) (*Config, error) {
	c := &Config{Raw: make(map[string][]string)}
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		// Strip inline comment (# or ; not inside a quoted string).
		if idx := stripInlineComment(line); idx >= 0 {
			line = line[:idx]
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq < 0 {
			continue
		}
		key := strings.TrimSpace(line[:eq])
		val := strings.TrimSpace(line[eq+1:])
		val = unquote(val)
		c.Raw[key] = append(c.Raw[key], val)
		switch key {
		case "witness":
			c.Witnesses = append(c.Witnesses, val)
		case "private-key":
			c.PrivateKeys = append(c.PrivateKeys, val)
		case "plugin":
			// Boost's "plugin = a b c" syntax space-separates names
			// in one line — split.
			for _, p := range strings.Fields(val) {
				c.Plugins = append(c.Plugins, p)
			}
		case "p2p-endpoint":
			c.P2PEndpoint = val
		case "p2p-seed-node":
			// Repeated "p2p-seed-node = host:port" lines accumulate.
			// Boost also accepts space-separated lists on one line.
			for _, s := range strings.Fields(val) {
				c.P2PSeedNodes = append(c.P2PSeedNodes, s)
			}
		case "p2p-max-outbound-peers":
			// Optional integer override for the outbound peer cap.
			// Parse-tolerant: invalid values just leave the default.
			var v int
			if _, err := fmt.Sscanf(val, "%d", &v); err == nil && v > 0 {
				c.P2PMaxOutboundPeers = v
			}
		case "p2p-max-concurrent-dials":
			var v int
			if _, err := fmt.Sscanf(val, "%d", &v); err == nil && v > 0 {
				c.P2PMaxConcurrentDials = v
			}
		case "rpc-endpoint", "webserver-http-endpoint":
			c.RPCEndpoint = val
		case "data-dir":
			c.DataDir = val
		case "shared-file-size":
			c.SharedFileSize = val
		case "genesis-type":
			c.GenesisType = val
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if len(c.Witnesses) != 0 && len(c.Witnesses) != len(c.PrivateKeys) {
		return nil, fmt.Errorf("config: %d witnesses but %d private-keys (must match)",
			len(c.Witnesses), len(c.PrivateKeys))
	}
	return c, nil
}

// stripInlineComment returns the index of the first comment marker
// outside any quoted region, or -1 if none. Recognized markers are
// `#` and `;` (Boost INI-compatible).
func stripInlineComment(line string) int {
	inQuote := false
	for i, r := range line {
		switch r {
		case '"':
			inQuote = !inQuote
		case '#', ';':
			if !inQuote {
				return i
			}
		}
	}
	return -1
}

// unquote strips surrounding double-quotes if both ends are quoted.
func unquote(s string) string {
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return s[1 : len(s)-1]
	}
	return s
}
