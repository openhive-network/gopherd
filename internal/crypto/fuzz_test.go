package crypto

import "testing"

// FuzzDecodePublicKey parses the STM.../TST... key strings that arrive in API
// calls and JSON operations. It must not panic, and a key it accepts must
// print to a string that decodes to the same key.
func FuzzDecodePublicKey(f *testing.F) {
	f.Add("STM6LLegbAgLAy28EHrffBVuANFWcFgmqRMW13wBmTExqFE9SCkg4")
	f.Add("TST6LLegbAgLAy28EHrffBVuANFWcFgmqRMW13wBmTExqFE9SCkg4")
	f.Add("STM1111111111111111111111111111111114T1Anm")
	f.Add("")
	f.Fuzz(func(t *testing.T, s string) {
		k, err := DecodePublicKey(s)
		if err != nil {
			return
		}
		k2, err := DecodePublicKey(k.String())
		if err != nil || k2 != k {
			t.Fatalf("%q -> %x -> %q -> (%x, %v)", s, k[:], k.String(), k2[:], err)
		}
	})
}

// FuzzDecodeWIF parses private keys in wallet import format, as the CLI and
// witness configuration do. It must not panic, and a key it accepts must
// encode back to a WIF that decodes to the same key.
func FuzzDecodeWIF(f *testing.F) {
	f.Add("5JNHfZYKGaomSFvd4NUdQ9qMcEAC43kujbfjueTHpVapX1Kzq2n")
	f.Add("5KQwrPbwdL6PhXujxW37FSSQZ1JiwsST4cqQzDeyXtP79zkvFD3")
	f.Add("")
	f.Fuzz(func(t *testing.T, s string) {
		k, err := DecodeWIF(s)
		if err != nil {
			return
		}
		w, err := EncodeWIF(k)
		if err != nil {
			t.Fatalf("%q decoded but EncodeWIF fails: %v", s, err)
		}
		k2, err := DecodeWIF(w)
		if err != nil || k2.PublicKey() != k.PublicKey() {
			t.Fatalf("%q -> %q does not decode to the same key (err=%v)", s, w, err)
		}
	})
}
