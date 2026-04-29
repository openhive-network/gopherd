package chain

import (
	"hived-go/internal/state"
	"hived-go/internal/state/objects"
)

// commentCashoutAuthorReward is the placeholder HIVE-satoshi total paid
// to the author of each comment at cashout.
//
// hived's actual payout is content_reward × (rshares² / total_rshares²),
// distributed 75% author / 25% curators (with the % shifting at HF17 /
// HF21). Modeling that requires per-block content_reward accumulation in
// total_reward_fund_hive plus a global rshares² sum that's expensive to
// maintain.
//
// As a stopgap each cashed-out comment with positive net_rshares grants
// the author a flat 1 HIVE total reward, split per hived's percent_hbd
// rule: (percent_hbd / 20000) of the reward is paid as HIVE liquid
// (matching the HBD-portion path that pre-feed-history reduces to liquid
// HIVE), the remainder as VESTS. Curators receive a small per-vote
// credit at vote time (see voteEvaluator). Replace with the real reward2
// curve once virtual_supply / reward_fund tracking lands.
const commentCashoutAuthorReward int64 = 100000 // 100.000 HIVE in satoshi

// processCommentCashouts pays out comments whose CashoutTime has elapsed.
// Mirrors the perform_cashout / cashout_comment_helper flow in
// libraries/chain/database_comment.cpp, drastically simplified.
func processCommentCashouts(s *state.Session, blockTime uint32) {
	type pending struct {
		cashoutTime uint32
		author      string
		permlink    string
	}
	var ready []pending

	// Snapshot ready entries so we don't mutate the table mid-iteration.
	_ = s.Range(objects.CommentCashoutScheduleTable, state.PrimaryIndex, nil,
		func(body, _ []byte) (bool, error) {
			if len(body) < 4+state.AccountNameKeyLen {
				return true, nil
			}
			t := uint32(body[0])<<24 | uint32(body[1])<<16 | uint32(body[2])<<8 | uint32(body[3])
			if t > blockTime {
				return false, nil
			}
			authorBytes := body[4 : 4+state.AccountNameKeyLen]
			permlink := body[4+state.AccountNameKeyLen:]
			n := state.AccountNameKeyLen
			for n > 0 && authorBytes[n-1] == 0 {
				n--
			}
			ready = append(ready, pending{
				cashoutTime: t,
				author:      string(authorBytes[:n]),
				permlink:    string(permlink),
			})
			return true, nil
		})

	for _, p := range ready {
		processOneCashout(s, p.cashoutTime, p.author, p.permlink)
	}
}

// processOneCashout pays out a single due comment.
func processOneCashout(s *state.Session, scheduledTime uint32, author, permlink string) {
	objects.DeleteCommentCashoutSchedule(s, scheduledTime, author, permlink)

	c, err := objects.GetComment(s, author, permlink)
	if err != nil {
		return
	}

	// Mark as paid out. Setting CashoutTime to a sentinel keeps the
	// comment row but prevents re-scheduling. (Hived's full flow may
	// also schedule a second cashout window pre-HF17 for additional
	// curation; we collapse into a single payout for now.)
	c.CashoutTime = 0

	// Only pay if AllowCurationRewards (a comment_options flag) and the
	// comment had any positive votes — matches hived's gate.
	if c.AllowCurationRewards && c.VoteCount > 0 {
		// Split: percent_hbd / 20000 share goes to liquid HIVE balance
		// (the path that hived's create_hbd reduces to before the price
		// feed is established), remainder to VESTS via CreateVesting.
		hbdHive := commentCashoutAuthorReward * int64(c.PercentHbd) / 20000
		vestingHive := commentCashoutAuthorReward - hbdHive

		if hbdHive > 0 {
			a, gerr := objects.GetAccount(s, author)
			if gerr == nil {
				a.Balance += hbdHive
				objects.PutAccount(s, &a)
				if dgpo, derr := objects.GetDGPO(s); derr == nil {
					dgpo.CurrentSupply += hbdHive
					objects.PutDGPO(s, &dgpo)
				}
			}
		}
		if vestingHive > 0 {
			_, _ = createVestingForCashout(s, author, vestingHive)
		}
	}

	objects.PutComment(s, &c)
}

// createVestingForCashout invokes the evaluator package's CreateVesting
// helper through the chain bridge, keeping the import direction one-way.
func createVestingForCashout(s *state.Session, account string, hiveSatoshi int64) (int64, error) {
	return CreateVesting_via_chain(s, account, hiveSatoshi, false)
}
