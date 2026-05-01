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

	"hived-go/internal/protocol/op"
)

func stub(name string, validate func(op.Operation) error) Evaluator {
	return EvaluatorFunc(func(ctx *Context, o op.Operation) error {
		if validate != nil {
			if err := validate(o); err != nil {
				return fmt.Errorf("%s (stub): %w", name, err)
			}
		}
		return nil
	})
}

func noValidate(_ op.Operation) error { return nil }

func init() {
	register(op.TagCustom, stub("custom", noValidate))
	// custom_json now has a real evaluator (custom_json_rc.go) that
	// dispatches consensus-touching IDs (chiefly "rc" for delegate_rc
	// post-HF22). It registers itself separately.
	register(op.TagCustomBinary, stub("custom_binary", noValidate))
	register(op.TagWitnessBlockApprove, stub("witness_block_approve", noValidate))
	register(op.TagResetAccount, stub("reset_account", noValidate))
	register(op.TagSetResetAccount, stub("set_reset_account", noValidate))
}
