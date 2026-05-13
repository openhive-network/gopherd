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
	EnsureConvertRequestStore(s).Put(c)
}

func HasConvertRequest(s *state.Session, owner auth.AccountName, requestID uint32) (bool, error) {
	return EnsureConvertRequestStore(s).Has(owner, requestID), nil
}

// GetConvertRequest fetches a pending convert request.
func GetConvertRequest(s *state.Session, owner auth.AccountName, requestID uint32) (ConvertRequest, error) {
	c, ok := EnsureConvertRequestStore(s).Get(owner, requestID)
	if !ok {
		return ConvertRequest{}, state.ErrNotFound
	}
	return c, nil
}

// DeleteConvertRequestFull removes the typed-store entry. Kept as the
// "Full" name for backward compatibility with prior call sites that
// expected the K/V two-row delete.
func DeleteConvertRequestFull(s *state.Session, c *ConvertRequest) {
	EnsureConvertRequestStore(s).Delete(c.Owner, c.RequestID)
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

// DeleteCollateralizedConvert removes the row.
func DeleteCollateralizedConvert(s *state.Session, owner auth.AccountName, requestID uint32) {
	s.Delete(CollateralizedConvertTable, state.PrimaryIndex, collConvertKey(owner, requestID))
}

// RangeAllCollateralizedConvert iterates every collateralized_convert
// row in primary-key order. The per-block settlement hook filters by
// conversion_date itself. Outstanding-request count is small in
// practice (≤7-day TTL × users with open requests), so a full scan per
// block is acceptable; a by_conversion_date index can be added if it
// ever becomes hot.
func RangeAllCollateralizedConvert(s *state.Session, fn func(c CollateralizedConvert) bool) error {
	return s.Range(CollateralizedConvertTable, state.PrimaryIndex, nil, func(_, value []byte) (bool, error) {
		c, err := ReadCollateralizedConvert(value)
		if err != nil {
			return false, err
		}
		return fn(c), nil
	})
}
