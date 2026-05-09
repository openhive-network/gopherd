package chain

import (
	"fmt"
	"math/big"
	"os"

	"hived-go/internal/protocol/hardfork"
	"hived-go/internal/state"
	"hived-go/internal/state/objects"
)

// updateVirtualSupply mirrors database::update_virtual_supply
// (libraries/chain/database.cpp:2868). Recomputes
//
//	virtual_supply = current_supply + current_hbd_supply × median_price
//
// Then, post-HF14 with a real median feed, ramps `hbd_print_rate` from
// 100% (when HBD ≤ start_percent of virtual_supply) to 0% (when HBD ≥
// stop_percent), linearly in between. The rate gates how much of
// per-cashout author / liquidity-reward HIVE is actually printed as
// HBD vs paid out as liquid HIVE.
//
// Pre-HF14 print_rate stays pinned to 10000 by virtue of the genesis
// seed (DGPO.HBDPrintRate is initialized to 10000 elsewhere).
func updateVirtualSupply(s *state.Session, blockNum uint32) {
	dgpo, err := objects.GetDGPO(s)
	if err != nil {
		return
	}
	fh, ferr := objects.GetFeedHistory(s)
	hasFeed := ferr == nil && !priceIsZero(fh.CurrentMedianHistory)

	// virtual_supply = current_supply + (HBD × median_price)
	dgpo.VirtualSupply = dgpo.CurrentSupply
	if hasFeed {
		hbdInHive := convertHBDToHive(dgpo.CurrentHBDSupply, fh.CurrentMedianHistory)
		dgpo.VirtualSupply += hbdInHive
	}

	// Print-rate ramp: only post-HF14 with a real feed.
	hf := hardfork.SetForBlock(blockNum)
	if hasFeed && hf.Has(hardfork.HF14) {
		// percent_hbd = current_hbd_supply × HIVE_100_PERCENT / virtual_supply
		// (basis points). dgpo.virtual_supply is HIVE-satoshi; current_hbd_supply
		// is HBD-satoshi but expressed as a "value share" via the feed price.
		//
		// HF24+ excludes treasury HBD from the debt-ratio calculation
		// (database.cpp:2975-2982): hbd_supply -= treasury.hbd_balance,
		// then virtual_supply = hbd_supply × median + current_supply.
		// HF21+ adds round-half-up: numerator += virtual_supply / 2.
		hbdSupply := dgpo.CurrentHBDSupply
		virtualSupplyForRatio := dgpo.VirtualSupply
		if hf.Has(hardfork.HF24) {
			treasuryName := "hive.fund"
			if treasury, terr := objects.GetAccount(s, treasuryName); terr == nil {
				hbdSupply -= treasury.HBDBalance
				if hbdSupply < 0 {
					hbdSupply = 0
				}
			}
			if hbdSupply != dgpo.CurrentHBDSupply {
				hbdAsHiveAdj := convertHBDToHive(hbdSupply, fh.CurrentMedianHistory)
				virtualSupplyForRatio = hbdAsHiveAdj + dgpo.CurrentSupply
			}
		}
		var percentHBD int64
		if virtualSupplyForRatio > 0 {
			hbdAsHive := convertHBDToHive(hbdSupply, fh.CurrentMedianHistory)
			num := new(big.Int).Mul(big.NewInt(hbdAsHive), big.NewInt(10000))
			if hf.Has(hardfork.HF21) {
				num.Add(num, big.NewInt(virtualSupplyForRatio/2))
			}
			num.Quo(num, big.NewInt(virtualSupplyForRatio))
			if num.IsInt64() {
				percentHBD = num.Int64()
			}
		}
		start := int64(dgpo.HBDStartPercent)
		stop := int64(dgpo.HBDStopPercent)
		if start == 0 || stop == 0 || stop < start {
			// Defensive fallback for old state rows that pre-date the
			// genesis seeding of these fields.
			start = 200  // 2 %
			stop = 500   // 5 %
			dgpo.HBDStartPercent = uint16(start)
			dgpo.HBDStopPercent = uint16(stop)
		}
		switch {
		case percentHBD >= stop:
			dgpo.HBDPrintRate = 0
		case percentHBD <= start:
			dgpo.HBDPrintRate = 10000
		default:
			// Linear ramp: rate = (stop - percent_hbd) × HIVE_100_PERCENT / (stop - start)
			rate := (stop - percentHBD) * 10000 / (stop - start)
			if rate < 0 {
				rate = 0
			}
			if rate > 10000 {
				rate = 10000
			}
			dgpo.HBDPrintRate = uint16(rate)
		}
		if blockNum == 5216528 {
			fmt.Fprintf(os.Stderr, "GHIVEDIFF VS-DUMP block=%d hbd_supply=%d virtual_supply=%d hbd_as_hive=%d percent_hbd=%d print_rate=%d median_base=%d median_quote=%d\n",
				blockNum, hbdSupply, virtualSupplyForRatio, convertHBDToHive(hbdSupply, fh.CurrentMedianHistory), percentHBD, dgpo.HBDPrintRate,
				fh.CurrentMedianHistory.Base.Amount, fh.CurrentMedianHistory.Quote.Amount)
		}
	}
	objects.PutDGPO(s, &dgpo)
}
