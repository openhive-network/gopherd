package evaluator

import (
	"encoding/json"
	"fmt"

	"hived-go/internal/protocol/auth"
	"hived-go/internal/protocol/hardfork"
	"hived-go/internal/protocol/op"
	"hived-go/internal/state/objects"
)

// customJSONEvaluator handles the consensus-level custom_json ops.
// Most custom_json content is plugin-side (no consensus effect), but a
// few payload IDs DO mutate state — most notably "rc" (delegate_rc,
// HF22+). This evaluator dispatches on `id` and applies the rare
// consensus-touching variants; everything else is a no-op.
//
// Payload format for "rc":
//
//	["delegate_rc", { "from": "alice", "delegatees": ["bob","carol"], "max_rc": 5000000 }]
//
// Mirrors hive::protocol::rc_custom_operation in
// libraries/protocol/include/hive/protocol/hive_custom_operations.hpp.
type customJSONEvaluator struct{}

func (customJSONEvaluator) Apply(ctx *Context, opArg op.Op) error {
	if opArg.Tag != op.TagCustomJSON {
		return fmt.Errorf("customJSONEvaluator: unexpected tag %d (%s)", opArg.Tag, opArg.Tag.Name())
	}
	cj := opArg.AsCustomJSON()
	switch cj.ID {
	case "rc":
		// HF22+ only.
		if !ctx.Hardforks.Has(hardfork.HF22) {
			return nil
		}
		return applyRCCustomJSON(ctx, *cj)
	}
	// All other custom_json IDs are plugin-data only (no chain state
	// effect). Mirrors hived's behavior — custom_operation_interpreter
	// plugins, not the chain, parse them.
	return nil
}

// applyRCCustomJSON parses the "rc" payload and dispatches the named
// sub-operation. Currently supports `delegate_rc`.
func applyRCCustomJSON(ctx *Context, cj op.CustomJSON) error {
	// Format: ["op_name", {...args...}]
	var raw []json.RawMessage
	if err := json.Unmarshal([]byte(cj.JSON), &raw); err != nil {
		// Tolerate malformed JSON — hived also no-ops on bad payload.
		return nil
	}
	if len(raw) != 2 {
		return nil
	}
	var name string
	if err := json.Unmarshal(raw[0], &name); err != nil {
		return nil
	}
	switch name {
	case "delegate_rc":
		return applyDelegateRC(ctx, cj, raw[1])
	}
	return nil
}

// applyDelegateRC handles a delegate_rc payload — set the delegation
// from `from` to each `delegatees` to `max_rc`. Posting auth gates
// the op (must be in cj.RequiredPostingAuths).
func applyDelegateRC(ctx *Context, cj op.CustomJSON, args json.RawMessage) error {
	var p struct {
		From       string   `json:"from"`
		Delegatees []string `json:"delegatees"`
		MaxRC      int64    `json:"max_rc"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return nil
	}
	if p.From == "" || p.MaxRC < 0 {
		return nil
	}
	// Validate from-account auth gate: from must appear in
	// required_posting_auths (per delegate_rc_operation::get_required_posting_authorities).
	hasFrom := false
	for _, n := range cj.RequiredPostingAuths {
		if string(n) == p.From {
			hasFrom = true
			break
		}
	}
	if !hasFrom {
		return nil
	}
	from, err := objects.GetAccount(ctx.DB, auth.AccountName(p.From))
	if err != nil {
		return nil
	}
	for _, dn := range p.Delegatees {
		to, err := objects.GetAccount(ctx.DB, auth.AccountName(dn))
		if err != nil {
			continue
		}
		if err := applyOneRCDelegation(ctx, &from, &to, p.MaxRC); err != nil {
			continue
		}
	}
	objects.PutAccount(ctx.DB, &from)
	return nil
}

func init() {
	register(op.TagCustomJSON, customJSONEvaluator{})
}

// applyOneRCDelegation applies a single (from → to) delegation update,
// mutating the in-memory `from` and writing `to` back. Mirrors
// chain.ApplyRCDelegation but operates on already-loaded accounts.
func applyOneRCDelegation(ctx *Context, from, to *objects.Account, maxRC int64) error {
	if from.Name == to.Name || maxRC < 0 {
		return nil
	}
	prev, err := objects.GetRCDirectDelegation(ctx.DB, from.Name, to.Name)
	prevAmt := int64(0)
	if err == nil {
		prevAmt = int64(prev.DelegatedRC)
	}
	delta := maxRC - prevAmt
	if delta == 0 {
		return nil
	}
	from.DelegatedRCOut += delta
	to.DelegatedRCIn += delta
	if from.DelegatedRCOut < 0 {
		from.DelegatedRCOut = 0
	}
	if to.DelegatedRCIn < 0 {
		to.DelegatedRCIn = 0
	}
	if maxRC == 0 {
		objects.DeleteRCDirectDelegation(ctx.DB, from.Name, to.Name)
	} else {
		objects.PutRCDirectDelegation(ctx.DB, &objects.RCDirectDelegation{
			From: from.Name, To: to.Name, DelegatedRC: uint64(maxRC),
		})
	}
	objects.PutAccount(ctx.DB, to)
	return nil
}
