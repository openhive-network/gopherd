package evaluator

import (
	"encoding/binary"
	"testing"
)

func endDateExt(dates ...uint32) []byte {
	ext := []byte{byte(len(dates))}
	for _, d := range dates {
		ext = append(ext, 1)
		ext = binary.LittleEndian.AppendUint32(ext, d)
	}
	return ext
}

// TestUpdateProposalEndDateUsesDecodedSet pins dhf_evaluator.cpp:130-139 as
// applied to the decoded flat_set: repeats of a tag collapse to the first, so
// two end dates are one extension and the first date is the one used.
func TestUpdateProposalEndDateUsesDecodedSet(t *testing.T) {
	for _, tc := range []struct {
		name    string
		ext     []byte
		hf25    bool
		want    uint32
		has     bool
		wantErr bool
	}{
		{"none", nil, true, 0, false, false},
		{"empty set", []byte{0}, false, 0, false, false},
		{"one end date", endDateExt(100), true, 100, true, false},
		{"repeated end date keeps the first", endDateExt(100, 200), true, 100, true, false},
		{"void only", []byte{1, 0}, true, 0, false, true},
		{"void and end date are two", append([]byte{2, 0}, endDateExt(100)[1:]...), true, 0, false, true},
		{"before HF25", endDateExt(100), false, 0, false, true},
		{"truncated", []byte{1, 1, 0, 0}, true, 0, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, has, err := updateProposalEndDate(tc.ext, tc.hf25)
			if (err != nil) != tc.wantErr || got != tc.want || has != tc.has {
				t.Fatalf("got (%d, %v, %v), want (%d, %v, err=%v)", got, has, err, tc.want, tc.has, tc.wantErr)
			}
		})
	}
}
