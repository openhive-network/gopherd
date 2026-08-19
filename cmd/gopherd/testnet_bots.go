//go:build testnet

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/rand"
	"os"
	"strings"
	"sync"
	"time"

	"gopherd/internal/api"
	"gopherd/internal/crypto"
	"gopherd/internal/protocol/asset"
	"gopherd/internal/protocol/auth"
	"gopherd/internal/protocol/network"
	"gopherd/internal/protocol/op"
	"gopherd/internal/protocol/tx"
)

// Testnet load generator: a pool of bots that continuously submit RANDOM
// operations, so the chain sees traffic resembling real usage instead of the
// empty blocks a bare testnet produces.
//
// The point is not throughput, it is COVERAGE. Consensus bugs hide in
// operations nobody exercises; a testnet that only ever moves HIVE proves
// almost nothing about escrow, savings, proposals or the market. Each bot keeps
// a small model of what it has created (comments, orders, escrows, savings
// requests, proposals) so it can emit valid follow-up operations —
// limit_order_cancel needs an open order, escrow_approve needs an escrow.
//
// Testnet build only. It signs with keys it derives itself and would be
// meaningless (and rude) against mainnet.

const botFundHive = 50_000 // whole HIVE given to each bot at setup
const botFundHbd = 10_000  // whole HBD
const botVestHive = 10_000 // whole HIVE converted to VESTS

func hiveAmt(whole int64) asset.Asset {
	return asset.Asset{Amount: whole * 1000, Symbol: asset.Symbol{Num: asset.HiveSymbol, WasLegacyEncoded: true}}
}
func hbdAmt(whole int64) asset.Asset {
	return asset.Asset{Amount: whole * 1000, Symbol: asset.Symbol{Num: asset.HbdSymbol, WasLegacyEncoded: true}}
}
func hiveSat(sat int64) asset.Asset {
	return asset.Asset{Amount: sat, Symbol: asset.Symbol{Num: asset.HiveSymbol, WasLegacyEncoded: true}}
}
func hbdSat(sat int64) asset.Asset {
	return asset.Asset{Amount: sat, Symbol: asset.Symbol{Num: asset.HbdSymbol, WasLegacyEncoded: true}}
}
func vestSat(sat int64) asset.Asset {
	return asset.Asset{Amount: sat, Symbol: asset.Symbol{Num: asset.VestsSymbol, WasLegacyEncoded: true}}
}

type escrowRef struct {
	from, to, agent string
	id              uint32
}

type bot struct {
	name string
	priv *crypto.PrivateKey
	pub  crypto.PublicKey

	mu          sync.Mutex
	rng         *rand.Rand
	comments    []string
	orders      []uint32
	escrows     []escrowRef
	savingsReqs []uint32
	convertReqs []uint32
	proposals   []int64
	subAccounts []string
	tickets     int  // unclaimed account tickets from claim_account
	isWitness   bool // set once witness_update succeeds
	seq         uint32
	// pending holds state a generator wants recorded ONLY if the chain accepts
	// the transaction. Set by the generator, run by the caller on success.
	pending func()
}

func (b *bot) nextSeq() uint32 { b.seq++; return b.seq }

type opStat struct {
	sent, ok, failed int
	reasons          map[string]int
}

type world struct {
	endpoint  string
	bots      []*bot
	witnesses []string
	mu        sync.Mutex
	stats     map[string]*opStat

	taposMu   sync.Mutex
	refNum    uint16
	refPrefix uint32
	taposAt   time.Time
}

var debugOnce sync.Once

func (w *world) record(name string, err error) {
	if err != nil && os.Getenv("GOPHERD_BOTS_DEBUG") != "" {
		debugOnce.Do(func() {
			fmt.Fprintf(os.Stderr, "\n--- raw error for %s ---\n%s\n---\n", name, err.Error())
		})
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	st := w.stats[name]
	if st == nil {
		st = &opStat{reasons: map[string]int{}}
		w.stats[name] = st
	}
	st.sent++
	if err == nil {
		st.ok++
		return
	}
	st.failed++
	st.reasons[shortErr(err)]++
}

// shortErr collapses a node error to a stable bucket so the report groups by
// CAUSE rather than by whichever account or id happened to appear in the text.
//
// The node returns a JSON-RPC error object, so the useful text is nested inside
// it. Bucketing on the raw string put every failure under the METHOD NAME —
// 217 identical rows saying "broadcast_transaction", which is exactly the
// information already known. Pull the message out first.
func shortErr(err error) string {
	s := err.Error()
	if i := strings.Index(s, `"message":"`); i >= 0 {
		rest := s[i+len(`"message":"`):]
		if j := strings.Index(rest, `"`); j > 0 {
			s = rest[:j]
		}
	}
	// hived formats assertions as "Assert Exception:<predicate>: <explanation>".
	// Bucketing on the outer prefix collapses every distinct chain rule into one
	// meaningless row of "Assert Exception"; the PREDICATE names the exact check
	// and, unlike the explanation, carries no account names to fragment on.
	if rest, ok := strings.CutPrefix(s, "Assert Exception:"); ok {
		if j := strings.Index(rest, ": "); j > 0 {
			return strings.TrimSpace(rest[:j])
		}
		return strings.TrimSpace(rest)
	}
	for _, marker := range []string{
		"missing required active authority", "missing required posting authority",
		"missing required owner authority", "does not have sufficient funds",
		"Insufficient", "insufficient", "unknown key", "does not exist",
		"is not a witness", "Cannot vote", "duplicate", "expired",
		"tx_missing", "assert_exception", "plugin_exception",
	} {
		if strings.Contains(s, marker) {
			return marker
		}
	}
	// Strip trailing detail after a colon only when the prefix is meaningful.
	if i := strings.Index(s, ":"); i > 12 && i < 70 {
		return s[:i]
	}
	if len(s) > 60 {
		return s[:60]
	}
	return s
}

// tapos caches the reference block for a couple of seconds. Every transaction
// needs one, and re-reading global properties per transaction would make the
// generator measure the API rather than the chain.
func (w *world) tapos() (uint16, uint32, error) {
	w.taposMu.Lock()
	defer w.taposMu.Unlock()
	if time.Since(w.taposAt) < 2*time.Second && w.refPrefix != 0 {
		return w.refNum, w.refPrefix, nil
	}
	props, err := rpcCall(w.endpoint, "database_api.get_dynamic_global_properties", map[string]any{})
	if err != nil {
		return 0, 0, err
	}
	id, _ := props["head_block_id"].(string)
	n, p, err := taposFromBlockID(id)
	if err != nil {
		return 0, 0, err
	}
	w.refNum, w.refPrefix, w.taposAt = n, p, time.Now()
	return n, p, nil
}

// send signs ops with the bot's key and broadcasts them as one transaction.
func (w *world) send(priv *crypto.PrivateKey, ops []op.Op) error {
	refNum, refPrefix, err := w.tapos()
	if err != nil {
		return err
	}
	signed := tx.SignedTransaction{Transaction: tx.Transaction{
		RefBlockNum:    refNum,
		RefBlockPrefix: refPrefix,
		Expiration:     uint32(time.Now().Unix()) + 120,
		Operations:     ops,
	}}
	unsigned := tx.AppendTransactionBody(nil, &signed.Transaction)
	sig, err := crypto.SignCompact(priv, crypto.SHA256Of2(network.ChainID[:], unsigned))
	if err != nil {
		return err
	}
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
	_, err = rpcCall(w.endpoint, "network_broadcast_api.broadcast_transaction", map[string]any{"trx": trx})
	return err
}

// pendingRewards reports an account's unclaimed reward balances in satoshi.
// claim_reward_balance asserts at least one amount is non-zero, so a generator
// that guesses gets refused every single time.
func (w *world) pendingRewards(name string) (hive, hbd, vests int64) {
	res, err := rpcCallAny(w.endpoint, "condenser_api.get_accounts", []any{[]string{name}})
	if err != nil {
		return 0, 0, 0
	}
	arr, ok := res.([]any)
	if !ok || len(arr) == 0 {
		return 0, 0, 0
	}
	acc, _ := arr[0].(map[string]any)
	return legacySat(acc["reward_hive_balance"]), legacySat(acc["reward_hbd_balance"]), legacySat(acc["reward_vesting_balance"])
}

// legacySat parses a legacy asset string ("1.234 TESTS") into satoshi. The
// condenser API renders assets as strings, so the amount has to be recovered
// from the decimal text rather than read off a field.
func legacySat(v any) int64 {
	s, ok := v.(string)
	if !ok {
		return 0
	}
	parts := strings.Fields(s)
	if len(parts) == 0 {
		return 0
	}
	num := parts[0]
	dot := strings.Index(num, ".")
	digits := strings.Replace(num, ".", "", 1)
	var out int64
	for _, c := range digits {
		if c < '0' || c > '9' {
			return 0
		}
		out = out*10 + int64(c-'0')
	}
	_ = dot
	return out
}

func botKey(seed string, i int) (*crypto.PrivateKey, error) {
	h := sha256.Sum256([]byte(fmt.Sprintf("gopherd-testnet-bot:%s:%d", seed, i)))
	return crypto.PrivateKeyFromBytes(h[:])
}

func authOf(k crypto.PublicKey) auth.Authority {
	return auth.Authority{WeightThreshold: 1, KeyAuths: []auth.KeyAuth{{Key: k, Weight: 1}}}
}

func (b *bot) pick(list []string) string {
	if len(list) == 0 {
		return ""
	}
	return list[b.rng.Intn(len(list))]
}

func (w *world) otherBot(self *bot, rng *rand.Rand) *bot {
	if len(w.bots) < 2 {
		return nil
	}
	for i := 0; i < 8; i++ {
		c := w.bots[rng.Intn(len(w.bots))]
		if c.name != self.name {
			return c
		}
	}
	return nil
}
