package asset

import "encoding/binary"

// legacySymBytes builds the 8-byte legacy symbol frame: one precision byte
// followed by the NUL-padded symbol name.
func legacySymBytes(precision byte, name string) []byte {
	b := make([]byte, 8)
	b[0] = precision
	copy(b[1:], name)
	return b
}

// legacyLow4 is the first 4 bytes of a legacy frame, which is what the decoder
// peeks at to decide whether a legacy asset follows.
func legacyLow4(precision byte, name string) uint32 {
	return binary.LittleEndian.Uint32(legacySymBytes(precision, name)[:4])
}
