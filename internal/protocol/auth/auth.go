// Package auth implements Hive's authority types — account_name_type,
// public_key_type, weight_type, and authority — along with their FC binary
// codecs. See authority.hpp.notes.md for wire-format details.
package auth

import (
	"encoding/binary"
	"fmt"

	"hived-go/internal/crypto"
	"hived-go/internal/fcraw"
)

// AccountName is hive::protocol::account_name_type. It is declared as a
// fixed_string<16> in C++ but serialized identically to std::string —
// varint-prefixed length followed by UTF-8 bytes. We model it as a Go
// string so it composes cleanly with the rest of the type system.
//
// Per the Hive account-name validation rules, valid names are 3–16
// lowercase ASCII characters with hyphens and dots; we don't enforce that
// here because consensus only requires the byte representation to be
// preserved as-is.
type AccountName = string

// PublicKey is hive::protocol::public_key_type — a 33-byte compressed
// secp256k1 key. On the wire it is exactly 33 raw bytes (no length prefix).
type PublicKey = crypto.PublicKey

// Weight is hive::protocol::weight_type (uint16 LE).
type Weight uint16

// AccountAuth is one (account_name, weight) entry in account_auths.
type AccountAuth struct {
	Account AccountName
	Weight  Weight
}

// KeyAuth is one (public_key, weight) entry in key_auths.
type KeyAuth struct {
	Key    PublicKey
	Weight Weight
}

// Authority mirrors hive::protocol::authority. Field order matches the
// FC_REFLECT macro: (weight_threshold)(account_auths)(key_auths).
//
// account_auths and key_auths are flat_map<K, V> in C++, which on the wire
// is a varint-count-prefixed sequence of (key, value) pairs sorted
// ascending by key. We carry them as slices to preserve the on-wire order
// for round-trip fidelity, and provide a helper to verify sort invariants.
type Authority struct {
	WeightThreshold uint32
	AccountAuths    []AccountAuth
	KeyAuths        []KeyAuth
}

// ReadAuthority decodes an authority struct from r.
func ReadAuthority(r *fcraw.Reader) (Authority, error) {
	var a Authority
	wt, err := r.ReadUint32()
	if err != nil {
		return a, fmt.Errorf("auth.ReadAuthority weight_threshold: %w", err)
	}
	a.WeightThreshold = wt

	count, err := r.ReadVarUint()
	if err != nil {
		return a, fmt.Errorf("auth.ReadAuthority account_auths count: %w", err)
	}
	a.AccountAuths = make([]AccountAuth, 0, count)
	for i := uint32(0); i < count; i++ {
		name, err := r.ReadString()
		if err != nil {
			return a, fmt.Errorf("auth.ReadAuthority account_auths[%d] name: %w", i, err)
		}
		w, err := r.ReadUint16()
		if err != nil {
			return a, fmt.Errorf("auth.ReadAuthority account_auths[%d] weight: %w", i, err)
		}
		a.AccountAuths = append(a.AccountAuths, AccountAuth{Account: name, Weight: Weight(w)})
	}

	count, err = r.ReadVarUint()
	if err != nil {
		return a, fmt.Errorf("auth.ReadAuthority key_auths count: %w", err)
	}
	a.KeyAuths = make([]KeyAuth, 0, count)
	for i := uint32(0); i < count; i++ {
		var k PublicKey
		if err := r.ReadInto(k[:]); err != nil {
			return a, fmt.Errorf("auth.ReadAuthority key_auths[%d] key: %w", i, err)
		}
		w, err := r.ReadUint16()
		if err != nil {
			return a, fmt.Errorf("auth.ReadAuthority key_auths[%d] weight: %w", i, err)
		}
		a.KeyAuths = append(a.KeyAuths, KeyAuth{Key: k, Weight: Weight(w)})
	}
	return a, nil
}

// AppendAuthority writes a to dst in FC binary form.
func AppendAuthority(dst []byte, a Authority) []byte {
	dst = binary.LittleEndian.AppendUint32(dst, a.WeightThreshold)
	dst = fcraw.AppendVarUint(dst, uint32(len(a.AccountAuths)))
	for _, ae := range a.AccountAuths {
		dst = fcraw.AppendVarUint(dst, uint32(len(ae.Account)))
		dst = append(dst, ae.Account...)
		dst = binary.LittleEndian.AppendUint16(dst, uint16(ae.Weight))
	}
	dst = fcraw.AppendVarUint(dst, uint32(len(a.KeyAuths)))
	for _, ke := range a.KeyAuths {
		dst = append(dst, ke.Key[:]...)
		dst = binary.LittleEndian.AppendUint16(dst, uint16(ke.Weight))
	}
	return dst
}

// IsSorted reports whether a's account_auths and key_auths are in strictly
// ascending order by key (the invariant flat_map preserves on the wire).
// Useful for validating decoded authorities.
func (a Authority) IsSorted() bool {
	for i := 1; i < len(a.AccountAuths); i++ {
		if a.AccountAuths[i].Account <= a.AccountAuths[i-1].Account {
			return false
		}
	}
	for i := 1; i < len(a.KeyAuths); i++ {
		prev := a.KeyAuths[i-1].Key
		cur := a.KeyAuths[i].Key
		if compareKeys(cur[:], prev[:]) <= 0 {
			return false
		}
	}
	return true
}

// compareKeys is bytes.Compare-equivalent for fixed PublicKey arrays;
// inlined to avoid the bytes-package dep here.
func compareKeys(a, b []byte) int {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] < b[i] {
			return -1
		}
		if a[i] > b[i] {
			return 1
		}
	}
	return len(a) - len(b)
}
