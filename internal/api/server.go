// Package api implements a hived-compatible JSON-RPC API over gopherd state.
// Read-only. Methods query committed state via chain.Database.WithReadSession
// (which serialises against the single ApplyBlock writer via applyMu).
package api

import (
	"encoding/hex"
	"math/big"
	"strconv"
	"time"

	"hived-go/internal/api/jsonrpc"
	"hived-go/internal/chain"
	"hived-go/internal/protocol/asset"
	"hived-go/internal/protocol/network"
)

// Server exposes gopherd chain state over a JSON-RPC registry.
type Server struct {
	chain   *chain.Database
	reg     *jsonrpc.Registry
	mempool *Mempool
}

// New builds a Server and registers every implemented API method.
func New(c *chain.Database) *Server {
	s := &Server{chain: c, reg: jsonrpc.NewRegistry(), mempool: newMempool()}
	s.registerDatabaseAPI()
	s.registerBlockAPI()
	s.registerCondenserAPI()
	s.registerListsAPI()
	s.registerWitnessListAPI()
	s.registerAccountByKeyAPI()
	s.registerTxAPI()
	s.registerAccountHistoryAPI()
	return s
}

// Registry returns the underlying method registry (for HTTP/WS serving).
func (s *Server) Registry() *jsonrpc.Registry { return s.reg }

// --- shared formatting helpers (match hived's to_variant output) ------------

// hiveAsset/hbdAsset/vestsAsset wrap an int64 satoshi amount in an asset.Asset,
// which marshals as the HF26 NAI object — the database_api form. condenser
// methods wrap these in legacyAsset for the "1.234 HIVE" string form.
func hiveAsset(a int64) asset.Asset  { return asset.Asset{Amount: a, Symbol: asset.Symbol{Num: asset.HiveSymbol}} }
func hbdAsset(a int64) asset.Asset   { return asset.Asset{Amount: a, Symbol: asset.Symbol{Num: asset.HbdSymbol}} }
func vestsAsset(a int64) asset.Asset { return asset.Asset{Amount: a, Symbol: asset.Symbol{Num: asset.VestsSymbol}} }

// legacyAsset marshals as the legacy "1.234 HIVE" string (condenser_api form).
type legacyAsset asset.Asset

func (l legacyAsset) MarshalJSON() ([]byte, error) {
	return []byte(`"` + asset.Asset(l).String() + `"`), nil
}

// timeString formats a time_point_sec as hived's ISO-8601 "2006-01-02T15:04:05".
func timeString(sec uint32) string {
	return time.Unix(int64(sec), 0).UTC().Format("2006-01-02T15:04:05")
}

// hexID renders a 20-byte block_id as lowercase hex.
func hexID(id [20]byte) string { return hex.EncodeToString(id[:]) }

// parseTimeString inverts timeString: "2006-01-02T15:04:05" → time_point_sec.
func parseTimeString(s string) (uint32, error) {
	t, err := time.Parse("2006-01-02T15:04:05", s)
	if err != nil {
		return 0, err
	}
	return uint32(t.Unix()), nil
}

// u128dec renders a 16-byte big-endian uint128 (e.g. total_reward_shares2) as a
// decimal string.
func u128dec(b [16]byte) string { return new(big.Int).SetBytes(b[:]).String() }

func itoa(v uint32) string  { return strconv.FormatUint(uint64(v), 10) }
func itoa64(v int64) string { return strconv.FormatInt(v, 10) }

// networkChainIDHex returns the active build's chain_id in hex.
func networkChainIDHex() string { return network.ChainID.Hex() }

// versionString renders a packed hived version_type (low 3 bytes = a.b.c) as
// "a.b.c".
func versionString(v uint32) string {
	return itoa((v>>16)&0xff) + "." + itoa((v>>8)&0xff) + "." + itoa(v&0xff)
}
