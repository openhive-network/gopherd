package op

import (
	"encoding/json"
	"fmt"
	"unicode/utf8"
)

// Protocol-level validate() for the three custom operations, ported from
// hive_operations.cpp:412-447. Until now none of them had a Validate, so a
// custom_json with no authorities, a 33-byte id or unparseable JSON was
// admitted and relayed where every hived node refuses it.

// HIVE_CUSTOM_OP_ID_MAX_LENGTH (config.hpp:424).
const customOpIDMaxLength = 32

// HIVE_MAX_AUTHORITY_MEMBERSHIP (config.hpp): the cap hived applies to the
// number of required authorities on a custom op.
const customOpMaxAuthorityMembership = 40

func (o Custom) Validate() error {
	if len(o.RequiredAuths) == 0 {
		return fmt.Errorf("custom: at least one account must be specified")
	}
	if len(o.RequiredAuths) > customOpMaxAuthorityMembership {
		return fmt.Errorf("custom: authority membership exceeded (max %d, got %d)", customOpMaxAuthorityMembership, len(o.RequiredAuths))
	}
	for _, a := range o.RequiredAuths {
		if err := validateAccountName("custom.required_auths", string(a)); err != nil {
			return err
		}
	}
	return nil
}

func (o CustomJSON) Validate() error {
	n := len(o.RequiredAuths) + len(o.RequiredPostingAuths)
	if n == 0 {
		return fmt.Errorf("custom_json: at least one account must be specified")
	}
	if n > customOpMaxAuthorityMembership {
		return fmt.Errorf("custom_json: authority membership exceeded (max %d, got %d)", customOpMaxAuthorityMembership, n)
	}
	for _, a := range o.RequiredAuths {
		if err := validateAccountName("custom_json.required_auths", string(a)); err != nil {
			return err
		}
	}
	for _, a := range o.RequiredPostingAuths {
		if err := validateAccountName("custom_json.required_posting_auths", string(a)); err != nil {
			return err
		}
	}
	if len(o.ID) > customOpIDMaxLength {
		return fmt.Errorf("custom_json: id length %d exceeds %d", len(o.ID), customOpIDMaxLength)
	}
	return validateJSONWithFallback("custom_json.json", o.JSON)
}

func (o CustomBinary) Validate() error {
	n := len(o.RequiredOwnerAuths) + len(o.RequiredActiveAuths) + len(o.RequiredPostingAuths)
	if n == 0 {
		return fmt.Errorf("custom_binary: at least one account must be specified")
	}
	for _, a := range o.RequiredAuths {
		n += len(a.KeyAuths) + len(a.AccountAuths)
	}
	if n > customOpMaxAuthorityMembership {
		return fmt.Errorf("custom_binary: authority membership exceeded (max %d, got %d)", customOpMaxAuthorityMembership, n)
	}
	if len(o.ID) > customOpIDMaxLength {
		return fmt.Errorf("custom_binary: id length %d exceeds %d", len(o.ID), customOpIDMaxLength)
	}
	return nil
}

// validateJSONWithFallback mirrors hive_operations.cpp:22-33
// (validate_json_with_fallback): accept anything fc::json::fast_is_valid
// accepts; otherwise require UTF-8 and fc's relaxed parser. Go's json.Valid
// stands in for fast_is_valid. The relaxed fallback exists so hived can
// replay pre-fix blocks; here this runs at admission only (never when
// applying a block), and the few documents only the relaxed parser accepts
// are refused — strictly a relay-side difference, never a block one.
func validateJSONWithFallback(label, s string) error {
	if json.Valid([]byte(s)) {
		return nil
	}
	if !utf8.ValidString(s) {
		return fmt.Errorf("%s: not formatted in UTF-8", label)
	}
	return fmt.Errorf("%s: not valid JSON", label)
}
