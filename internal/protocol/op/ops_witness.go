package op

import (
	"encoding/binary"
	"fmt"

	"hived-go/internal/crypto"
	"hived-go/internal/fcraw"
	"hived-go/internal/protocol/asset"
	"hived-go/internal/protocol/auth"
)

// ---- 11 witness_update --------------------------------------------------

type WitnessUpdate struct {
	Owner           auth.AccountName
	URL             string
	BlockSigningKey crypto.PublicKey
	Props           LegacyChainProperties
	Fee             asset.Asset
}

func (WitnessUpdate) OpTag() Tag { return TagWitnessUpdate }
func (o WitnessUpdate) AppendFCBody(dst []byte) []byte {
	dst = appendString(dst, o.Owner)
	dst = appendString(dst, o.URL)
	dst = append(dst, o.BlockSigningKey[:]...)
	dst = appendLegacyChainProperties(dst, o.Props)
	return asset.AppendAsset(dst, o.Fee)
}

func readWitnessUpdate(r *fcraw.Reader) (Operation, error) {
	var o WitnessUpdate
	var err error
	if o.Owner, err = r.ReadString(); err != nil {
		return nil, err
	}
	if o.URL, err = r.ReadString(); err != nil {
		return nil, err
	}
	if err := r.ReadInto(o.BlockSigningKey[:]); err != nil {
		return nil, err
	}
	if o.Props, err = readLegacyChainProperties(r); err != nil {
		return nil, err
	}
	if o.Fee, err = asset.ReadAsset(r); err != nil {
		return nil, err
	}
	return o, nil
}

// ---- 42 witness_set_properties ------------------------------------------

// WitnessProperty is a single key/value entry inside the witness_set_properties
// flat_map<string, vector<char>>. The value semantics depend on the key
// (e.g. "key" → 33-byte public key, "url" → string bytes, etc.). For
// codec correctness we treat both as opaque byte sequences.
type WitnessProperty struct {
	Key   string
	Value []byte
}

type WitnessSetProperties struct {
	Owner      auth.AccountName
	Props      []WitnessProperty
	Extensions []byte
}

func (WitnessSetProperties) OpTag() Tag { return TagWitnessSetProperties }
func (o WitnessSetProperties) AppendFCBody(dst []byte) []byte {
	dst = appendString(dst, o.Owner)
	dst = fcraw.AppendVarUint(dst, uint32(len(o.Props)))
	for _, p := range o.Props {
		dst = appendString(dst, p.Key)
		dst = fcraw.AppendVarUint(dst, uint32(len(p.Value)))
		dst = append(dst, p.Value...)
	}
	return appendRawExtensions(dst, o.Extensions)
}

func readWitnessSetProperties(r *fcraw.Reader) (Operation, error) {
	var o WitnessSetProperties
	var err error
	if o.Owner, err = r.ReadString(); err != nil {
		return nil, err
	}
	count, err := r.ReadVarUint()
	if err != nil {
		return nil, fmt.Errorf("witness_set_properties.props count: %w", err)
	}
	for i := uint32(0); i < count; i++ {
		k, err := r.ReadString()
		if err != nil {
			return nil, fmt.Errorf("witness_set_properties.props[%d].key: %w", i, err)
		}
		v, err := r.ReadBytes()
		if err != nil {
			return nil, fmt.Errorf("witness_set_properties.props[%d].value: %w", i, err)
		}
		o.Props = append(o.Props, WitnessProperty{Key: k, Value: v})
	}
	if o.Extensions, err = readExtensionsRaw(r, "witness_set_properties.extensions", futureExtensionsTags); err != nil {
		return nil, err
	}
	return o, nil
}

func init() {
	register(TagWitnessUpdate, readWitnessUpdate)
	register(TagWitnessSetProperties, readWitnessSetProperties)
}

// silence unused-import warning if binary helpers aren't used here directly.
var _ = binary.LittleEndian
