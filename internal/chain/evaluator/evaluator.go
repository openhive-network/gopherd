// Package evaluator implements the per-operation apply logic that mutates
// state on every block. Each operation tag (op.Tag) has a corresponding
// Evaluator implementation; the chain database walks a block's
// transactions and dispatches to the right evaluator for each op.
//
// Mirroring hived's evaluator.hpp / hive_evaluator_*.cpp tree, but in
// Go-idiomatic form: no template macros, direct interface dispatch,
// per-operation hardfork gating via the hardfork.Set bitmap precomputed
// once per block.
package evaluator

import (
	"errors"
	"fmt"

	"hived-go/internal/protocol/hardfork"
	"hived-go/internal/protocol/op"
	"hived-go/internal/state"
)

// Context carries the per-block state visible to evaluators. One Context
// is constructed per block and passed to every Apply during that block.
type Context struct {
	// DB is the live state session. Evaluators read/write through it;
	// the caller commits or undoes after applying the whole block.
	DB *state.Session
	// Hardforks is the set of hardforks active at this block height.
	// Evaluators check `ctx.Hardforks.Has(hardfork.HF20)` to gate behavior.
	Hardforks hardfork.Set
	// BlockNum is the block currently being applied (1-based).
	BlockNum uint32
	// BlockTime is the head block's time_point_sec.
	BlockTime uint32
}

// Evaluator applies a single operation to ctx.DB.
type Evaluator interface {
	// Apply mutates state per the operation's semantics. Return nil on
	// success, or an error describing why the op cannot apply (insufficient
	// balance, missing account, hardfork-gated, etc.).
	Apply(ctx *Context, o op.Operation) error
}

// EvaluatorFunc adapts a plain function to the Evaluator interface.
type EvaluatorFunc func(ctx *Context, o op.Operation) error

func (f EvaluatorFunc) Apply(ctx *Context, o op.Operation) error { return f(ctx, o) }

// dispatch maps op.Tag to the evaluator that handles it. Populated via
// init() in per-op evaluator files.
var dispatch [op.TagFirstVirtual]Evaluator

// register associates an evaluator with a tag. Called from per-op file init().
func register(tag op.Tag, e Evaluator) {
	if int(tag) >= len(dispatch) {
		panic(fmt.Sprintf("evaluator.register: tag %d out of range", tag))
	}
	if dispatch[tag] != nil {
		panic(fmt.Sprintf("evaluator.register: tag %d already registered", tag))
	}
	dispatch[tag] = e
}

// Apply dispatches o to the registered evaluator for its tag.
//
// Returns ErrNotImplemented when no evaluator has been wired up yet for
// the operation's tag. The chain database currently treats this as a
// hard error during replay so we know to fill in coverage; in P1 final
// state every regular op should have an evaluator.
func Apply(ctx *Context, o op.Operation) error {
	tag := o.OpTag()
	if int(tag) >= len(dispatch) || dispatch[tag] == nil {
		return fmt.Errorf("%w: tag %d (%s)", ErrNotImplemented, tag, tag.Name())
	}
	if err := dispatch[tag].Apply(ctx, o); err != nil {
		return fmt.Errorf("%s: %w", tag.Name(), err)
	}
	return nil
}

// ErrNotImplemented is returned when an op has no registered evaluator.
var ErrNotImplemented = errors.New("evaluator not implemented")

// Coverage returns the set of op tags with registered evaluators.
// Useful for diagnostics and tests.
func Coverage() []op.Tag {
	out := make([]op.Tag, 0, len(dispatch))
	for i, e := range dispatch {
		if e != nil {
			out = append(out, op.Tag(i))
		}
	}
	return out
}
