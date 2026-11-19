package evaluator

import (
	"encoding/binary"
	"fmt"
	"slices"
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
	// hived's rules apply to the decoded flat_set, which fc builds by
	// inserting: static_variant compares by tag alone, so repeats of a tag
	// collapse to the first. Two end dates are one extension (the first),
	// not two.
	var tags []uint64
	var first uint32
	pos := n
	for i := uint64(0); i < count; i++ {
		tag, m := binary.Uvarint(ext[pos:])
		if m <= 0 {
			return 0, false, fmt.Errorf("update_proposal: malformed extension tag")
		}
		pos += m
		var date uint32
		switch tag {
		case 0:
		case 1:
			if len(ext) < pos+4 {
				return 0, false, fmt.Errorf("update_proposal: truncated update_proposal_end_date")
			}
			date = binary.LittleEndian.Uint32(ext[pos : pos+4])
			pos += 4
		default:
			return 0, false, fmt.Errorf("update_proposal: unknown extension tag %d", tag)
		}
		if !slices.Contains(tags, tag) {
			tags = append(tags, tag)
			if tag == 1 {
				first = date
			}
		}
	}
	if len(tags) >= 2 {
		return 0, false, fmt.Errorf("update_proposal: cannot have more than 1 extension")
	}
	if tags[0] != 1 {
		return 0, false, fmt.Errorf("update_proposal: extension is not update_proposal_end_date")
	}
	return first, true, nil
}
