package evaluator

import (
	"errors"
	"fmt"

	"gopherd/internal/protocol/auth"
	"gopherd/internal/protocol/hardfork"
)

// HF29 is still in development: it has no activation point, so
// hardfork.MainnetActivationBlock[HF29] is hardfork.NeverActivates and
// ctx.Hardforks.Has(HF29) is false on every chain gopherd can currently
// replay. Each rule below is therefore reachable ONLY via ctx.InControl.
//
// That is not a stopgap, it is the shape hived ships:
//
//	if( _db.is_in_control() || _db.has_hardfork( HIVE_HARDFORK_1_29_... ) )
//
// The two halves do different jobs. `is_in_control()` is true when a node is
// admitting a NEW transaction (block production, mempool admission) and false
// while replaying or syncing history. So the check tightens what the network
// will accept from here on, while leaving the historical chain — which
// contains transactions these rules would reject — replayable byte for byte.
// Gating on the hardfork alone would break replay; gating on InControl alone
// would mean nodes disagree once the fork lands. Both are required.
//
// Consequence worth remembering: a rule in this file changes behaviour at the
// HEAD today, before HF29 is scheduled anywhere. A clean replay proves nothing
// about it (see the tip-only bug class in the p2p work) — the tests that
// matter set InControl.
func hf29Rule(ctx *Context, id hardfork.ID) bool {
	return ctx.InControl || ctx.Hardforks.Has(id)
}

// ErrOpenAuthority is returned when an account_update[2] tries to install an
// authority nobody can ever satisfy.
var ErrOpenAuthority = errors.New("cannot set an open authority")

// ErrAuthoritySize is returned when an authority exceeds
// HIVE_MAX_AUTHORITY_MEMBERSHIP.
var ErrAuthoritySize = errors.New("authority membership exceeded")

// maxAuthorityMembership mirrors HIVE_MAX_AUTHORITY_MEMBERSHIP
// (libraries/protocol/include/hive/protocol/config.hpp:375). The 41 variant
// next to it applies only to HIVE_CONVERTER_BUILD, which we do not model.
const maxAuthorityMembership = 40

// validateAuthSize mirrors hive::protocol::validate_auth_size
// (libraries/protocol/hive_operations.cpp:36) — account_auths and key_auths
// are counted TOGETHER against one limit, not separately.
func validateAuthSize(a auth.Authority, subject string) error {
	size := len(a.AccountAuths) + len(a.KeyAuths)
	if size > maxAuthorityMembership {
		return fmt.Errorf("%w: %s max %d, current %d",
			ErrAuthoritySize, subject, maxAuthorityMembership, size)
	}
	return nil
}

// validateAccountUpdateAuthority mirrors validate_account_update_authority
// (libraries/chain/hive_evaluator_account.cpp:97).
//
// An authority with weight_threshold == 0 is "open": sign_state treats the
// threshold as already met, so ANY signature satisfies it. Installing one on
// the owner role hands the account to the whole network; hived's issue #586
// hit this by way of malformed JSON erasing an owner authority, which
// permanently locked the account out of that role.
//
// hived checks the threshold and nothing else here, even though 1_29.hf
// describes the rule as covering "otherwise impossible-to-satisfy" authorities
// too. Match the code, not the comment: an impossible authority (threshold
// above the sum of all weights) is merely unusable by its owner, whereas an
// open one is usable by everyone, and only the latter is rejected. Note also
// that account_create is deliberately NOT covered — see hived commit
// d88fe7371, which documents that gap with a test rather than closing it.
func validateAccountUpdateAuthority(a auth.Authority, account, role string) error {
	if a.WeightThreshold == 0 {
		return fmt.Errorf("%w: %s.%s", ErrOpenAuthority, account, role)
	}
	return nil
}
