package main

// govsweepscan answers one question, from the block log alone: has a
// governance-expiry sweep ever had to remove more than
// HIVE_GLOBAL_REMOVE_THRESHOLD proposal votes in a single block?
//
// Why it matters. hived's remove_expired_governance_votes opens a
// remove_guard( 200 ) and, when an account's proposal-vote removal exhausts
// it, processes NOTHING further for that account and BREAKS — deferring it and
// every later one to the next block. gopherd clears every due account at once.
// If the budget is ever reached the two disagree about which block an
// account's witness votes were cleared in, which moves vote totals and
// therefore the schedule.
//
// The port is blocked on account ids (hived iterates
// by_governance_vote_expiration_ts, tie-broken by account_id, and gopherd has
// none), so before doing that work it is worth knowing whether the budget is
// reachable at all. chain.GovernanceRemovalStats() answers the same question
// during a replay; this answers it in minutes instead of hours, because it
// needs no state at all.
//
// What it models. The budget counts PROPOSAL-vote removals, so an account
// holding none contributes nothing — which bounds the whole problem to
// accounts that appear in an update_proposal_votes op, i.e. to blocks from
// HF21 onward. For those accounts it tracks:
//
//   - the vote set, from update_proposal_votes (approve adds, unapprove
//     removes) and remove_proposal (which cascades to every vote on the
//     proposals it erases), and
//   - the governance-expiration stamp, recomputed on every
//     account_witness_vote / account_witness_proxy / update_proposal_votes
//     exactly as account_object::update_governance_vote_expiration_ts does.
//
// Expirations are then bucketed by three-second slot. Blocks are three seconds
// apart, so every account expiring inside one slot is swept by the same block;
// the largest per-slot total is the largest per-block removal count.
//
// Where it deliberately OVER-counts, and why that is the right direction. Two
// other paths also erase proposal votes, and neither is modelled:
// process_decline_voting_rights (HF28+), and the one-shot sweep over accounts
// without voting rights at the HF28 boundary. An account cleared by either
// keeps its votes here, so its expiry slot is credited with removals that
// would already have happened. That inflates the count, which can only turn a
// clean chain into a reported divergence — never the reverse. So a "no slot
// reaches the budget" verdict is sound as it stands, while an over-budget
// verdict is a prompt to check whether the accounts in that slot had declined
// voting rights first.
//
// Usage: gopherd govsweep-scan <block_log> <from_block> <to_block> [events.gob]
//
// Extraction costs ~80 minutes over the full range and the simulation costs
// seconds, so the optional fourth argument writes the extracted events out.
// Re-running the analysis against a dumped file takes no I/O at all — which
// matters because the simulation is the part that gets revised as more of
// hived's behaviour is modelled, and re-reading 380GB to change one line of
// arithmetic is a bad trade. Pass the .gob path in place of the block log to
// re-simulate.

import (
	"encoding/gob"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"gopherd/internal/blocklog"
	"gopherd/internal/fcraw"
	"gopherd/internal/protocol/block"
	"gopherd/internal/protocol/hardfork"
	"gopherd/internal/protocol/network"
	"gopherd/internal/protocol/op"
	"gopherd/internal/protocol/tx"
)

// Mirrors internal/chain/evaluator/witness.go's
// update_governance_vote_expiration_ts.
//
// govFirstExpireTimestamp is DERIVED, not written down. It was a literal here
// first, and the literal was wrong by 7200 seconds — which translates every
// pre-HF25 account's expiry by 2400 slots and quietly changes where clumps
// land relative to post-HF25 accounts. Deriving it from the same hardfork
// table the evaluator uses is the only version that cannot drift.
const (
	govVoteExpirationPeriodSec     uint32 = 365 * 24 * 60 * 60
	govMaxOldExpireShiftSec        uint32 = govVoteExpirationPeriodSec / 2
	govBlockIntervalSec            uint32 = 3
	govHF21ProposalsActivationHint uint32 = 35_921_786
)

// govFirstExpireTimestamp is HARDFORK_1_25_FIRST_GOVERNANCE_VOTE_EXPIRE_TIMESTAMP:
// the earliest expiration anyone can hold post-HF25, and the anchor the
// pre-HF25 redistribution hangs off. Taken from the shared hardfork table, not
// copied.
var govFirstExpireTimestamp = hardfork.MainnetActivationTime[hardfork.HF25] + govVoteExpirationPeriodSec

func govExpirationFor(voteTime uint32) uint32 {
	e := voteTime + govVoteExpirationPeriodSec
	if e < govFirstExpireTimestamp {
		e = govFirstExpireTimestamp + (e % govMaxOldExpireShiftSec)
	}
	return e
}

// govEventKind distinguishes the four things that move the state this scan
// tracks.
type govEventKind uint8

const (
	govEvtGovernanceAction govEventKind = iota // restamps the expiration
	govEvtProposalVote                         // also adds or removes a vote
	govEvtProposalRemoved                      // cascades to every vote on it
)

type govEvent struct {
	block    uint32
	ts       uint32
	kind     govEventKind
	account  string
	proposal int64
	approve  bool
}

func govScanShard(path string, from, to uint32, worker int, progress func(worker int, done, total uint32)) []govEvent {
	r, err := blocklog.Open(path)
	if err != nil {
		panic(err)
	}
	defer r.Close()

	total := to - from + 1
	// The full range is ~80 minutes of block-log reading. Without this the run
	// prints nothing at all until every worker finishes, which is
	// indistinguishable from a hang.
	//
	// Small enough that the FIRST line appears within a minute: at ~1.2k
	// blocks/s per worker, a 500k stride would have held the first report back
	// six minutes, which is most of the way to "is this thing alive?" again.
	// The 15s throttle in the aggregator is what keeps the volume down, so
	// there is no reason for the stride to do it as well.
	const progressEvery = 50_000

	var out []govEvent
	// Evaluators stamp with head_block_time(), which during transaction
	// application is the PREVIOUS block's timestamp — dgpo.time advances after
	// the tx loop. Using the action block's own timestamp puts every stamp 3s
	// late, which is exactly one block of skew in the answer.
	var prevTs uint32
	if from > 1 {
		if praw, err := r.Block(from - 1); err == nil {
			if phdr, _, err := block.UnmarshalSignedHeader(praw); err == nil {
				prevTs = phdr.Timestamp
			}
		}
	}

	for n := from; n <= to; n++ {
		if progress != nil && (n-from)%progressEvery == 0 && n != from {
			progress(worker, n-from, total)
		}
		raw, err := r.Block(n)
		if err != nil {
			continue
		}
		hdr, hdrLen, err := block.UnmarshalSignedHeader(raw)
		if err != nil {
			continue
		}
		headTime := prevTs
		prevTs = hdr.Timestamp

		rd := fcraw.NewReader(raw[hdrLen:])
		txCount, _ := rd.ReadVarUint()
		for i := uint32(0); i < txCount; i++ {
			txRes, err := tx.ReadSignedTransaction(rd)
			if err != nil {
				break
			}
			for _, o := range txRes.SignedTx.Operations {
				switch v := o.Operation().(type) {
				case op.AccountWitnessVote:
					out = append(out, govEvent{n, headTime, govEvtGovernanceAction, string(v.Account), 0, false})
				case op.AccountWitnessProxy:
					out = append(out, govEvent{n, headTime, govEvtGovernanceAction, string(v.Account), 0, false})
				case op.UpdateProposalVotes:
					for _, pid := range v.ProposalIDs {
						out = append(out, govEvent{n, headTime, govEvtProposalVote, string(v.Voter), pid, v.Approve})
					}
				case op.RemoveProposal:
					for _, pid := range v.ProposalIDs {
						out = append(out, govEvent{n, headTime, govEvtProposalRemoved, "", pid, false})
					}
				}
			}
		}
	}
	return out
}

type govSweepResult struct {
	maxPerSlot  int
	maxSlotTime uint32
	maxAccounts int
	overBudget  int
	slotsSeen   int
	votersSeen  int
	// Over-budget slots, split by whether the scanned range had already
	// reached them. An expiry is (last governance action + one year), so a
	// recent action produces an expiry up to a year beyond the chain's head:
	// the sweep for it has NOT happened yet. A clean replay to head is
	// therefore entirely consistent with slots that will blow the budget
	// later, and conflating the two turns "will diverge" into the much
	// stronger and wrong "has diverged".
	offenders []govOffender
	lastTs    uint32
	// maxPerWindow is the largest total across any run of govMergeWindowSlots
	// CONSECUTIVE slots. Slots are only one block each while the chain never
	// misses one; every missed block makes the next block cover the skipped
	// slot too, so its sweep faces both slots' accounts at once. A verdict
	// that only looked at single slots would under-count exactly when the
	// chain is unhealthy, which is when a clump is most likely.
	maxPerWindow     int
	maxWindowEndTime uint32
	overBudgetWindow int
}

// govOffender is one slot whose sweep would exceed the budget.
type govOffender struct {
	slotTime  uint32
	votes     int
	accounts  int
	inThePast bool // the scanned range reached this slot
	worst     string
	worstN    int
}

// govMergeWindowSlots is how many consecutive slots one block is assumed to be
// able to absorb — 20 slots is a full minute of missed blocks, far beyond any
// normal run of misses. If even that window stays under the budget, no
// realistic outage can push a sweep over it.
const govMergeWindowSlots = 20

// govSimulate replays the events in block order and reports the largest number
// of proposal votes a single sweep would have had to remove.
func govSimulate(events []govEvent, limit int) govSweepResult {
	// votes[account] = set of proposal ids
	votes := make(map[string]map[int64]struct{}, 1<<16)
	// expiry[account] = governance vote expiration timestamp
	expiry := make(map[string]uint32, 1<<16)

	for _, e := range events {
		switch e.kind {
		case govEvtGovernanceAction:
			expiry[e.account] = govExpirationFor(e.ts)
		case govEvtProposalVote:
			expiry[e.account] = govExpirationFor(e.ts)
			set := votes[e.account]
			if set == nil {
				set = make(map[int64]struct{}, 4)
				votes[e.account] = set
			}
			if e.approve {
				set[e.proposal] = struct{}{}
			} else {
				delete(set, e.proposal)
			}
		case govEvtProposalRemoved:
			// Removing a proposal erases every vote cast on it, so those
			// votes are no longer the expiry sweep's to remove.
			for _, set := range votes {
				delete(set, e.proposal)
			}
		}
	}

	// Bucket the surviving accounts by the block slot their expiration falls
	// in. Blocks are 3s apart, so one slot is one block's worth of sweep.
	type bucket struct {
		voteCount int
		accounts  int
		// The single biggest holder in the slot. When one account alone
		// exceeds the budget, the divergence needs no clump at all, and
		// naming it is what makes the finding actionable.
		worst  string
		worstN int
	}
	slots := make(map[uint32]*bucket, 1<<16)
	voters := 0
	for account, set := range votes {
		if len(set) == 0 {
			continue
		}
		voters++
		exp, ok := expiry[account]
		if !ok {
			continue
		}
		slot := exp / govBlockIntervalSec
		b := slots[slot]
		if b == nil {
			b = &bucket{}
			slots[slot] = b
		}
		b.voteCount += len(set)
		b.accounts++
		if len(set) > b.worstN {
			b.worstN = len(set)
			b.worst = account
		}
	}

	// The latest action timestamp the scan saw stands in for where the range
	// ends in chain time.
	var lastTs uint32
	for _, e := range events {
		if e.ts > lastTs {
			lastTs = e.ts
		}
	}

	res := govSweepResult{slotsSeen: len(slots), votersSeen: voters, lastTs: lastTs}
	ordered := make([]uint32, 0, len(slots))
	for slot, b := range slots {
		if b.voteCount > limit {
			res.overBudget++
			res.offenders = append(res.offenders, govOffender{
				slotTime:  slot * govBlockIntervalSec,
				votes:     b.voteCount,
				accounts:  b.accounts,
				inThePast: slot*govBlockIntervalSec <= lastTs,
				worst:     b.worst,
				worstN:    b.worstN,
			})
		}
		if b.voteCount > res.maxPerSlot {
			res.maxPerSlot = b.voteCount
			res.maxSlotTime = slot * govBlockIntervalSec
			res.maxAccounts = b.accounts
		}
		ordered = append(ordered, slot)
	}

	// Sliding window over CONSECUTIVE slot numbers, so a run of missed blocks
	// merging several slots into one sweep is accounted for. Gaps in the slot
	// numbering are real gaps in time, not adjacency, so the window is keyed
	// on slot number rather than on position in this list.
	sort.Slice(ordered, func(i, j int) bool { return ordered[i] < ordered[j] })
	lo := 0
	running := 0
	for hi := 0; hi < len(ordered); hi++ {
		running += slots[ordered[hi]].voteCount
		for ordered[hi]-ordered[lo] >= govMergeWindowSlots {
			running -= slots[ordered[lo]].voteCount
			lo++
		}
		if running > limit {
			res.overBudgetWindow++
		}
		if running > res.maxPerWindow {
			res.maxPerWindow = running
			res.maxWindowEndTime = ordered[hi] * govBlockIntervalSec
		}
	}
	return res
}

func cmdGovSweepScan(args []string) int {
	// Re-simulate from a previously dumped event file: one argument, and it
	// ends in .gob.
	if len(args) == 1 && strings.HasSuffix(args[0], ".gob") {
		events, from, to, err := govLoadEvents(args[0])
		if err != nil {
			fmt.Fprintf(os.Stderr, "govsweep-scan: %v\n", err)
			return 1
		}
		fmt.Fprintf(os.Stderr, "loaded %d events from %s\n", len(events), args[0])
		return govReport(events, from, to)
	}
	if len(args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: gopherd govsweep-scan <block_log> <from_block> <to_block> [events.gob]")
		fmt.Fprintln(os.Stderr, "       gopherd govsweep-scan <events.gob>")
		return 2
	}
	path := args[0]
	fromN, err := strconv.ParseUint(args[1], 10, 32)
	if err != nil {
		fmt.Fprintf(os.Stderr, "bad from_block: %v\n", err)
		return 2
	}
	toN, err := strconv.ParseUint(args[2], 10, 32)
	if err != nil {
		fmt.Fprintf(os.Stderr, "bad to_block: %v\n", err)
		return 2
	}
	from, to := uint32(fromN), uint32(toN)
	if from < 1 {
		from = 1
	}
	if to < from {
		fmt.Fprintln(os.Stderr, "to_block must be >= from_block")
		return 2
	}
	if from > govHF21ProposalsActivationHint {
		fmt.Fprintf(os.Stderr,
			"note: from_block %d is past HF21 (%d); proposal votes cast before it are invisible to this scan\n",
			from, govHF21ProposalsActivationHint)
	}

	// More shards than cores on purpose. A shard is only checkpointed when it
	// FINISHES, so shard size sets how much work an interrupt can destroy:
	// with 12 shards over 72M blocks the first checkpoint lands more than an
	// hour in, which is exactly when the first full run died having written
	// nothing. Smaller shards checkpoint sooner and more often, and since
	// workers are I/O bound the extra concurrency costs nothing.
	workers := 48
	if n := os.Getenv("GOPHERD_GOVSWEEP_SHARDS"); n != "" {
		if v, err := strconv.Atoi(n); err == nil && v > 0 {
			workers = v
		}
	}

	// Where per-shard checkpoints live. Derived from the output path so a run
	// that names an output file gets resumability for free.
	ckpt := ""
	if len(args) >= 4 {
		ckpt = args[3]
	}

	// One line per 500k blocks per worker is too noisy; aggregate instead and
	// print a single overall percentage at a steady cadence.
	var progMu sync.Mutex
	perWorker := make([]uint32, workers)
	start := time.Now()
	lastPrint := start
	reportProgress := func(w int, done, wtotal uint32) {
		progMu.Lock()
		defer progMu.Unlock()
		perWorker[w] = done
		if time.Since(lastPrint) < 15*time.Second {
			return
		}
		lastPrint = time.Now()
		var sum uint32
		for _, v := range perWorker {
			sum += v
		}
		span := to - from + 1
		pct := float64(sum) / float64(span) * 100
		elapsed := time.Since(start)
		eta := "?"
		if pct > 0.5 {
			eta = time.Duration(float64(elapsed) * (100 - pct) / pct).Round(time.Second).String()
		}
		fmt.Fprintf(os.Stderr, "scanned %d/%d blocks (%.1f%%) elapsed=%s eta=%s\n",
			sum, span, pct, elapsed.Round(time.Second), eta)
	}

	span := to - from + 1
	shard := span / uint32(workers)
	if shard == 0 {
		shard = span
	}
	shards := make([][]govEvent, workers)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		lo := from + uint32(w)*shard
		hi := lo + shard - 1
		if w == workers-1 || hi > to {
			hi = to
		}
		if lo > to {
			continue
		}
		wg.Add(1)
		go func(w int, lo, hi uint32) {
			defer wg.Done()
			// Resume: a shard already on disk for this exact range is reused.
			// The first full run died at 98% with ten of twelve workers
			// finished and nothing written, because the dump only happened
			// after the merge. Eighty minutes of reading should not be
			// forfeit because the last worker was interrupted.
			if ckpt != "" {
				if ev, ok := govLoadShard(ckpt, w, lo, hi); ok {
					shards[w] = ev
					progMu.Lock()
					perWorker[w] = hi - lo + 1
					progMu.Unlock()
					fmt.Fprintf(os.Stderr, "worker %d [%d..%d] RESUMED events=%d\n", w, lo, hi, len(ev))
					return
				}
			}
			shards[w] = govScanShard(path, lo, hi, w, reportProgress)
			if ckpt != "" {
				if err := govSaveShard(ckpt, w, lo, hi, shards[w]); err != nil {
					fmt.Fprintf(os.Stderr, "worker %d: could not checkpoint: %v\n", w, err)
				}
			}
			fmt.Fprintf(os.Stderr, "worker %d [%d..%d] DONE events=%d\n", w, lo, hi, len(shards[w]))
		}(w, lo, hi)
	}
	wg.Wait()

	var events []govEvent
	for _, sh := range shards {
		events = append(events, sh...)
	}
	// Shards are contiguous and already in order internally, but sort anyway:
	// a shard boundary landing mid-block would otherwise reorder two events in
	// the same block, and the vote set is order-sensitive.
	sort.SliceStable(events, func(i, j int) bool { return events[i].block < events[j].block })

	if len(args) >= 4 {
		if err := govSaveEvents(args[3], events, from, to); err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not write %s: %v\n", args[3], err)
		} else {
			fmt.Fprintf(os.Stderr, "wrote %d events to %s\n", len(events), args[3])
		}
	}
	return govReport(events, from, to)
}

// govEventFile is the dumped form. Exported fields because gob ignores
// unexported ones, which would silently write empty events.
type govEventFile struct {
	From, To uint32
	Events   []govEventDump
}

type govEventDump struct {
	Block, Ts uint32
	Kind      uint8
	Account   string
	Proposal  int64
	Approve   bool
}

// govShardPath names one worker's checkpoint. The range is in the name so a
// re-run with different bounds cannot silently reuse the wrong events.
func govShardPath(base string, w int, lo, hi uint32) string {
	return fmt.Sprintf("%s.shard%02d-%d-%d", base, w, lo, hi)
}

func govSaveShard(base string, w int, lo, hi uint32, events []govEvent) error {
	// Write-then-rename: a checkpoint interrupted mid-write would otherwise be
	// resumed as a truncated shard, which is worse than no shard at all —
	// silently missing events look exactly like a chain with fewer votes.
	tmp := govShardPath(base, w, lo, hi) + ".tmp"
	if err := govSaveEvents(tmp, events, lo, hi); err != nil {
		return err
	}
	return os.Rename(tmp, govShardPath(base, w, lo, hi))
}

func govLoadShard(base string, w int, lo, hi uint32) ([]govEvent, bool) {
	ev, from, to, err := govLoadEvents(govShardPath(base, w, lo, hi))
	if err != nil || from != lo || to != hi {
		return nil, false
	}
	return ev, true
}

func govSaveEvents(path string, events []govEvent, from, to uint32) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	out := govEventFile{From: from, To: to, Events: make([]govEventDump, len(events))}
	for i, e := range events {
		out.Events[i] = govEventDump{e.block, e.ts, uint8(e.kind), e.account, e.proposal, e.approve}
	}
	return gob.NewEncoder(f).Encode(&out)
}

func govLoadEvents(path string) ([]govEvent, uint32, uint32, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, 0, err
	}
	defer f.Close()
	var in govEventFile
	if err := gob.NewDecoder(f).Decode(&in); err != nil {
		return nil, 0, 0, err
	}
	events := make([]govEvent, len(in.Events))
	for i, e := range in.Events {
		events[i] = govEvent{e.Block, e.Ts, govEventKind(e.Kind), e.Account, e.Proposal, e.Approve}
	}
	return events, in.From, in.To, nil
}

func govReport(events []govEvent, from, to uint32) int {
	limit := int(network.GlobalRemoveThreshold)
	res := govSimulate(events, limit)

	fmt.Printf("\n--- governance sweep budget scan ---\n")
	fmt.Printf("range:            %d..%d\n", from, to)
	fmt.Printf("events:           %d\n", len(events))
	fmt.Printf("accounts holding proposal votes at the end: %d\n", res.votersSeen)
	fmt.Printf("distinct expiry slots: %d\n", res.slotsSeen)
	fmt.Printf("budget:           %d objects per sweep\n", limit)
	fmt.Printf("busiest slot:     %d proposal votes across %d accounts (expiry ts %d)\n",
		res.maxPerSlot, res.maxAccounts, res.maxSlotTime)
	fmt.Printf("busiest %d-slot window: %d proposal votes (ending ts %d)\n",
		govMergeWindowSlots, res.maxPerWindow, res.maxWindowEndTime)
	if res.overBudget > 0 || res.overBudgetWindow > 0 {
		sort.Slice(res.offenders, func(i, j int) bool { return res.offenders[i].slotTime < res.offenders[j].slotTime })
		past, future := 0, 0
		fmt.Printf("\nover-budget slots (%d):\n", len(res.offenders))
		for _, o := range res.offenders {
			when := "UPCOMING"
			if o.inThePast {
				when = "ALREADY SWEPT"
				past++
			} else {
				future++
			}
			fmt.Printf("  ts=%d (%s) votes=%d accounts=%d biggest=%q(%d) [%s]\n",
				o.slotTime, time.Unix(int64(o.slotTime), 0).UTC().Format("2006-01-02 15:04:05"),
				o.votes, o.accounts, o.worst, o.worstN, when)
		}
		fmt.Printf("\nrange ends at ts %d (%s)\n", res.lastTs,
			time.Unix(int64(res.lastTs), 0).UTC().Format("2006-01-02 15:04:05"))
		if past > 0 {
			fmt.Printf("VERDICT: %d over-budget slots are ALREADY PAST — gopherd and hived have\n"+
				"         DIVERGED on those blocks, and %d more are upcoming. The account-id\n"+
				"         ordering work is required, and the past ones need explaining.\n", past, future)
		} else {
			fmt.Printf("VERDICT: %d over-budget slots, ALL of them still UPCOMING (an expiry is the\n"+
				"         last governance action plus a year, so recent actions land beyond head).\n"+
				"         Nothing has diverged yet, which is consistent with a clean replay — but\n"+
				"         the guard WILL fire at those slots, so the account-id ordering work has\n"+
				"         a deadline rather than being purely hypothetical.\n", future)
		}
		return 1
	}
	fmt.Printf("VERDICT: no slot reaches the budget, and neither does any %d-slot window —\n"+
		"         so not even a full minute of missed blocks merging sweeps together\n"+
		"         would reach it. The guard would never have fired, and gopherd's\n"+
		"         unbudgeted sweep has matched hived over this range.\n", govMergeWindowSlots)
	return 0
}
