package crypto

import (
	"errors"
	"math/big"
)

// base58Alphabet is the standard Bitcoin/Hive base58 alphabet (no 0/O/I/l).
const base58Alphabet = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"

var (
	bigBase58 = big.NewInt(58)
	bigZero   = big.NewInt(0)

	// base58Decode maps each ASCII byte to its index in base58Alphabet, or 0xff
	// for non-alphabet bytes.
	base58Decode [256]byte
)

func init() {
	for i := range base58Decode {
		base58Decode[i] = 0xff
	}
	for i, c := range []byte(base58Alphabet) {
		base58Decode[c] = byte(i)
	}
}

// Base58Encode encodes data using the Bitcoin base58 alphabet, preserving
// leading zero bytes as leading '1' characters (matches fc::to_base58).
func Base58Encode(data []byte) string {
	leading := 0
	for leading < len(data) && data[leading] == 0 {
		leading++
	}

	x := new(big.Int).SetBytes(data)
	out := make([]byte, 0, len(data)*138/100+1)
	mod := new(big.Int)
	for x.Cmp(bigZero) > 0 {
		x.DivMod(x, bigBase58, mod)
		out = append(out, base58Alphabet[mod.Int64()])
	}
	for i := 0; i < leading; i++ {
		out = append(out, base58Alphabet[0]) // '1'
	}
	// reverse
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return string(out)
}

// errInvalidBase58 is returned for strings containing non-alphabet bytes.
var errInvalidBase58 = errors.New("crypto: invalid base58 character")

// Base58Decode reverses Base58Encode. Returns an error if s contains any
// character outside the alphabet.
func Base58Decode(s string) ([]byte, error) {
	leading := 0
	for leading < len(s) && s[leading] == base58Alphabet[0] {
		leading++
	}

	x := new(big.Int)
	mul := new(big.Int)
	for i := 0; i < len(s); i++ {
		idx := base58Decode[s[i]]
		if idx == 0xff {
			return nil, errInvalidBase58
		}
		x.Mul(x, bigBase58)
		mul.SetUint64(uint64(idx))
		x.Add(x, mul)
	}

	body := x.Bytes()
	out := make([]byte, leading+len(body))
	copy(out[leading:], body)
	return out, nil
}
