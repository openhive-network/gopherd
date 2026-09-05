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
	// Header length is format-dependent, and this helper is shared across
	// stores: the vote store is still magic(4)+version(4)+count(4), while the
	// comment store's v2 adds a cold hint. Hardcoding one length silently
	// mis-parses the other (it reads the first record's length prefix as data),
	// which is how this helper broke the vote tests once.
	const baseHeader = 12 // magic(4) + version(4) + count(4)
	if len(buf) < baseHeader {
		return nil, false
	}
	headerLen := baseHeader
	if [4]byte(buf[0:4]) == commentStoreSnapshotMagic &&
		binary.LittleEndian.Uint32(buf[4:8]) == commentStoreSnapshotVersion {
		headerLen = 16 // + coldHint(4)
	}
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
		if l == 0 {
			break // end-of-stream terminator
		}
		out = append(out, buf[p+n:p+n+int(l)])
		p += n + int(l)
	}
	return out, true
}
