package objects

import (
	"errors"
	"fmt"

	"hived-go/internal/fcraw"
	"hived-go/internal/protocol/auth"
	"hived-go/internal/state"
)

// ConvertRequest is the convert_request_object — pending HBD→HIVE
// conversion. Settles after HIVE_CONVERSION_DELAY (3.5 days post-HF16,
// 7 days pre-HF16) via a per-block hook (deferred).
type ConvertRequest struct {
	Owner     auth.AccountName
	RequestID uint32
	Amount    int64  // HBD satoshi
	Conversion uint32 // time_point_sec when settles
}

const ConvertRequestTable state.TableID = 11

// ConvertRequestByDateIndex orders entries by conversion time so the
// per-block process_conversions hook can settle matures in time order.
const ConvertRequestByDateIndex state.IndexID = 1

func convertRequestKey(owner auth.AccountName, requestID uint32) []byte {
	out := state.AppendAccountName(nil, owner)
	return state.AppendUint32BE(out, requestID)
}

func convertRequestByDateKey(conversion uint32, owner auth.AccountName, requestID uint32) []byte {
	out := state.AppendUint32BE(nil, conversion)
	out = state.AppendAccountName(out, owner)
	return state.AppendUint32BE(out, requestID)
}

func AppendConvertRequest(dst []byte, c *ConvertRequest) []byte {
	dst = fcraw.AppendVarUint(dst, uint32(len(c.Owner)))
	dst = append(dst, c.Owner...)
	dst = appendU32LE(dst, c.RequestID)
	dst = appendI64LE(dst, c.Amount)
	dst = appendU32LE(dst, c.Conversion)
	return dst
}

func ReadConvertRequest(src []byte) (ConvertRequest, error) {
	r := fcraw.NewReader(src)
	var c ConvertRequest
	owner, err := r.ReadString()
	if err != nil {
		return c, err
	}
	c.Owner = owner
	if c.RequestID, err = r.ReadUint32(); err != nil {
		return c, err
	}
	if c.Amount, err = r.ReadInt64(); err != nil {
		return c, err
	}
	if c.Conversion, err = r.ReadUint32(); err != nil {
		return c, err
	}
	if !r.AtEnd() {
		return c, errors.New("convert_request: trailing bytes")
	}
	return c, nil
}

func PutConvertRequest(s *state.Session, c *ConvertRequest) {
	s.Put(ConvertRequestTable, state.PrimaryIndex, convertRequestKey(c.Owner, c.RequestID), AppendConvertRequest(nil, c))
	s.Put(ConvertRequestTable, ConvertRequestByDateIndex, convertRequestByDateKey(c.Conversion, c.Owner, c.RequestID), nil)
}

func HasConvertRequest(s *state.Session, owner auth.AccountName, requestID uint32) (bool, error) {
	return s.Has(ConvertRequestTable, state.PrimaryIndex, convertRequestKey(owner, requestID))
}

// GetConvertRequest fetches a pending convert request.
func GetConvertRequest(s *state.Session, owner auth.AccountName, requestID uint32) (ConvertRequest, error) {
	v, err := s.Get(ConvertRequestTable, state.PrimaryIndex, convertRequestKey(owner, requestID))
	if err != nil {
		return ConvertRequest{}, err
	}
	return ReadConvertRequest(v)
}

// DeleteConvertRequestFull removes both the primary entry and the
// by-conversion-date secondary entry.
func DeleteConvertRequestFull(s *state.Session, c *ConvertRequest) {
	s.Delete(ConvertRequestTable, state.PrimaryIndex, convertRequestKey(c.Owner, c.RequestID))
	s.Delete(ConvertRequestTable, ConvertRequestByDateIndex, convertRequestByDateKey(c.Conversion, c.Owner, c.RequestID))
}

// CollateralizedConvert (HF26+) — separate table since the semantics differ
// (collateral up-front, partial refund after).
type CollateralizedConvert struct {
	Owner             auth.AccountName
	RequestID         uint32
	Collateral        int64 // HIVE satoshi escrowed up-front
	HBDOut            int64 // HBD satoshi credited immediately
	Conversion        uint32
}

const CollateralizedConvertTable state.TableID = 12

func collConvertKey(owner auth.AccountName, requestID uint32) []byte {
	out := state.AppendAccountName(nil, owner)
	return state.AppendUint32BE(out, requestID)
}

func AppendCollateralizedConvert(dst []byte, c *CollateralizedConvert) []byte {
	dst = fcraw.AppendVarUint(dst, uint32(len(c.Owner)))
	dst = append(dst, c.Owner...)
	dst = appendU32LE(dst, c.RequestID)
	dst = appendI64LE(dst, c.Collateral)
	dst = appendI64LE(dst, c.HBDOut)
	dst = appendU32LE(dst, c.Conversion)
	return dst
}

func ReadCollateralizedConvert(src []byte) (CollateralizedConvert, error) {
	r := fcraw.NewReader(src)
	var c CollateralizedConvert
	owner, err := r.ReadString()
	if err != nil {
		return c, err
	}
	c.Owner = owner
	if c.RequestID, err = r.ReadUint32(); err != nil {
		return c, err
	}
	if c.Collateral, err = r.ReadInt64(); err != nil {
		return c, err
	}
	if c.HBDOut, err = r.ReadInt64(); err != nil {
		return c, err
	}
	if c.Conversion, err = r.ReadUint32(); err != nil {
		return c, err
	}
	if !r.AtEnd() {
		return c, fmt.Errorf("collateralized_convert: trailing bytes")
	}
	return c, nil
}

func PutCollateralizedConvert(s *state.Session, c *CollateralizedConvert) {
	s.Put(CollateralizedConvertTable, state.PrimaryIndex, collConvertKey(c.Owner, c.RequestID), AppendCollateralizedConvert(nil, c))
}

func HasCollateralizedConvert(s *state.Session, owner auth.AccountName, requestID uint32) (bool, error) {
	return s.Has(CollateralizedConvertTable, state.PrimaryIndex, collConvertKey(owner, requestID))
}
