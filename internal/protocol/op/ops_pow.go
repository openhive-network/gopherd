package op

import (
	"encoding/binary"
	"fmt"

	"hived-go/internal/crypto"
	"hived-go/internal/fcraw"
)

// ---- 14 pow -------------------------------------------------------------
//
// pow_operation embeds a pow struct which has:
//
//	worker:   public_key_type   33 bytes
//	input:    digest_type       32 bytes (sha256)
//	signature: signature_type   65 bytes (compact_signature)
//	work:     digest_type       32 bytes
//
// followed by chain_properties (now legacy_chain_properties).

type PowWork struct {
	Worker    crypto.PublicKey
	Input     [32]byte
	Signature [65]byte
	Work      [32]byte
}

type Pow struct {
	WorkerAccount string
	BlockID       [20]byte
	Nonce         uint64
	Work          PowWork
	Props         LegacyChainProperties
}

func (Pow) OpTag() Tag { return TagPow }
func (o Pow) AppendFCBody(dst []byte) []byte {
	dst = appendString(dst, o.WorkerAccount)
	dst = append(dst, o.BlockID[:]...)
	dst = binary.LittleEndian.AppendUint64(dst, o.Nonce)
	dst = append(dst, o.Work.Worker[:]...)
	dst = append(dst, o.Work.Input[:]...)
	dst = append(dst, o.Work.Signature[:]...)
	dst = append(dst, o.Work.Work[:]...)
	return appendLegacyChainProperties(dst, o.Props)
}

func readPow(r *fcraw.Reader) (Operation, error) {
	var o Pow
	var err error
	if o.WorkerAccount, err = r.ReadString(); err != nil {
		return nil, err
	}
	if err := r.ReadInto(o.BlockID[:]); err != nil {
		return nil, err
	}
	if o.Nonce, err = r.ReadUint64(); err != nil {
		return nil, err
	}
	if err := r.ReadInto(o.Work.Worker[:]); err != nil {
		return nil, err
	}
	if err := r.ReadInto(o.Work.Input[:]); err != nil {
		return nil, err
	}
	if err := r.ReadInto(o.Work.Signature[:]); err != nil {
		return nil, err
	}
	if err := r.ReadInto(o.Work.Work[:]); err != nil {
		return nil, err
	}
	if o.Props, err = readLegacyChainProperties(r); err != nil {
		return nil, err
	}
	return o, nil
}

// ---- 30 pow2 ------------------------------------------------------------
//
// pow2_operation:
//
//	work:           static_variant<pow2, equihash_pow>  (varint tag + body)
//	new_owner_key:  optional<public_key_type>
//	props:          legacy_chain_properties
//
// pow2 variant:
//	input: pow2_input { worker_account: string, prev_block: block_id (20 bytes), nonce: uint64 }
//	pow_summary: uint32
//
// equihash_pow variant: input + 1344-byte equihash proof + prev_block + pow_summary.
// We support tag 0 (pow2) fully; tag 1 (equihash_pow) decoding is best-effort
// (we capture as opaque bytes when encountered) since equihash was used
// briefly post-HF15 only.

type Pow2Input struct {
	WorkerAccount string
	PrevBlock     [20]byte
	Nonce         uint64
}

func appendPow2Input(dst []byte, in Pow2Input) []byte {
	dst = appendString(dst, in.WorkerAccount)
	dst = append(dst, in.PrevBlock[:]...)
	return binary.LittleEndian.AppendUint64(dst, in.Nonce)
}

func readPow2Input(r *fcraw.Reader) (Pow2Input, error) {
	var in Pow2Input
	var err error
	if in.WorkerAccount, err = r.ReadString(); err != nil {
		return in, err
	}
	if err := r.ReadInto(in.PrevBlock[:]); err != nil {
		return in, err
	}
	if in.Nonce, err = r.ReadUint64(); err != nil {
		return in, err
	}
	return in, nil
}

type Pow2 struct {
	// WorkVariantTag is 0 for pow2, 1 for equihash_pow.
	WorkVariantTag uint8
	// PowInput carries the pow2-variant input (when WorkVariantTag == 0).
	PowInput   Pow2Input
	PowSummary uint32
	// EquihashRaw captures the entire equihash_pow body verbatim when
	// WorkVariantTag == 1. Operationally we don't need to inspect it.
	EquihashRaw []byte

	NewOwnerKey *crypto.PublicKey
	Props       LegacyChainProperties
}

func (Pow2) OpTag() Tag { return TagPow2 }
func (o Pow2) AppendFCBody(dst []byte) []byte {
	dst = fcraw.AppendVarUint(dst, uint32(o.WorkVariantTag))
	switch o.WorkVariantTag {
	case 0:
		dst = appendPow2Input(dst, o.PowInput)
		dst = binary.LittleEndian.AppendUint32(dst, o.PowSummary)
	case 1:
		dst = append(dst, o.EquihashRaw...)
	}
	dst = appendOptional(dst, o.NewOwnerKey, func(dst []byte, k crypto.PublicKey) []byte {
		return append(dst, k[:]...)
	})
	return appendLegacyChainProperties(dst, o.Props)
}

func readPow2(r *fcraw.Reader) (Operation, error) {
	var o Pow2
	tag, err := r.ReadVarUint()
	if err != nil {
		return nil, fmt.Errorf("pow2.work tag: %w", err)
	}
	o.WorkVariantTag = uint8(tag)
	switch tag {
	case 0:
		if o.PowInput, err = readPow2Input(r); err != nil {
			return nil, err
		}
		if o.PowSummary, err = r.ReadUint32(); err != nil {
			return nil, err
		}
	case 1:
		// equihash_pow body is variable: input + equihash::proof + prev_block + uint32.
		// equihash::proof contains n,k,seed,inputs which is fairly opaque without
		// implementing the full struct. Until we hit one of these in real data,
		// fail loudly so we know to extend.
		return nil, fmt.Errorf("pow2.work: equihash_pow variant not yet implemented")
	default:
		return nil, fmt.Errorf("pow2.work: unknown variant tag %d", tag)
	}
	if o.NewOwnerKey, err = readOptional(r, func(r *fcraw.Reader) (crypto.PublicKey, error) {
		var k crypto.PublicKey
		if err := r.ReadInto(k[:]); err != nil {
			return k, err
		}
		return k, nil
	}); err != nil {
		return nil, err
	}
	if o.Props, err = readLegacyChainProperties(r); err != nil {
		return nil, err
	}
	return o, nil
}

func init() {
	register(TagPow, readPow)
	register(TagPow2, readPow2)
}
