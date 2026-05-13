package op

import (
	"unsafe"

	"encoding/binary"

	"hived-go/internal/fcraw"
	"hived-go/internal/protocol/asset"
	"hived-go/internal/protocol/auth"
)

// ---- 27 escrow_transfer -------------------------------------------------

type EscrowTransfer struct {
	From, To, Agent      auth.AccountName
	EscrowID             uint32
	HbdAmount            asset.Asset
	HiveAmount           asset.Asset
	Fee                  asset.Asset
	RatificationDeadline uint32
	EscrowExpiration     uint32
	JSONMeta             string
}

func (EscrowTransfer) OpTag() Tag { return TagEscrowTransfer }

// AppendFCBody matches hived's FC_REFLECT field order for
// escrow_transfer_operation (libraries/protocol/include/hive/protocol/
// hive_operations.hpp:1223):
//
//	(from)(to)(hbd_amount)(hive_amount)(escrow_id)(agent)(fee)
//	(json_meta)(ratification_deadline)(escrow_expiration)
//
// The Go struct field order in source is unrelated to the wire order —
// reflected by FC_REFLECT alone.
func (o EscrowTransfer) AppendFCBody(dst []byte) []byte {
	dst = appendString(dst, o.From)
	dst = appendString(dst, o.To)
	dst = asset.AppendAsset(dst, o.HbdAmount)
	dst = asset.AppendAsset(dst, o.HiveAmount)
	dst = binary.LittleEndian.AppendUint32(dst, o.EscrowID)
	dst = appendString(dst, o.Agent)
	dst = asset.AppendAsset(dst, o.Fee)
	dst = appendString(dst, o.JSONMeta)
	dst = binary.LittleEndian.AppendUint32(dst, o.RatificationDeadline)
	return binary.LittleEndian.AppendUint32(dst, o.EscrowExpiration)
}

// readEscrowTransfer mirrors the AppendFCBody order above.
func readEscrowTransfer(r *fcraw.Reader) (Op, error) {
	var o EscrowTransfer
	var err error
	if o.From, err = r.ReadString(); err != nil {
		return Op{}, err
	}
	if o.To, err = r.ReadString(); err != nil {
		return Op{}, err
	}
	if o.HbdAmount, err = asset.ReadAsset(r); err != nil {
		return Op{}, err
	}
	if o.HiveAmount, err = asset.ReadAsset(r); err != nil {
		return Op{}, err
	}
	if o.EscrowID, err = r.ReadUint32(); err != nil {
		return Op{}, err
	}
	if o.Agent, err = r.ReadString(); err != nil {
		return Op{}, err
	}
	if o.Fee, err = asset.ReadAsset(r); err != nil {
		return Op{}, err
	}
	if o.JSONMeta, err = r.ReadString(); err != nil {
		return Op{}, err
	}
	if o.RatificationDeadline, err = r.ReadUint32(); err != nil {
		return Op{}, err
	}
	if o.EscrowExpiration, err = r.ReadUint32(); err != nil {
		return Op{}, err
	}
	return OpFromPtr(TagEscrowTransfer, unsafe.Pointer(&o)), nil
}

// ---- 29 escrow_release --------------------------------------------------

type EscrowRelease struct {
	From, To, Agent, Who, Receiver auth.AccountName
	EscrowID                       uint32
	HbdAmount                      asset.Asset
	HiveAmount                     asset.Asset
}

func (EscrowRelease) OpTag() Tag { return TagEscrowRelease }
func (o EscrowRelease) AppendFCBody(dst []byte) []byte {
	dst = appendString(dst, o.From)
	dst = appendString(dst, o.To)
	dst = appendString(dst, o.Agent)
	dst = appendString(dst, o.Who)
	dst = appendString(dst, o.Receiver)
	dst = binary.LittleEndian.AppendUint32(dst, o.EscrowID)
	dst = asset.AppendAsset(dst, o.HbdAmount)
	return asset.AppendAsset(dst, o.HiveAmount)
}

func readEscrowRelease(r *fcraw.Reader) (Op, error) {
	var o EscrowRelease
	var err error
	for _, dst := range []*string{&o.From, &o.To, &o.Agent, &o.Who, &o.Receiver} {
		if *dst, err = r.ReadString(); err != nil {
			return Op{}, err
		}
	}
	if o.EscrowID, err = r.ReadUint32(); err != nil {
		return Op{}, err
	}
	if o.HbdAmount, err = asset.ReadAsset(r); err != nil {
		return Op{}, err
	}
	if o.HiveAmount, err = asset.ReadAsset(r); err != nil {
		return Op{}, err
	}
	return OpFromPtr(TagEscrowRelease, unsafe.Pointer(&o)), nil
}

func init() {
	register(TagEscrowTransfer, readEscrowTransfer)
	register(TagEscrowRelease, readEscrowRelease)
}

var _ = fcraw.AppendVarUint
