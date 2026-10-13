package chain

import (
	"fmt"
	"os"

	"gopherd/internal/state"
	"gopherd/internal/state/objects"
)

// hivedRCResourceParams are the rc_resource_param_object entries hived installs
// at HF20, for every resource except new_accounts (whose dynamics
// set_pool_params replaces with the witness-voted subsidy params).
//
// hived does not compute these at runtime. Its build runs
// programs/build_helpers/hive_build_helpers/rc_generate_resource_parameters.py
// over libraries/jsonball/data/resource_parameters.json and compiles the
// result in: floating-point decay (-expm1), a per-resource decay shift chosen
// for precision (42-51 bits), max_pool_size = 2*pool_eq, and the price-curve
// coefficients. These are that output, and equal what mainnet hived reports
// from rc_api.get_resource_params.
//
// gopherd used to re-derive them with an integer approximation at a fixed
// 36-bit shift. Every value came out slightly off — decay ~0.1%, pool_eq and
// the curve ~0.07%, budgets by one unit — and the errors compound through the
// pool levels into the price of every transaction.
var hivedRCResourceParams = map[objects.RCResource]objects.RCResourceEntry{
	objects.RCHistoryBytes: {
		Dynamics: objects.ResourceDynamics{ResourceUnit: 1, BudgetPerTime: 43403,
			PoolEq: 27050539251, MaxPool: 54101078501, DecayPerTime: 3613026481, DecayDenomShift: 51},
		Curve: objects.PriceCurve{CoeffA: 10525659774662010880, CoeffB: 211332338, Shift: 50},
	},
	objects.RCMarketBytes: {
		Dynamics: objects.ResourceDynamics{ResourceUnit: 10, BudgetPerTime: 72338,
			PoolEq: 2003755169, MaxPool: 4007510337, DecayPerTime: 2540365427, DecayDenomShift: 46},
		Curve: objects.PriceCurve{CoeffA: 14969827235074865152, CoeffB: 15654337, Shift: 55},
	},
	objects.RCStateBytes: {
		Dynamics: objects.ResourceDynamics{ResourceUnit: 1, BudgetPerTime: 43546196,
			PoolEq: 27139923979692, MaxPool: 54279847959383, DecayPerTime: 3613026481, DecayDenomShift: 51},
		Curve: objects.PriceCurve{CoeffA: 10525659774662010880, CoeffB: 212030656091, Shift: 50},
	},
	objects.RCExecutionTime: {
		Dynamics: objects.ResourceDynamics{ResourceUnit: 1, BudgetPerTime: 40000000,
			PoolEq: 69256028844, MaxPool: 138512057687, DecayPerTime: 2539677724, DecayDenomShift: 42},
		Curve: objects.PriceCurve{CoeffA: 14969827235074865152, CoeffB: 541062725, Shift: 59},
	},
}

// alignRCResourceParams rewrites the stored resource parameters to hived's
// where they differ, so state built by an older gopherd stops pricing RC with
// the approximated values. RC is not consensus before HF29, so this changes no
// block's validity; pools then converge on their own through decay. A no-op on
// a chain that has not reached HF20 or already holds the right values.
func alignRCResourceParams(s *state.Session) {
	if !objects.HasRCResourceParam(s) {
		return
	}
	rp, err := objects.GetRCResourceParam(s)
	if err != nil {
		return
	}
	changed := 0
	for res, want := range hivedRCResourceParams {
		if rp.Entries[res] != want {
			rp.Entries[res] = want
			changed++
		}
	}
	if changed == 0 {
		return
	}
	objects.PutRCResourceParam(s, &rp)
	fmt.Fprintf(os.Stderr, "gopherd: rc: aligned %d resource parameter set(s) with hived's generated values\n", changed)
}
