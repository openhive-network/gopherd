package objects

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
)

// Cold bookkeeping trailer.
//
// A snapshot taken over a durable cold tier omits the cold rows; they stay in
// the tier, which the snapshot carries as a checkpoint or which survives the
// restart. What the stores still need at load is the bookkeeping those rows
// would have rebuilt on the way in:
//
//	comment store: coldTotal, the cold-membership bloom, coldRoot
//	vote store:    coldCount (cold votes per comment)
//
// Rebuilding them means walking every key in the tier, and on pebble a key
// walk reads the values too (they share the data blocks): the whole tier,
// tens of GB. So the durable stream ends with them instead. They are tens to
// hundreds of MB, and they are exactly the in-memory state at save time, so
// they cannot disagree with the tier saved beside them.
//
// Each section carries a CRC: a corrupt bloom gives false NEGATIVES (a cold
// comment reads as missing), which must be a load error, not a silent hole.

const coldAccountingMagic = "CACC"

func writeColdAccounting(w io.Writer, sizeHint int, fill func(b []byte) []byte) error {
	body := fill(make([]byte, 0, sizeHint))
	var hdr [16]byte
	copy(hdr[0:4], coldAccountingMagic)
	binary.LittleEndian.PutUint64(hdr[4:12], uint64(len(body)))
	binary.LittleEndian.PutUint32(hdr[12:16], crc32.Checksum(body, castagnoliTable))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err := w.Write(body)
	return err
}

// maxColdAccountingBytes bounds a trailer before it is allocated. A bloom for
// 2^30 keys at 10 bits/key is ~1.3 GB; nothing legitimate is larger.
const maxColdAccountingBytes = 4 << 30

func readColdAccounting(r io.Reader) ([]byte, error) {
	var hdr [16]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, fmt.Errorf("cold accounting: read header: %w", err)
	}
	if string(hdr[0:4]) != coldAccountingMagic {
		return nil, fmt.Errorf("cold accounting: bad magic %q", hdr[0:4])
	}
	n := binary.LittleEndian.Uint64(hdr[4:12])
	if n > maxColdAccountingBytes {
		return nil, fmt.Errorf("cold accounting: implausible size %d", n)
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, fmt.Errorf("cold accounting: read body: %w", err)
	}
	if crc32.Checksum(body, castagnoliTable) != binary.LittleEndian.Uint32(hdr[12:16]) {
		return nil, fmt.Errorf("cold accounting: crc mismatch")
	}
	return body, nil
}

var castagnoliTable = crc32.MakeTable(crc32.Castagnoli)

func appendPKCounts(b []byte, m map[commentPK]int) []byte {
	b = binary.AppendUvarint(b, uint64(len(m)))
	for k, n := range m {
		b = binary.AppendUvarint(b, uint64(len(k.author)))
		b = append(b, k.author...)
		b = binary.AppendUvarint(b, uint64(len(k.permlink)))
		b = append(b, k.permlink...)
		b = binary.AppendUvarint(b, uint64(n))
	}
	return b
}

// accReader decodes a trailer body; the first error sticks.
type accReader struct {
	b   []byte
	err error
}

func (r *accReader) uvarint() uint64 {
	if r.err != nil {
		return 0
	}
	v, n := binary.Uvarint(r.b)
	if n <= 0 {
		r.err = fmt.Errorf("cold accounting: truncated varint")
		return 0
	}
	r.b = r.b[n:]
	return v
}

func (r *accReader) u64() uint64 {
	if r.err != nil {
		return 0
	}
	if len(r.b) < 8 {
		r.err = fmt.Errorf("cold accounting: truncated")
		return 0
	}
	v := binary.LittleEndian.Uint64(r.b)
	r.b = r.b[8:]
	return v
}

func (r *accReader) str() string {
	n := r.uvarint()
	if r.err != nil {
		return ""
	}
	if uint64(len(r.b)) < n {
		r.err = fmt.Errorf("cold accounting: truncated string")
		return ""
	}
	s := string(r.b[:n])
	r.b = r.b[n:]
	return s
}

func (r *accReader) pkCounts() map[commentPK]int {
	n := r.uvarint()
	if r.err != nil {
		return nil
	}
	if n > uint64(len(r.b)) { // every entry is at least 3 bytes
		r.err = fmt.Errorf("cold accounting: implausible map size %d", n)
		return nil
	}
	m := make(map[commentPK]int, n)
	for i := uint64(0); i < n && r.err == nil; i++ {
		a := r.str()
		p := r.str()
		m[commentPK{author: a, permlink: p}] = int(r.uvarint())
	}
	return m
}

func (r *accReader) done() error {
	if r.err != nil {
		return r.err
	}
	if len(r.b) != 0 {
		return fmt.Errorf("cold accounting: %d trailing bytes", len(r.b))
	}
	return nil
}

// marshalColdAccounting writes the comment store's cold bookkeeping.
func (s *CommentStore) marshalColdAccounting(w io.Writer) error {
	hint := 64 + 48*len(s.coldRoot)
	if s.bloom != nil {
		hint += 8 * len(s.bloom.bits)
	}
	return writeColdAccounting(w, hint, func(b []byte) []byte {
		b = binary.LittleEndian.AppendUint64(b, uint64(s.coldTotal))
		if s.bloom == nil {
			b = binary.LittleEndian.AppendUint64(b, 0) // no bloom words
		} else {
			b = binary.LittleEndian.AppendUint64(b, uint64(len(s.bloom.bits)))
			b = binary.LittleEndian.AppendUint64(b, s.bloom.mask)
			b = binary.LittleEndian.AppendUint64(b, uint64(s.bloom.k))
			for _, w := range s.bloom.bits {
				b = binary.LittleEndian.AppendUint64(b, w)
			}
		}
		return appendPKCounts(b, s.coldRoot)
	})
}

// unmarshalColdAccounting restores what marshalColdAccounting wrote.
func (s *CommentStore) unmarshalColdAccounting(r io.Reader) error {
	body, err := readColdAccounting(r)
	if err != nil {
		return fmt.Errorf("comment_store: %w", err)
	}
	ar := &accReader{b: body}
	total := ar.u64()
	nwords := ar.u64()
	var bloom *coldBloom
	if nwords > 0 && ar.err == nil {
		if nwords > uint64(len(ar.b))/8 {
			return fmt.Errorf("comment_store: cold accounting: bloom of %d words overruns the trailer", nwords)
		}
		mask := ar.u64()
		k := ar.u64()
		if mask+1 != nwords*64 || k == 0 || k > 64 {
			return fmt.Errorf("comment_store: cold accounting: inconsistent bloom (words %d mask %#x k %d)", nwords, mask, k)
		}
		bits := make([]uint64, nwords)
		for i := range bits {
			bits[i] = ar.u64()
		}
		bloom = &coldBloom{bits: bits, mask: mask, k: int(k)}
	}
	root := ar.pkCounts()
	if err := ar.done(); err != nil {
		return fmt.Errorf("comment_store: %w", err)
	}
	s.coldTotal = int(total)
	if bloom != nil {
		s.bloom = bloom
	}
	s.coldRoot = root
	return nil
}

// marshalColdAccounting writes the vote store's cold bookkeeping.
func (s *CommentVoteStore) marshalColdAccounting(w io.Writer) error {
	return writeColdAccounting(w, 16+48*len(s.coldCount), func(b []byte) []byte { return appendPKCounts(b, s.coldCount) })
}

// unmarshalColdAccounting restores what marshalColdAccounting wrote.
func (s *CommentVoteStore) unmarshalColdAccounting(r io.Reader) error {
	body, err := readColdAccounting(r)
	if err != nil {
		return fmt.Errorf("comment_vote_store: %w", err)
	}
	ar := &accReader{b: body}
	m := ar.pkCounts()
	if err := ar.done(); err != nil {
		return fmt.Errorf("comment_vote_store: %w", err)
	}
	s.coldCount = m
	return nil
}
