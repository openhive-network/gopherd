package op

import (
	"unsafe"

	"encoding/binary"

	"hived-go/internal/fcraw"
	"hived-go/internal/protocol/asset"
	"hived-go/internal/protocol/auth"
)

// ---- 5 limit_order_create -----------------------------------------------

type LimitOrderCreate struct {
	Owner         auth.AccountName
	OrderID       uint32
	AmountToSell  asset.Asset
	MinToReceive  asset.Asset
	FillOrKill    bool
	Expiration    uint32 // time_point_sec
}

func (LimitOrderCreate) OpTag() Tag { return TagLimitOrderCreate }
func (o LimitOrderCreate) AppendFCBody(dst []byte) []byte {
	dst = appendString(dst, o.Owner)
	dst = binary.LittleEndian.AppendUint32(dst, o.OrderID)
	dst = asset.AppendAsset(dst, o.AmountToSell)
	dst = asset.AppendAsset(dst, o.MinToReceive)
	if o.FillOrKill {
		dst = append(dst, 1)
	} else {
		dst = append(dst, 0)
	}
	return binary.LittleEndian.AppendUint32(dst, o.Expiration)
}

func readLimitOrderCreate(r *fcraw.Reader) (Op, error) {
	var o LimitOrderCreate
	var err error
	if o.Owner, err = r.ReadString(); err != nil {
		return Op{}, err
	}
	if o.OrderID, err = r.ReadUint32(); err != nil {
		return Op{}, err
	}
	if o.AmountToSell, err = asset.ReadAsset(r); err != nil {
		return Op{}, err
	}
	if o.MinToReceive, err = asset.ReadAsset(r); err != nil {
		return Op{}, err
	}
	if o.FillOrKill, err = r.ReadBool(); err != nil {
		return Op{}, err
	}
	if o.Expiration, err = r.ReadUint32(); err != nil {
		return Op{}, err
	}
	return OpFromPtr(TagLimitOrderCreate, unsafe.Pointer(&o)), nil
}

// ---- 21 limit_order_create2 ---------------------------------------------

type LimitOrderCreate2 struct {
	Owner        auth.AccountName
	OrderID      uint32
	AmountToSell asset.Asset
	FillOrKill   bool
	ExchangeRate asset.Price
	Expiration   uint32
}

func (LimitOrderCreate2) OpTag() Tag { return TagLimitOrderCreate2 }
func (o LimitOrderCreate2) AppendFCBody(dst []byte) []byte {
	dst = appendString(dst, o.Owner)
	dst = binary.LittleEndian.AppendUint32(dst, o.OrderID)
	dst = asset.AppendAsset(dst, o.AmountToSell)
	if o.FillOrKill {
		dst = append(dst, 1)
	} else {
		dst = append(dst, 0)
	}
	dst = asset.AppendPrice(dst, o.ExchangeRate)
	return binary.LittleEndian.AppendUint32(dst, o.Expiration)
}

func readLimitOrderCreate2(r *fcraw.Reader) (Op, error) {
	var o LimitOrderCreate2
	var err error
	if o.Owner, err = r.ReadString(); err != nil {
		return Op{}, err
	}
	if o.OrderID, err = r.ReadUint32(); err != nil {
		return Op{}, err
	}
	if o.AmountToSell, err = asset.ReadAsset(r); err != nil {
		return Op{}, err
	}
	if o.FillOrKill, err = r.ReadBool(); err != nil {
		return Op{}, err
	}
	if o.ExchangeRate, err = asset.ReadPrice(r); err != nil {
		return Op{}, err
	}
	if o.Expiration, err = r.ReadUint32(); err != nil {
		return Op{}, err
	}
	return OpFromPtr(TagLimitOrderCreate2, unsafe.Pointer(&o)), nil
}

func init() {
	register(TagLimitOrderCreate, readLimitOrderCreate)
	register(TagLimitOrderCreate2, readLimitOrderCreate2)
}
