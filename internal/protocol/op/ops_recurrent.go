package op

import (
	"unsafe"

	"encoding/binary"

	"hived-go/internal/fcraw"
	"hived-go/internal/protocol/asset"
	"hived-go/internal/protocol/auth"
)

// ---- 49 recurrent_transfer ----------------------------------------------
//
// extensions has variants:
//
//	tag 0: void_t                       — 0 bytes
//	tag 1: recurrent_transfer_pair_id   — uint8 (1 byte)
var recurrentTransferExtTags = [][2]int{{0, 0}, {1, 1}}

type RecurrentTransfer struct {
	From       auth.AccountName
	To         auth.AccountName
	Amount     asset.Asset
	Memo       string
	Recurrence uint16 // hours
	Executions uint16
	Extensions []byte
}

func (RecurrentTransfer) OpTag() Tag { return TagRecurrentTransfer }
func (o RecurrentTransfer) AppendFCBody(dst []byte) []byte {
	dst = appendString(dst, o.From)
	dst = appendString(dst, o.To)
	dst = asset.AppendAsset(dst, o.Amount)
	dst = appendString(dst, o.Memo)
	dst = binary.LittleEndian.AppendUint16(dst, o.Recurrence)
	dst = binary.LittleEndian.AppendUint16(dst, o.Executions)
	return appendRawExtensions(dst, o.Extensions)
}

func readRecurrentTransfer(r *fcraw.Reader) (Op, error) {
	var o RecurrentTransfer
	var err error
	if o.From, err = r.ReadString(); err != nil {
		return Op{}, err
	}
	if o.To, err = r.ReadString(); err != nil {
		return Op{}, err
	}
	if o.Amount, err = asset.ReadAsset(r); err != nil {
		return Op{}, err
	}
	if o.Memo, err = r.ReadString(); err != nil {
		return Op{}, err
	}
	if o.Recurrence, err = r.ReadUint16(); err != nil {
		return Op{}, err
	}
	if o.Executions, err = r.ReadUint16(); err != nil {
		return Op{}, err
	}
	if o.Extensions, err = readExtensionsRaw(r, "recurrent_transfer.extensions", recurrentTransferExtTags); err != nil {
		return Op{}, err
	}
	return OpFromPtr(TagRecurrentTransfer, unsafe.Pointer(&o)), nil
}

func init() {
	register(TagRecurrentTransfer, readRecurrentTransfer)
}

var _ = fcraw.AppendVarUint
