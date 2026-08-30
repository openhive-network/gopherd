//go:build testnet

package hardfork

// A testnet launches directly on CURRENT consensus rules: hived applies every
// hardfork already due by block 1's timestamp (database.cpp:2246), and a
// testnet created today carries a present-day timestamp, so all of them are
// due. Its nodes then run HF28 rules from block 1 onward.
//
// gopherd decides "is HF N active?" from MainnetActivationBlock, which encodes
// where each fork landed on the MAINNET chain — block numbers in the millions.
// On a testnet sitting at block ~400 that table reports every hardfork as
// inactive, so gopherd evaluated blocks under HF0 rules while its hived peers
// used HF28. The two agreed on every block ID (block IDs do not commit to
// state) and silently disagreed on inflation, vesting and the reward fund.
//
// Zeroing the activation blocks makes every hardfork active from block 1, which
// is what the chain itself reports (last_hardfork = 28).
//
// LIMITATION: this models a testnet launched with all hardforks in the past —
// what `hived --chain-id <testnet>` gives you today, and what interop requires.
// It does NOT model a testnet configured via hived's hf_times to schedule a
// FUTURE hardfork; such a chain would activate mid-run and gopherd would be
// early. Driving activation off the hardfork_property_object instead of a table
// would cover that case and is the more general fix.
// UNSCHEDULED hardforks are deliberately EXCLUDED from the zeroing. A fork
// still in development (MainnetActivationBlock == NeverActivates) has no
// agreed activation point, so switching it on from block 1 would make gopherd
// enforce rules its hived testnet peers do not — the mirror image of the bug
// this file exists to fix, and a consensus split rather than a silent state
// drift. hived guards the same case from the other side: HIVE_HARDFORK_1_29's
// default time is year 2100 precisely "so HF29 never auto-activates on
// testnets that don't explicitly schedule it", and a mainnet build does not
// register it at all.
//
// Testing an unscheduled fork therefore takes a deliberate act — give it a
// real activation block — which is the point. Interop with a stock hived
// testnet keeps working meanwhile.
func init() {
	for i := ID(1); i <= NumHardforks; i++ {
		if MainnetActivationBlock[i] == NeverActivates {
			continue
		}
		MainnetActivationBlock[i] = 0
	}
}
