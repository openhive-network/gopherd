//go:build testnet

package main

import (
	"flag"
	"fmt"
	"math/rand"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"gopherd/internal/protocol/auth"
	"gopherd/internal/protocol/op"
	"gopherd/internal/state/genesis"
)

func cmdTestnetBots(args []string) int {
	if err := runTestnetBots(args); err != nil {
		fmt.Fprintf(os.Stderr, "gopherd testnet-bots: %v\n", err)
		return 1
	}
	return 0
}

func runTestnetBots(args []string) error {
	fs := flag.NewFlagSet("testnet-bots", flag.ExitOnError)
	endpoint := fs.String("endpoint", "http://127.0.0.1:8791", "JSON-RPC endpoint to submit through")
	count := fs.Int("bots", 6, "number of bot accounts")
	duration := fs.Duration("duration", 3*time.Minute, "how long to generate traffic")
	tps := fs.Float64("tps", 6, "target transactions per second across all bots")
	seed := fs.String("seed", "default", "seed for deterministic bot keys and op choices")
	setupOnly := fs.Bool("setup-only", false, "create and fund the accounts, then stop")
	witnesses := fs.String("witnesses", "initminer,wit-a,wit-b,wit-c,wit-d", "witnesses bots may vote for")
	onlyOps := fs.String("only-ops", "", "restrict traffic to these operations (comma-separated). Essential for RC parity work: a mixed workload cannot be compared per-transaction against hived because the cost depends on which operations ran")
	fs.Parse(args)

	if *count < 2 {
		return fmt.Errorf("need at least 2 bots so they have someone to transact with")
	}

	w := &world{endpoint: *endpoint, stats: map[string]*opStat{}}
	for _, s := range strings.Split(*witnesses, ",") {
		if s = strings.TrimSpace(s); s != "" {
			w.witnesses = append(w.witnesses, s)
		}
	}

	// Deterministic identities: same seed, same accounts and same key material,
	// so a run that trips a consensus bug can be replayed exactly.
	for i := 0; i < *count; i++ {
		priv, err := botKey(*seed, i)
		if err != nil {
			return err
		}
		w.bots = append(w.bots, &bot{
			name: fmt.Sprintf("bot-%s-%d", *seed, i),
			priv: priv, pub: priv.PublicKey(),
			rng: rand.New(rand.NewSource(int64(i)*7919 + 13)),
		})
	}

	fmt.Fprintf(os.Stderr, "testnet-bots: creating and funding %d accounts...\n", len(w.bots))
	if err := setupBots(w); err != nil {
		return fmt.Errorf("setup: %w", err)
	}
	if *setupOnly {
		fmt.Fprintln(os.Stderr, "testnet-bots: setup complete")
		return nil
	}

	active := generators
	if *onlyOps != "" {
		want := map[string]bool{}
		for _, n := range strings.Split(*onlyOps, ",") {
			if n = strings.TrimSpace(n); n != "" {
				want[n] = true
			}
		}
		active = nil
		for _, g := range generators {
			if want[g.name] {
				active = append(active, g)
			}
		}
		if len(active) == 0 {
			return fmt.Errorf("--only-ops matched no generators")
		}
	}
	totalWeight := 0
	for _, g := range active {
		totalWeight += g.weight
	}

	fmt.Fprintf(os.Stderr, "testnet-bots: generating traffic for %s at ~%.1f tx/s across %d ops...\n",
		*duration, *tps, len(active))

	// One ticker shared by all bots enforces the aggregate rate; each tick is
	// handed to a random bot. Rate-limiting per bot instead would make the
	// total depend on bot count.
	interval := time.Duration(float64(time.Second) / *tps)
	if interval <= 0 {
		interval = time.Millisecond
	}
	tick := time.NewTicker(interval)
	defer tick.Stop()
	deadline := time.Now().Add(*duration)

	var wg sync.WaitGroup
	sem := make(chan struct{}, 8) // bound concurrent RPCs
	pickRng := rand.New(rand.NewSource(99))
	skipped := 0
	for time.Now().Before(deadline) {
		<-tick.C
		b := w.bots[pickRng.Intn(len(w.bots))]
		g := pickGenerator(active, pickRng, totalWeight)

		b.mu.Lock()
		b.pending = nil
		ops := g.gen(b, w, b.rng)
		commit := b.pending
		b.pending = nil
		b.mu.Unlock()
		if len(ops) == 0 {
			skipped++
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(name string, b *bot, operations []op.Operation, commit func()) {
			defer wg.Done()
			defer func() { <-sem }()
			err := w.send(b.priv, op.Ops(operations...))
			if err == nil && commit != nil {
				// Only now does the state the generator created actually exist
				// on chain, so only now may other generators reference it.
				b.mu.Lock()
				commit()
				b.mu.Unlock()
			}
			w.record(name, err)
		}(g.name, b, ops, commit)
	}
	wg.Wait()

	report(w, skipped, len(active))
	return nil
}

func pickGenerator(gens []generator, r *rand.Rand, total int) generator {
	n := r.Intn(total)
	for _, g := range gens {
		if n < g.weight {
			return g
		}
		n -= g.weight
	}
	return gens[0]
}

// setupBots creates every bot account from initminer and funds it with HIVE,
// HBD and VESTS. Without VESTS a bot has no mana and every operation it submits
// is refused, so vesting is part of setup rather than something to discover
// later from a wall of RC errors.
func setupBots(w *world) error {
	priv, err := genesis.TestnetInitminerKey()
	if err != nil {
		return err
	}
	for _, b := range w.bots {
		a := authOf(b.pub)
		create := []op.Operation{op.AccountCreate{
			Fee: hiveSat(0), Creator: acct(genesis.MainnetInitminer), NewAccountName: acct(b.name),
			Owner: a, Active: a, Posting: a, MemoKey: b.pub,
		}}
		if err := w.send(priv, op.Ops(create...)); err != nil {
			// Already created by an earlier run is fine; anything else is not.
			if !strings.Contains(err.Error(), "already exists") {
				return fmt.Errorf("create %s: %w", b.name, err)
			}
		}
	}
	// Wait for the creations to be included before funding: a transfer to an
	// account that is still only in the mempool fails.
	time.Sleep(6 * time.Second)

	for _, b := range w.bots {
		fund := []op.Operation{
			op.Transfer{From: acct(genesis.MainnetInitminer), To: acct(b.name), Amount: hiveAmt(botFundHive), Memo: "fund"},
			op.Transfer{From: acct(genesis.MainnetInitminer), To: acct(b.name), Amount: hbdAmt(botFundHbd), Memo: "fund"},
			op.TransferToVesting{From: acct(genesis.MainnetInitminer), To: acct(b.name), Amount: hiveAmt(botVestHive)},
		}
		if err := w.send(priv, op.Ops(fund...)); err != nil {
			return fmt.Errorf("fund %s: %w", b.name, err)
		}
	}
	time.Sleep(6 * time.Second)
	return nil
}

func report(w *world, skipped, nGen int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	names := make([]string, 0, len(w.stats))
	for n := range w.stats {
		names = append(names, n)
	}
	sort.Strings(names)

	var sent, ok, failed int
	fmt.Fprintf(os.Stderr, "\n%-32s %7s %7s %7s\n", "operation", "sent", "ok", "failed")
	fmt.Fprintf(os.Stderr, "%s\n", strings.Repeat("-", 58))
	for _, n := range names {
		st := w.stats[n]
		sent += st.sent
		ok += st.ok
		failed += st.failed
		// Show each op's DOMINANT failure reason inline. An aggregate reason
		// histogram says "27 missing active authority" without saying which
		// operations produced them, which is the only part that leads anywhere.
		top, topN := "", 0
		for r, c := range st.reasons {
			if c > topN {
				top, topN = r, c
			}
		}
		note := ""
		if topN > 0 {
			note = fmt.Sprintf("  %s (%d)", top, topN)
		}
		fmt.Fprintf(os.Stderr, "%-32s %7d %7d %7d%s\n", n, st.sent, st.ok, st.failed, note)
	}
	fmt.Fprintf(os.Stderr, "%s\n%-32s %7d %7d %7d\n", strings.Repeat("-", 58), "TOTAL", sent, ok, failed)
	fmt.Fprintf(os.Stderr, "\ncovered %d/%d operation types; %d attempts skipped (preconditions unmet)\n",
		len(names), nGen, skipped)

	// Failure reasons matter more than the counts: a rejection is only
	// interesting if it is not one the chain is supposed to produce.
	agg := map[string]int{}
	for _, st := range w.stats {
		for r, c := range st.reasons {
			agg[r] += c
		}
	}
	if len(agg) > 0 {
		type kv struct {
			r string
			c int
		}
		all := make([]kv, 0, len(agg))
		for r, c := range agg {
			all = append(all, kv{r, c})
		}
		sort.Slice(all, func(i, j int) bool { return all[i].c > all[j].c })
		fmt.Fprintln(os.Stderr, "\nfailure reasons:")
		for i, e := range all {
			if i >= 12 {
				break
			}
			fmt.Fprintf(os.Stderr, "  %6d  %s\n", e.c, e.r)
		}
	}
}

var _ = auth.AccountName("")
