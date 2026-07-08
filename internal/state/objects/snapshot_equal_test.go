package objects

import (
	"bytes"
	"encoding/binary"
	"sort"
	"testing"
)

// snapshotRecordSetsEqual decodes two length-prefixed snapshot streams
// (magic+version+count header, then uvarint-length records) and compares
// the record MULTISETS, ignoring order. Cold-tier stores emit evicted
// rows after the sorted resident set, so byte order differs from a
// pure-resident store while the decoded state is identical.
func snapshotRecordSetsEqual(t *testing.T, a, b []byte) bool {
	t.Helper()
	ra, oka := snapshotRecords(t, a)
	rb, okb := snapshotRecords(t, b)
	if !oka || !okb || len(ra) != len(rb) {
		return false
	}
	sort.Slice(ra, func(i, j int) bool { return bytes.Compare(ra[i], ra[j]) < 0 })
	sort.Slice(rb, func(i, j int) bool { return bytes.Compare(rb[i], rb[j]) < 0 })
	for i := range ra {
		if !bytes.Equal(ra[i], rb[i]) {
			return false
		}
	}
	return true
}

func snapshotRecords(t *testing.T, buf []byte) ([][]byte, bool) {
	t.Helper()
	const headerLen = 12 // magic(4) + version(4) + count(4)
	if len(buf) < headerLen {
		return nil, false
	}
	var out [][]byte
	p := headerLen
	for p < len(buf) {
		l, n := binary.Uvarint(buf[p:])
		if n <= 0 || p+n+int(l) > len(buf) {
			return nil, false
		}
		out = append(out, buf[p+n:p+n+int(l)])
		p += n + int(l)
	}
	return out, true
}
