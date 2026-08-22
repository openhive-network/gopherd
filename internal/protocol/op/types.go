package op

import (
	"encoding/binary"
	"fmt"

	"gopherd/internal/crypto"
	"gopherd/internal/fcraw"
	"gopherd/internal/protocol/asset"
	"gopherd/internal/protocol/auth"
)

// --- shared field-level helpers ------------------------------------------

// readStringSlice reads a vector<string> as varint count + each entry.
func readStringSlice(r *fcraw.Reader, label string) ([]string, error) {
	count, err := r.ReadVarUint()
	if err != nil {
		return nil, fmt.Errorf("%s count: %w", label, err)
	}
	out := make([]string, 0, count)
	for i := uint32(0); i < count; i++ {
		s, err := r.ReadString()
		if err != nil {
			return nil, fmt.Errorf("%s[%d]: %w", label, i, err)
		}
		out = append(out, s)
	}
	return out, nil
}

func appendStringSlice(dst []byte, v []string) []byte {
	dst = fcraw.AppendVarUint(dst, uint32(len(v)))
	for _, s := range v {
		dst = fcraw.AppendVarUint(dst, uint32(len(s)))
		dst = append(dst, s...)
	}
	return dst
}

// readPubKeySlice reads a vector<public_key_type> (each 33 raw bytes).
func readPubKeySlice(r *fcraw.Reader, label string) ([]crypto.PublicKey, error) {
	count, err := r.ReadVarUint()
	if err != nil {
		return nil, fmt.Errorf("%s count: %w", label, err)
	}
	if err := r.CheckCount(count, 33); err != nil {
		return nil, fmt.Errorf("%s: %w", label, err)
	}
	out := make([]crypto.PublicKey, count)
	for i := range out {
		if err := r.ReadInto(out[i][:]); err != nil {
			return nil, fmt.Errorf("%s[%d]: %w", label, i, err)
		}
	}
	return out, nil
}

func appendPubKeySlice(dst []byte, v []crypto.PublicKey) []byte {
	dst = fcraw.AppendVarUint(dst, uint32(len(v)))
	for _, k := range v {
		dst = append(dst, k[:]...)
	}
	return dst
}

// readAuthoritySlice reads a vector<authority>.
func readAuthoritySlice(r *fcraw.Reader, label string) ([]auth.Authority, error) {
	count, err := r.ReadVarUint()
	if err != nil {
		return nil, fmt.Errorf("%s count: %w", label, err)
	}
	out := make([]auth.Authority, 0, count)
	for i := uint32(0); i < count; i++ {
		a, err := auth.ReadAuthority(r)
		if err != nil {
			return nil, fmt.Errorf("%s[%d]: %w", label, i, err)
		}
		out = append(out, a)
	}
	return out, nil
}

func appendAuthoritySlice(dst []byte, v []auth.Authority) []byte {
	dst = fcraw.AppendVarUint(dst, uint32(len(v)))
	for _, a := range v {
		dst = auth.AppendAuthority(dst, a)
	}
	return dst
}

// readOptional reads `1 byte present_flag || (if present) value via inner`.
// The inner func is called only when the flag is 1.
func readOptional[T any](r *fcraw.Reader, inner func(*fcraw.Reader) (T, error)) (*T, error) {
	present, err := r.ReadBool()
	if err != nil {
		return nil, err
	}
	if !present {
		return nil, nil
	}
	v, err := inner(r)
	if err != nil {
		return nil, err
	}
	return &v, nil
}

func appendOptional[T any](dst []byte, v *T, inner func([]byte, T) []byte) []byte {
	if v == nil {
		return append(dst, 0)
	}
	dst = append(dst, 1)
	return inner(dst, *v)
}

// readBytes captures a raw vector<char> (varint count + bytes).
func readBytes(r *fcraw.Reader, label string) ([]byte, error) {
	b, err := r.ReadBytes()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", label, err)
	}
	return b, nil
}

// readExtensionsRaw captures a flat_set<future_extensions> as the raw byte
// range it occupies. For nearly all on-chain ops this is the single 0x00
// byte representing an empty set; callers should prefer this approach over
// fully decoding the static_variant variants when only round-trip parity
// is needed.
func readExtensionsRaw(r *fcraw.Reader, label string, maxBodySizes [][2]int) ([]byte, error) {
	start := r.Pos()
	count, err := r.ReadVarUint()
	if err != nil {
		return nil, fmt.Errorf("%s count: %w", label, err)
	}
	for i := uint32(0); i < count; i++ {
		tag, err := r.ReadVarUint()
		if err != nil {
			return nil, fmt.Errorf("%s[%d] tag: %w", label, i, err)
		}
		size, ok := lookupExtensionBodySize(maxBodySizes, int(tag))
		if !ok {
			return nil, fmt.Errorf("%s[%d]: unknown variant tag %d", label, i, tag)
		}
		if size == sizeReadString {
			if _, err := r.ReadString(); err != nil {
				return nil, fmt.Errorf("%s[%d] string body: %w", label, i, err)
			}
		} else if size == sizeReadDelegated {
			return nil, fmt.Errorf("%s[%d]: delegated extension body sizing not yet implemented", label, i)
		} else if size > 0 {
			if _, err := r.ReadRawAlias(size); err != nil {
				return nil, fmt.Errorf("%s[%d] body: %w", label, i, err)
			}
		}
	}
	end := r.Pos()
	return append([]byte(nil), r.SourceRange(start, end)...), nil
}

const (
	sizeReadString    = -1
	sizeReadDelegated = -2
)

// lookupExtensionBodySize finds the wire size for variant tag among a list
// of (tag, size) pairs. Size is -1 for "string body", -2 for "delegated
// (caller must implement)", otherwise byte count.
func lookupExtensionBodySize(table [][2]int, tag int) (int, bool) {
	for _, e := range table {
		if e[0] == tag {
			return e[1], true
		}
	}
	return 0, false
}

// AppendRawExtensions writes captured extension bytes verbatim.
func appendRawExtensions(dst []byte, raw []byte) []byte {
	if len(raw) == 0 {
		return append(dst, 0) // empty set
	}
	return append(dst, raw...)
}

// futureExtensionsTags is the trivial table for the common
// `flat_set<future_extensions>` field — only one variant (void_t, tag 0,
// 0-byte body). Most operations carry such a field.
var futureExtensionsTags = [][2]int{{0, 0}}

// --- common nested types -------------------------------------------------

// LegacyChainProperties mirrors hive::protocol::legacy_chain_properties used
// by witness_update / pow / pow2. Note that account_creation_fee is a
// legacy_hive_asset — six accepted symbol byte sequences exist in mainnet
// history (witnesses at blocks 994240, 1021529, 3143833, 3208405, 3695672,
// 4338089, 4626205, 4632595 set non-canonical encodings). See
// internal/protocol/asset/asset.go for the full list.
type LegacyChainProperties struct {
	AccountCreationFee asset.LegacyHiveAsset
	MaximumBlockSize   uint32
	HBDInterestRate    uint16
}

func readLegacyChainProperties(r *fcraw.Reader) (LegacyChainProperties, error) {
	var p LegacyChainProperties
	a, err := asset.ReadLegacyHiveAsset(r)
	if err != nil {
		return p, fmt.Errorf("legacy_chain_properties.account_creation_fee: %w", err)
	}
	p.AccountCreationFee = a
	if p.MaximumBlockSize, err = r.ReadUint32(); err != nil {
		return p, fmt.Errorf("legacy_chain_properties.maximum_block_size: %w", err)
	}
	if p.HBDInterestRate, err = r.ReadUint16(); err != nil {
		return p, fmt.Errorf("legacy_chain_properties.hbd_interest_rate: %w", err)
	}
	return p, nil
}

func appendLegacyChainProperties(dst []byte, p LegacyChainProperties) []byte {
	dst = asset.AppendLegacyHiveAsset(dst, p.AccountCreationFee)
	dst = binary.LittleEndian.AppendUint32(dst, p.MaximumBlockSize)
	dst = binary.LittleEndian.AppendUint16(dst, p.HBDInterestRate)
	return dst
}
