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

	"gopherd/internal/protocol/hardfork"
	"gopherd/internal/protocol/op"
	"gopherd/internal/state"
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
	// OwnerAuthHistoryStartBlock overrides where owner_authority_history
	// tracking begins. Zero means OwnerAuthHistoryDefaultStartBlock (mainnet).
	// Only tests set it, but it lives here rather than in a package global so
	// concurrent fixtures cannot stomp on each other's consensus parameters.
	OwnerAuthHistoryStartBlock uint32
	// BlockTime is the time_point_sec of the block currently being
	// applied — i.e., what hived calls `_current_timestamp`. Used for
	// the few places where C++ explicitly reads the new block's
	// timestamp (e.g., signing-witness records).
	BlockTime uint32
	// HeadBlockTime is the value hived's `head_block_time()` returns
	// while transactions are still applying — that is, the previous
	// block's timestamp. dgpo.time only advances to the current block
	// AFTER tx application via update_global_dynamic_data, so
	// evaluators that match `_now = _db.head_block_time()` should read
	// HeadBlockTime, not BlockTime, to stay byte-compatible with C++.
	HeadBlockTime uint32
	// InControl mirrors hived's `_db.is_in_control()` — true only when
	// validating at the head (production / admission / unit tests),
	// false in replay/sync. Gates the C++ asserts that only run at
	// admission (e.g. delegate_rc min-delegation and RC sufficiency).
	InControl bool
	// Witness is the producer of the block currently being applied (the
	// `witness` field of its signed_block_header). hived's DGPO is updated
	// to point at this name BEFORE evaluators run, so any evaluator
	// looking at dgpo.current_witness sees this account. We carry it on
	// Context to avoid having to mutate DGPO at the apply-loop top.
	Witness string

	// EmitVOp is a callback the chain.Database supplies for evaluators
	// to push a virtual operation onto the per-block vop buffer. It is
	// untyped here to avoid importing the chain package (which would
	// create a cycle); chain.Database wires it as a closure that casts
	// the payload into chain.BlockVOp. Evaluators should pass a value
	// of type chain.BlockVOp (caller side it's exposed as `any`). May
	// be nil — emit callers must guard.
	EmitVOp func(any)
}

// Evaluator applies a single operation to ctx.DB.
type Evaluator interface {
	// Apply mutates state per the operation's semantics. Return nil on
	// success, or an error describing why the op cannot apply (insufficient
	// balance, missing account, hardfork-gated, etc.).
	Apply(ctx *Context, o op.Op) error
}

// EvaluatorFunc adapts a plain function to the Evaluator interface.
type EvaluatorFunc func(ctx *Context, o op.Op) error

func (f EvaluatorFunc) Apply(ctx *Context, o op.Op) error { return f(ctx, o) }

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
	return ApplyOp(ctx, op.FromOperation(o))
}

// ApplyOp dispatches a tagged Op to the registered evaluator. Reads the
// tag directly from the struct (no OpTag() iface call) and dispatches
// directly to the evaluator's Apply(ctx, op.Op).
func ApplyOp(ctx *Context, o op.Op) error {
	tag := o.Tag
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
