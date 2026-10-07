package evaluator

import (
	"encoding/binary"
	"fmt"
)

// updateProposalEndDate decodes update_proposal_operation.extensions, which
// the op reader keeps as raw bytes (ops_dhf.go: varint count, then per item
// a varint variant tag; tag 0 is void_t with no body, tag 1 is
// update_proposal_end_date with a 4-byte little-endian time_point_sec).
//
// It applies hived's shape rules from dhf_evaluator.cpp:123-137:
//
//	HF25+:   extensions.size() < 2, and a single extension MUST be the
//	         end-date variant (`get<update_proposal_end_date>()` on a void_t
//	         throws, i.e. the tx is rejected).
//	pre-HF25: extensions must be empty.
//
// The range checks against the proposal's own dates are the caller's.
func updateProposalEndDate(ext []byte, hf25 bool) (endDate uint32, has bool, err error) {
	if len(ext) == 0 {
		return 0, false, nil
	}
	count, n := binary.Uvarint(ext)
	if n <= 0 {
		return 0, false, fmt.Errorf("update_proposal: malformed extensions count")
	}
	if count == 0 {
		return 0, false, nil
	}
	if !hf25 {
		return 0, false, fmt.Errorf("update_proposal: cannot set extensions before HF25")
	}
	if count >= 2 {
		return 0, false, fmt.Errorf("update_proposal: cannot have more than 1 extension")
	}
	tag, m := binary.Uvarint(ext[n:])
	if m <= 0 {
		return 0, false, fmt.Errorf("update_proposal: malformed extension tag")
	}
	if tag != 1 {
		return 0, false, fmt.Errorf("update_proposal: extension is not update_proposal_end_date")
	}
	body := ext[n+m:]
	if len(body) < 4 {
		return 0, false, fmt.Errorf("update_proposal: truncated update_proposal_end_date")
	}
	return binary.LittleEndian.Uint32(body[:4]), true, nil
}
