package chain

import (
	"gopherd/internal/chain/evaluator"
	"gopherd/internal/state"
)

// CreateVesting_via_chain is a thin re-export so chain/funds.go can call
// the evaluator package's CreateVesting helper without introducing an
// import cycle.
func CreateVesting_via_chain(s *state.Session, accountName string, hiveSatoshi int64, toReward bool) (int64, error) {
	return evaluator.CreateVesting(s, accountName, hiveSatoshi, toReward)
}
