package crypto

import (
	"crypto/sha512"
	"encoding/hex"
	"testing"
)

// Verified against fc::city_hash_crc_128 + the IV byte-swap performed by
// libfc's aes_encoder::init for the input sha512("hello"). The C++ reference
// program prints:
//
//	lo=4da0e322de77eecd hi=aab3753a59da3d08
//	iv-bytes=083dda593a75b3aacdee77de22e3a04d
//
// (i.e. bytes[0..8]=hi-LE, bytes[8..16]=lo-LE, mirroring the swap the
// hived AES wrapper does to compensate for fc::uint128's historical layout).
func TestCityHash128MatchesHivedFor_sha512_hello(t *testing.T) {
	digest := sha512.Sum512([]byte("hello"))
	lo, hi := CityHash128(digest[:])
	if lo != 0x4da0e322de77eecd {
		t.Errorf("lo: got %016x want 4da0e322de77eecd", lo)
	}
	if hi != 0xaab3753a59da3d08 {
		t.Errorf("hi: got %016x want aab3753a59da3d08", hi)
	}
	iv := CityHashIVBytes(digest[:])
	want := "083dda593a75b3aacdee77de22e3a04d"
	if got := hex.EncodeToString(iv[:]); got != want {
		t.Errorf("iv: got %s want %s", got, want)
	}
}
