package objects

// Hot-subsystem slot IDs. Each Ensure* accessor in this package uses
// one of these to index state.Database's hotSubsystem array, skipping
// the subsystemReg map probe on the hot path. Slots are assigned
// compile-time; the array is sized in state.hotSubsystemSlotCount.
//
// Keep IDs dense and < hotSubsystemSlotCount.
const (
	hotSlotDGPO             = 0
	hotSlotAccount          = 1
	hotSlotWitness          = 2
	hotSlotComment          = 3
	hotSlotCommentVote      = 4
	hotSlotWitnessVote      = 5
	hotSlotWitnessSchedule  = 6
	hotSlotFeedHistory      = 7
	hotSlotRewardFund       = 8
	hotSlotConvertRequest   = 9
	hotSlotVestWithdrawal   = 10
	hotSlotCommentCashout   = 11
	hotSlotTransaction      = 12
	hotSlotBlockSummary     = 13
	hotSlotLimitOrder       = 14
)
