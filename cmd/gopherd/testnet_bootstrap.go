//go:build testnet

package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"gopherd/internal/api"
	"gopherd/internal/crypto"
	"gopherd/internal/protocol/asset"
	"gopherd/internal/protocol/auth"
	"gopherd/internal/protocol/network"
	"gopherd/internal/protocol/op"
	"gopherd/internal/protocol/tx"
	"gopherd/internal/state/genesis"
)

// runTestnetBootstrap elects the genesis init witnesses so a multi-node testnet
// actually rotates producers.
//
// A fresh hived testnet creates its init_witnesses but gives them no votes, so
// the schedule contains only initminer and one node produces every block. That
// is useless for testing a mixed network. Governance weight comes from VESTS,
// and initminer holds the entire initial supply as LIQUID hive, so the
// bootstrap is: vest some of it, then vote for each witness.
//
// Every init witness shares the initminer key by construction (hived's
// init_witness registers them all with init_public_key), so one signature
// authorises the whole thing.
func runTestnetBootstrap(args []string) error {
	fs := flag.NewFlagSet("testnet-bootstrap", flag.ExitOnError)
	endpoint := fs.String("endpoint", "http://127.0.0.1:8751", "JSON-RPC endpoint of any node on the testnet")
	witnesses := fs.String("witnesses", "", "comma-separated witnesses to vote for (default: the --init-witness set you passed to the node)")
	vestAmount := fs.Int64("vest", 100_000_000, "HIVE (whole coins) for initminer to vest, buying governance weight")
	fs.Parse(args)

	names := []string{}
	for _, w := range strings.Split(*witnesses, ",") {
		if w = strings.TrimSpace(w); w != "" {
			names = append(names, w)
		}
	}
	if len(names) == 0 {
		return fmt.Errorf("testnet-bootstrap: --witnesses is required (e.g. --witnesses=wit-a,wit-b)")
	}

	priv, err := genesis.TestnetInitminerKey()
	if err != nil {
		return err
	}

	props, err := rpcCall(*endpoint, "database_api.get_dynamic_global_properties", map[string]any{})
	if err != nil {
		return fmt.Errorf("read global properties: %w", err)
	}
	headID, _ := props["head_block_id"].(string)
	if headID == "" {
		return fmt.Errorf("node returned no head_block_id")
	}
	refNum, refPrefix, err := taposFromBlockID(headID)
	if err != nil {
		return err
	}

	operations := []op.Operation{op.TransferToVesting{
		From:   auth.AccountName(genesis.MainnetInitminer),
		To:     auth.AccountName(genesis.MainnetInitminer),
		Amount: asset.Asset{Amount: *vestAmount * 1000, Symbol: asset.Symbol{Num: asset.HiveSymbol, WasLegacyEncoded: true}},
	}}
	for _, n := range names {
		operations = append(operations, op.AccountWitnessVote{
			Account: auth.AccountName(genesis.MainnetInitminer),
			Witness: auth.AccountName(n),
			Approve: true,
		})
	}
	ops := op.Ops(operations...)

	signed := tx.SignedTransaction{Transaction: tx.Transaction{
		RefBlockNum:    refNum,
		RefBlockPrefix: refPrefix,
		Expiration:     uint32(time.Now().Unix()) + 600,
		Operations:     ops,
	}}
	unsigned := tx.AppendTransactionBody(nil, &signed.Transaction)
	digest := crypto.SHA256Of2(network.ChainID[:], unsigned)
	sig, err := crypto.SignCompact(priv, digest)
	if err != nil {
		return fmt.Errorf("sign: %w", err)
	}
	signed.Signatures = append(signed.Signatures, sig)

	// Self-check: recover the key from our own signature. If this does not come
	// back as initminer, the fault is in how WE signed, not in how the node
	// re-serialises the JSON we send.
	recovered, rerr := crypto.RecoverPublicKey(digest, sig)
	if rerr != nil {
		return fmt.Errorf("self-check recover: %w", rerr)
	}
	if recovered != priv.PublicKey() {
		return fmt.Errorf("self-check: signature recovers to %s, want initminer %s",
			recovered.EncodeBase58(), priv.PublicKey().EncodeBase58())
	}
	fmt.Fprintf(os.Stderr, "testnet-bootstrap: signature self-check OK (%s)\n", recovered.EncodeBase58())

	// Both hived and gopherd take the transaction as a JSON object, not hex —
	// the signature is still computed over the BINARY form above.
	// Render from the SAME op structs that were signed, so the JSON can never
	// drift from the bytes the signature covers.
	jsonOps := make([]any, 0, len(ops))
	for _, o := range ops {
		jsonOps = append(jsonOps, api.OpToJSON(o, true))
	}
	trx := map[string]any{
		"ref_block_num":    signed.RefBlockNum,
		"ref_block_prefix": signed.RefBlockPrefix,
		"expiration":       time.Unix(int64(signed.Expiration), 0).UTC().Format("2006-01-02T15:04:05"),
		"operations":       jsonOps,
		"extensions":       []any{},
		"signatures":       []string{hex.EncodeToString(sig[:])},
	}
	if os.Getenv("GOPHERD_BOOTSTRAP_DEBUG") != "" {
		// Ask the node to re-serialise the JSON we are about to send. If its hex
		// differs from ours, the signature can never validate no matter how
		// correct the key is — and the diff names the offending field.
		fmt.Fprintf(os.Stderr, "our unsigned hex : %s\n", hex.EncodeToString(unsigned))
		if r, herr := rpcCall(*endpoint, "condenser_api.get_transaction_hex", []any{trx}); herr == nil {
			fmt.Fprintf(os.Stderr, "node hex         : %v\n", r)
		} else if raw, herr2 := rpcCallRaw(*endpoint, "condenser_api.get_transaction_hex", []any{trx}); herr2 == nil {
			fmt.Fprintf(os.Stderr, "node hex         : %s\n", truncate(raw, 400))
		} else {
			fmt.Fprintf(os.Stderr, "node hex         : (unavailable: %v)\n", herr)
		}
	}
	if _, err := rpcCall(*endpoint, "network_broadcast_api.broadcast_transaction",
		map[string]any{"trx": trx}); err != nil {
		return fmt.Errorf("broadcast: %w", err)
	}
	fmt.Fprintf(os.Stderr, "testnet-bootstrap: vested %d HIVE and voted for %v as initminer\n", *vestAmount, names)
	return nil
}

// taposFromBlockID derives the TaPOS reference from a block id, the same way
// hived does: the low 16 bits of the block number, and bytes 4..8 of the id as
// a little-endian prefix.
func taposFromBlockID(idHex string) (uint16, uint32, error) {
	b, err := hex.DecodeString(idHex)
	if err != nil || len(b) < 8 {
		return 0, 0, fmt.Errorf("bad head_block_id %q", idHex)
	}
	num := binary.BigEndian.Uint32(b[0:4])
	return uint16(num & 0xffff), binary.LittleEndian.Uint32(b[4:8]), nil
}

func rpcCall(endpoint, method string, params any) (map[string]any, error) {
	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "method": method, "params": params, "id": 1,
	})
	resp, err := http.Post(endpoint, "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out struct {
		Result map[string]any  `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decode %s: %w (%s)", method, err, truncate(string(raw), 200))
	}
	if len(out.Error) > 0 {
		return nil, fmt.Errorf("%s: %s", method, truncate(string(out.Error), 300))
	}
	return out.Result, nil
}

// rpcCallAny is rpcCall for methods whose result is not an object (condenser
// list methods return arrays).
func rpcCallAny(endpoint, method string, params any) (any, error) {
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": method, "params": params, "id": 1})
	resp, err := http.Post(endpoint, "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out struct {
		Result any             `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	if len(out.Error) > 0 {
		return nil, fmt.Errorf("%s: %s", method, truncate(string(out.Error), 200))
	}
	return out.Result, nil
}

func rpcCallRaw(endpoint, method string, params any) (string, error) {
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": method, "params": params, "id": 1})
	resp, err := http.Post(endpoint, "application/json", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return string(raw), nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

var _ = sha256.Sum256

// cmdTestnetBootstrap is the subcommand wrapper.
func cmdTestnetBootstrap(args []string) int {
	if err := runTestnetBootstrap(args); err != nil {
		fmt.Fprintf(os.Stderr, "gopherd testnet-bootstrap: %v\n", err)
		return 1
	}
	return 0
}
