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
// Usage: gopherd govsweep-scan <block_log> <from_block> <to_block>

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"sync"
	"time"

	"gopherd/internal/blocklog"
	"gopherd/internal/fcraw"
	"gopherd/internal/protocol/block"
	"gopherd/internal/protocol/network"
	"gopherd/internal/protocol/op"
	"gopherd/internal/protocol/tx"
)

// Mirrors internal/chain/evaluator/witness.go — kept as literals because this
// is a diagnostic, and a copy that drifts is louder than an import cycle.
const (
	govVoteExpirationPeriodSec     uint32 = 365 * 24 * 60 * 60
	govFirstExpireTimestamp        uint32 = 1656590400 // HARDFORK_1_25_FIRST_GOVERNANCE_VOTE_EXPIRE_TIMESTAMP
	govMaxOldExpireShiftSec        uint32 = govVoteExpirationPeriodSec / 2
	govBlockIntervalSec            uint32 = 3
	govHF21ProposalsActivationHint uint32 = 35_921_786
)

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
	const progressEvery = 500_000

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
}

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
	}

	res := govSweepResult{slotsSeen: len(slots), votersSeen: voters}
	for slot, b := range slots {
		if b.voteCount > limit {
			res.overBudget++
		}
		if b.voteCount > res.maxPerSlot {
			res.maxPerSlot = b.voteCount
			res.maxSlotTime = slot * govBlockIntervalSec
			res.maxAccounts = b.accounts
		}
	}
	return res
}

func cmdGovSweepScan(args []string) int {
	if len(args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: gopherd govsweep-scan <block_log> <from_block> <to_block>")
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

	const workers = 12

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
	shard := span / workers
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
			shards[w] = govScanShard(path, lo, hi, w, reportProgress)
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
	if res.overBudget > 0 {
		fmt.Printf("VERDICT: %d slots exceed the budget — gopherd and hived DIVERGE on those blocks;\n"+
			"         the account-id ordering work is required before this can be enforced.\n", res.overBudget)
		return 1
	}
	fmt.Printf("VERDICT: no slot reaches the budget. The guard would never have fired,\n" +
		"         so gopherd's unbudgeted sweep has matched hived over this range.\n")
	return 0
}
