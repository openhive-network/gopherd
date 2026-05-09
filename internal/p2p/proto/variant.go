package proto

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"sort"

	"hived-go/internal/fcraw"
)

// fc::variant wire format
// -------------------------------------------------------------------
// libraries/fc/include/fc/io/raw_variant.hpp serializes an fc::variant as:
//   uint8 tag  +  body
//
//   0  null         (no body)
//   1  int64        (8 bytes LE)
//   2  uint64       (8 bytes LE)
//   3  double       (8 bytes IEEE-754 little-endian)
//   4  bool         (1 byte 0/1)
//   5  string       (varuint length + bytes)
//   6  array        (varuint count + recursive variant entries)
//   7  object       (varuint count + (string key + variant value) pairs;
//                    keys preserve insertion order — the underlying type is
//                    a pair-vector)
//   8  blob         (varuint length + bytes; deprecated; we don't emit, only
//                    accept on read for forward compatibility)
//
// fc::variant_object is encoded as a bare object body — varuint count + pairs —
// without any leading tag byte. (See `pack(Stream&, const variant_object&)`
// in raw_variant.hpp lines 121-130.)
//
// In hived hello messages, user_data is an fc::variant_object. Its values are
// almost always int64 / uint64 / string / sha-256, so we expose typed
// constructors plus generic Set/Get for tests.

const (
	VariantNull   uint8 = 0
	VariantInt64  uint8 = 1
	VariantUint64 uint8 = 2
	VariantDouble uint8 = 3
	VariantBool   uint8 = 4
	VariantString uint8 = 5
	VariantArray  uint8 = 6
	VariantObject uint8 = 7
	VariantBlob   uint8 = 8
)

// Variant is a tagged union mirroring fc::variant. Exactly one of the fields
// matched by Type carries the value; for VariantNull all are zero.
type Variant struct {
	Type   uint8
	Bool   bool
	Int    int64
	Uint   uint64
	Double float64
	Str    string
	Arr    []Variant
	Obj    *VarObject
	Blob   []byte
}

// NewVariantNull returns a null-typed variant.
func NewVariantNull() Variant { return Variant{Type: VariantNull} }

// NewVariantInt64 wraps an int64 value.
func NewVariantInt64(v int64) Variant { return Variant{Type: VariantInt64, Int: v} }

// NewVariantUint64 wraps a uint64 value.
func NewVariantUint64(v uint64) Variant { return Variant{Type: VariantUint64, Uint: v} }

// NewVariantDouble wraps a float64 value.
func NewVariantDouble(v float64) Variant { return Variant{Type: VariantDouble, Double: v} }

// NewVariantBool wraps a bool value.
func NewVariantBool(v bool) Variant { return Variant{Type: VariantBool, Bool: v} }

// NewVariantString wraps a UTF-8 string.
func NewVariantString(v string) Variant { return Variant{Type: VariantString, Str: v} }

// NewVariantArray wraps a slice of variants.
func NewVariantArray(v []Variant) Variant { return Variant{Type: VariantArray, Arr: v} }

// NewVariantObject wraps a VarObject.
func NewVariantObject(v *VarObject) Variant { return Variant{Type: VariantObject, Obj: v} }

// NewVariantBlob wraps a byte blob.
func NewVariantBlob(v []byte) Variant { return Variant{Type: VariantBlob, Blob: v} }

// VarObject mirrors fc::variant_object — an ordered collection of
// (key, value) pairs. Insertion order is preserved on the wire.
type VarObject struct {
	keys   []string
	values []Variant
	index  map[string]int
}

// NewVarObject creates an empty VarObject.
func NewVarObject() *VarObject {
	return &VarObject{index: make(map[string]int)}
}

// Len returns the number of entries.
func (o *VarObject) Len() int { return len(o.keys) }

// Set assigns key→v, replacing any existing entry while keeping its
// original position.
func (o *VarObject) Set(key string, v Variant) {
	if o.index == nil {
		o.index = make(map[string]int)
	}
	if i, ok := o.index[key]; ok {
		o.values[i] = v
		return
	}
	o.index[key] = len(o.keys)
	o.keys = append(o.keys, key)
	o.values = append(o.values, v)
}

// Get returns the value for key, plus a present-flag.
func (o *VarObject) Get(key string) (Variant, bool) {
	if o == nil || o.index == nil {
		return Variant{}, false
	}
	i, ok := o.index[key]
	if !ok {
		return Variant{}, false
	}
	return o.values[i], true
}

// Keys returns the keys in insertion order.
func (o *VarObject) Keys() []string {
	if o == nil {
		return nil
	}
	out := make([]string, len(o.keys))
	copy(out, o.keys)
	return out
}

// Values returns the values in insertion order.
func (o *VarObject) Values() []Variant {
	if o == nil {
		return nil
	}
	out := make([]Variant, len(o.values))
	copy(out, o.values)
	return out
}

// SortedKeys returns the keys sorted lexicographically (used by tests that
// want a stable comparison).
func (o *VarObject) SortedKeys() []string {
	out := o.Keys()
	sort.Strings(out)
	return out
}

// Equal reports deep value equality (key set + per-key variant equality).
func (o *VarObject) Equal(other *VarObject) bool {
	if (o == nil) != (other == nil) {
		return o.Len() == other.Len() // both empty / nil
	}
	if o == nil {
		return true
	}
	if len(o.keys) != len(other.keys) {
		return false
	}
	for i, k := range o.keys {
		if k != other.keys[i] {
			return false
		}
		if !o.values[i].Equal(other.values[i]) {
			return false
		}
	}
	return true
}

// Equal reports value equality between two variants.
func (v Variant) Equal(o Variant) bool {
	if v.Type != o.Type {
		return false
	}
	switch v.Type {
	case VariantNull:
		return true
	case VariantBool:
		return v.Bool == o.Bool
	case VariantInt64:
		return v.Int == o.Int
	case VariantUint64:
		return v.Uint == o.Uint
	case VariantDouble:
		return math.Float64bits(v.Double) == math.Float64bits(o.Double)
	case VariantString:
		return v.Str == o.Str
	case VariantBlob:
		return string(v.Blob) == string(o.Blob)
	case VariantArray:
		if len(v.Arr) != len(o.Arr) {
			return false
		}
		for i := range v.Arr {
			if !v.Arr[i].Equal(o.Arr[i]) {
				return false
			}
		}
		return true
	case VariantObject:
		return v.Obj.Equal(o.Obj)
	}
	return false
}

// AppendVariant marshals one fc::variant to dst.
func AppendVariant(dst []byte, v Variant) []byte {
	dst = append(dst, v.Type)
	switch v.Type {
	case VariantNull:
		// nothing
	case VariantInt64:
		dst = binary.LittleEndian.AppendUint64(dst, uint64(v.Int))
	case VariantUint64:
		dst = binary.LittleEndian.AppendUint64(dst, v.Uint)
	case VariantDouble:
		dst = binary.LittleEndian.AppendUint64(dst, math.Float64bits(v.Double))
	case VariantBool:
		if v.Bool {
			dst = append(dst, 1)
		} else {
			dst = append(dst, 0)
		}
	case VariantString:
		dst = fcraw.AppendVarUint(dst, uint32(len(v.Str)))
		dst = append(dst, v.Str...)
	case VariantArray:
		dst = fcraw.AppendVarUint(dst, uint32(len(v.Arr)))
		for _, e := range v.Arr {
			dst = AppendVariant(dst, e)
		}
	case VariantObject:
		dst = AppendVariantObject(dst, v.Obj)
	case VariantBlob:
		dst = fcraw.AppendVarUint(dst, uint32(len(v.Blob)))
		dst = append(dst, v.Blob...)
	}
	return dst
}

// ReadVariant decodes one fc::variant from r.
func ReadVariant(r *fcraw.Reader) (Variant, error) {
	tag, err := r.ReadByte()
	if err != nil {
		return Variant{}, fmt.Errorf("variant tag: %w", err)
	}
	v := Variant{Type: tag}
	switch tag {
	case VariantNull:
		// nothing
	case VariantInt64:
		x, err := r.ReadUint64()
		if err != nil {
			return v, fmt.Errorf("variant int64: %w", err)
		}
		v.Int = int64(x)
	case VariantUint64:
		x, err := r.ReadUint64()
		if err != nil {
			return v, fmt.Errorf("variant uint64: %w", err)
		}
		v.Uint = x
	case VariantDouble:
		x, err := r.ReadUint64()
		if err != nil {
			return v, fmt.Errorf("variant double: %w", err)
		}
		v.Double = math.Float64frombits(x)
	case VariantBool:
		b, err := r.ReadBool()
		if err != nil {
			return v, fmt.Errorf("variant bool: %w", err)
		}
		v.Bool = b
	case VariantString:
		s, err := r.ReadString()
		if err != nil {
			return v, fmt.Errorf("variant string: %w", err)
		}
		v.Str = s
	case VariantArray:
		count, err := r.ReadVarUint()
		if err != nil {
			return v, fmt.Errorf("variant array count: %w", err)
		}
		v.Arr = make([]Variant, count)
		for i := range v.Arr {
			e, err := ReadVariant(r)
			if err != nil {
				return v, fmt.Errorf("variant array[%d]: %w", i, err)
			}
			v.Arr[i] = e
		}
	case VariantObject:
		o, err := ReadVariantObject(r)
		if err != nil {
			return v, err
		}
		v.Obj = o
	case VariantBlob:
		b, err := r.ReadBytes()
		if err != nil {
			return v, fmt.Errorf("variant blob: %w", err)
		}
		v.Blob = b
	default:
		return v, fmt.Errorf("variant: unknown tag %d", tag)
	}
	return v, nil
}

// AppendVariantObject marshals an fc::variant_object body (no leading
// type tag — the variant_object is self-described as "object" only when
// nested inside an fc::variant).
func AppendVariantObject(dst []byte, o *VarObject) []byte {
	if o == nil {
		dst = fcraw.AppendVarUint(dst, 0)
		return dst
	}
	dst = fcraw.AppendVarUint(dst, uint32(len(o.keys)))
	for i, k := range o.keys {
		dst = fcraw.AppendVarUint(dst, uint32(len(k)))
		dst = append(dst, k...)
		dst = AppendVariant(dst, o.values[i])
	}
	return dst
}

// ReadVariantObject decodes an fc::variant_object body (no leading tag).
func ReadVariantObject(r *fcraw.Reader) (*VarObject, error) {
	count, err := r.ReadVarUint()
	if err != nil {
		return nil, fmt.Errorf("variant_object count: %w", err)
	}
	o := NewVarObject()
	for i := uint32(0); i < count; i++ {
		k, err := r.ReadString()
		if err != nil {
			return nil, fmt.Errorf("variant_object[%d] key: %w", i, err)
		}
		v, err := ReadVariant(r)
		if err != nil {
			return nil, fmt.Errorf("variant_object[%d=%q] value: %w", i, k, err)
		}
		o.Set(k, v)
	}
	return o, nil
}

// ErrVariantTypeMismatch is returned by typed Get* helpers when the variant
// at a key isn't the requested type.
var ErrVariantTypeMismatch = errors.New("proto: variant type mismatch")
