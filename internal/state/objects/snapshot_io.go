package objects

import (
	"bufio"
	"io"
)

// asByteReader wraps r in a *bufio.Reader if it doesn't already
// implement io.ByteReader. binary.ReadUvarint requires byte-at-a-time
// reads; raw io.Reader (os.File, *bytes.Buffer when used as io.Reader,
// etc.) doesn't satisfy ByteReader on its own.
//
// Used by every typed-store UnmarshalSnapshot. Caller is responsible
// for ensuring the returned reader is consumed in the same goroutine.
func asByteReader(r io.Reader) byteReader {
	if br, ok := r.(byteReader); ok {
		return br
	}
	return bufio.NewReaderSize(r, 1<<20)
}

// byteReader is the union of io.Reader + io.ByteReader (binary.ReadUvarint
// needs ByteReader). Defined here so each typed store can refer to one
// type instead of declaring an inline interface in every UnmarshalSnapshot.
type byteReader interface {
	io.Reader
	io.ByteReader
}
