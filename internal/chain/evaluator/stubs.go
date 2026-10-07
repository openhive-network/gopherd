package evaluator

// stubs.go registers no-op evaluators for the small handful of regular
// op tags that legitimately do not mutate consensus state at the chain
// level. Every other tag has a real evaluator in its own file
// (account_create.go, transfer.go, witness.go, comment.go, etc.).
//
// The remaining no-op stubs are:
//
//   custom, custom_json, custom_binary  — data-emission ops; their
//     side effects live in custom_operation_interpreter plugins, not
//     the chain. State-level no-op is correct.
//   witness_block_approve              — pre-HF1 only, never used since.
//   reset_account, set_reset_account   — historic placeholders; never
//     used in real mainnet activity.
//
// Every other tag has a real evaluator registered elsewhere; the
// TestEveryRegularTagHasEvaluator test enforces full coverage.

import (
	"fmt"

	"gopherd/internal/protocol/hardfork"
	"gopherd/internal/protocol/op"
)

// noopStub returns nil for every op — used for tags whose state effects
// live in plugins (custom*) or are pre-HF1 placeholders that never fire
// on real mainnet (witness_block_approve, reset_account, set_reset_account).
//
// All registered stubs were previously paired with a noValidate function;
// no surviving stub needs body validation, so the prior validate-callback
// machinery is gone.
func noopStub(ctx *Context, opArg op.Op) error { return nil }

// customOpDataMaxLength is HIVE_CUSTOM_OP_DATA_MAX_LENGTH (config.hpp:425).
const customOpDataMaxLength = 8192

// customEvaluator mirrors custom_evaluator::do_apply (hive_evaluator.cpp:328-335):
// from HF26 (SOLIDIFY_OLD_SOFTFORKS) the payload may not exceed 8192 bytes.
// The chain itself does nothing else with `custom`.
func customEvaluator(ctx *Context, opArg op.Op) error {
	if ctx.Hardforks.Has(hardfork.HF26) && len(opArg.AsCustom().Data) > customOpDataMaxLength {
		return fmt.Errorf("custom: operation data must be at most %d bytes", customOpDataMaxLength)
	}
	return nil
}

// customBinaryEvaluator mirrors custom_binary_evaluator::do_apply
// (hive_evaluator.cpp:376-379), which starts with an unconditional
// UNREACHABLE_CODE_ASSERT( false ): the operation is disallowed and no block
// has ever carried one. Accepting it as a no-op admitted and relayed
// transactions every hived node refuses.
func customBinaryEvaluator(ctx *Context, opArg op.Op) error {
	return fmt.Errorf("custom_binary: operation disallowed")
}

func init() {
	register(op.TagCustom, EvaluatorFunc(customEvaluator))
	// custom_json now has a real evaluator (custom_json_rc.go) that
	// dispatches consensus-touching IDs (chiefly "rc" for delegate_rc
	// post-HF22). It registers itself separately.
	register(op.TagCustomBinary, EvaluatorFunc(customBinaryEvaluator))
	register(op.TagWitnessBlockApprove, EvaluatorFunc(noopStub))
	register(op.TagResetAccount, EvaluatorFunc(noopStub))
	register(op.TagSetResetAccount, EvaluatorFunc(noopStub))
}
