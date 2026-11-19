package fcraw

import "testing"

// FuzzReader drives a Reader with a script taken from the input: each step
// picks a read by the next byte and runs it on the rest. Every decoder in the
// node is built on these reads, and a peer controls their input, so no
// sequence may panic, move the position backwards or past the end, or hand
// out more bytes than remain.
func FuzzReader(f *testing.F) {
	f.Add([]byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13})
	f.Add([]byte{13, 0xff, 0xff, 0xff, 0xff, 0x0f, 'a'})
	f.Add([]byte{10, 0x80, 0x80, 0x80, 0x80, 0x80, 0x01})
	f.Fuzz(func(t *testing.T, script []byte) {
		r := NewReader(script)
		for steps := 0; r.Remaining() > 0 && steps < 1000; steps++ {
			before := r.Pos()
			sel, _ := r.ReadByte()
			var n int
			var err error
			switch sel % 14 {
			case 0:
				_, err = r.ReadBool()
			case 1:
				_, err = r.ReadUint16()
			case 2:
				_, err = r.ReadUint32()
			case 3:
				_, err = r.ReadUint64()
			case 4:
				_, err = r.ReadVarUint()
			case 5:
				_, err = r.ReadVarInt()
			case 6:
				var b []byte
				b, err = r.ReadBytes()
				n = len(b)
			case 7:
				var b []byte
				b, err = r.ReadBytesAlias()
				n = len(b)
			case 8:
				var s string
				s, err = r.ReadString()
				n = len(s)
			case 9:
				var s string
				s, err = r.ReadStringAlias()
				n = len(s)
			case 10:
				var b []byte
				b, err = r.ReadRaw(int(sel) % 40)
				n = len(b)
			case 11:
				var b []byte
				b, err = r.ReadRawAlias(int(sel) % 40)
				n = len(b)
			case 12:
				err = r.ReadInto(make([]byte, int(sel)%40))
			case 13:
				_, err = r.ReadInt64()
			}
			if r.Pos() < before || r.Pos() > len(script) {
				t.Fatalf("step %d (read %d): position %d, was %d, input %d bytes", steps, sel%14, r.Pos(), before, len(script))
			}
			if n > len(script) {
				t.Fatalf("read %d returned %d bytes from a %d-byte input", sel%14, n, len(script))
			}
			if err != nil {
				return
			}
		}
	})
}

// FuzzVarUint pins that the free-standing varint decoder agrees with the
// Reader's and with AppendVarUint on every input it accepts.
func FuzzVarUint(f *testing.F) {
	f.Add([]byte{0x00})
	f.Add([]byte{0xff, 0xff, 0xff, 0xff, 0x0f})
	f.Add([]byte{0x80, 0x00})
	f.Fuzz(func(t *testing.T, src []byte) {
		v, n, err := ReadVarUint(src, 0)
		rv, rerr := NewReader(src).ReadVarUint()
		if (err == nil) != (rerr == nil) || (err == nil && v != rv) {
			t.Fatalf("ReadVarUint=(%d,%v) Reader.ReadVarUint=(%d,%v) for %x", v, err, rv, rerr, src)
		}
		if err != nil {
			return
		}
		if n <= 0 || n > len(src) {
			t.Fatalf("consumed %d of %d bytes", n, len(src))
		}
		if back, _, err := ReadVarUint(AppendVarUint(nil, v), 0); err != nil || back != v {
			t.Fatalf("AppendVarUint(%d) reads back as (%d, %v)", v, back, err)
		}
	})
}
