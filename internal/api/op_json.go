package api

import (
	"encoding/hex"
	"reflect"
	"strings"
	"time"

	"gopherd/internal/crypto"
	"gopherd/internal/protocol/asset"
	"gopherd/internal/protocol/auth"
	"gopherd/internal/protocol/op"
)

// opToJSON renders one operation in hived's LEGACY form used by condenser /
// block_api: ["op_name", {snake_case fields}]. Field names are the snake_case of
// the Go struct field (From→from, JSONMetadata→json_metadata, RequestID→request_id);
// asset/price/authority/pubkey/byte-array fields get hived's encodings. legacy
// controls asset rendering ("1.000 HIVE" vs NAI object).
func opToJSON(o op.Op, legacy bool) interface{} {
	name := o.Tag.Name()
	val := reflect.ValueOf(o.Operation())
	return []interface{}{name, structToMapNamed(val, legacy, opFieldNameOverrides[o.Tag])}
}

// opToVariant renders the modern appbase form {"type":"transfer_operation","value":{...}}.
func opToVariant(o op.Op, legacy bool) interface{} {
	val := reflect.ValueOf(o.Operation())
	return map[string]interface{}{
		"type":  o.Tag.Name() + "_operation",
		"value": structToMapNamed(val, legacy, opFieldNameOverrides[o.Tag]),
	}
}

// opFieldNameOverrides fixes the operation fields whose JSON names are NOT the
// snake_case of the Go field. hived is inconsistent here and the wire format is
// what it is: the market and convert operations use "orderid"/"requestid" with
// no underscore, while the savings operations use "request_id" with one
// (hive_operations.hpp:1119-1120 vs 1196). A blanket snake_case emitted
// "order_id"/"request_id" everywhere, so hived could not parse the operation at
// all and reported it as a MISSING AUTHORITY rather than a bad field — while
// transfers, which have no such field, worked fine.
var opFieldNameOverrides = map[op.Tag]map[string]string{
	op.TagLimitOrderCreate:      {"OrderID": "orderid"},
	op.TagLimitOrderCreate2:     {"OrderID": "orderid"},
	op.TagLimitOrderCancel:      {"OrderID": "orderid"},
	op.TagConvert:               {"RequestID": "requestid"},
	op.TagCollateralizedConvert: {"RequestID": "requestid"},
}

// opTimeFields lists the operation fields hived types as time_point_sec. They
// are uint32 on the Go side, indistinguishable by reflection from request ids
// or counters, so they are identified by name.
var opTimeFields = map[string]bool{
	"expiration":            true, // limit_order_create / _create2
	"ratification_deadline": true, // escrow_transfer
	"escrow_expiration":     true, // escrow_transfer
	"start_date":            true, // create_proposal
	"end_date":              true, // create_proposal
}

// asUint32 reports an unsigned integer field's value, if that is what it is.
func asUint32(v reflect.Value) (uint32, bool) {
	switch v.Kind() {
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return uint32(v.Uint()), true
	}
	return 0, false
}

func structToMap(v reflect.Value, legacy bool) map[string]interface{} {
	return structToMapNamed(v, legacy, nil)
}

func structToMapNamed(v reflect.Value, legacy bool, overrides map[string]string) map[string]interface{} {
	for v.Kind() == reflect.Ptr {
		if v.IsNil() {
			return nil
		}
		v = v.Elem()
	}
	t := v.Type()
	out := make(map[string]interface{}, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.PkgPath != "" { // unexported
			continue
		}
		name := snakeCase(f.Name)
		if o, ok := overrides[f.Name]; ok {
			name = o
		}
		out[name] = convertValue(v.Field(i), legacy)
		// time_point_sec fields are plain uint32 in Go, so the reflection walk
		// cannot tell them from a counter and rendered them as numbers. hived
		// emits ISO-8601, and feeding it a number fails with "Month number is
		// out of range" — an error about the VALUE that is really about the
		// TYPE. Fix by name; the set of operation fields that are
		// time_point_sec is small and fixed.
		if opTimeFields[name] {
			if u, ok := asUint32(v.Field(i)); ok {
				out[name] = time.Unix(int64(u), 0).UTC().Format("2006-01-02T15:04:05")
			}
		}
	}
	return out
}

func convertValue(v reflect.Value, legacy bool) interface{} {
	// Known concrete types first (interface value).
	switch x := v.Interface().(type) {
	case asset.Asset:
		if legacy {
			return legacyAsset(x)
		}
		return x
	case asset.LegacyHiveAsset:
		// legacy_chain_properties.account_creation_fee is an asset in hived and
		// renders as "0.000 TESTS". Emitting the Go struct produced
		// {"amount":0,"sym":"03535445454d0000"}, which hived rejects with
		// "Invalid cast from object_type to string".
		return asset.Asset{Amount: x.Amount, Symbol: asset.Symbol{Num: asset.HiveSymbol, WasLegacyEncoded: true}}.String()
	case asset.Price:
		return priceResult(x, legacy)
	case auth.Authority:
		return authorityResult(x)
	case *auth.Authority:
		if x == nil {
			return nil
		}
		return authorityResult(*x)
	case crypto.PublicKey:
		return x.String()
	case auth.AccountName:
		return string(x)
	}
	switch v.Kind() {
	case reflect.Ptr:
		if v.IsNil() {
			return nil
		}
		return convertValue(v.Elem(), legacy)
	case reflect.String:
		return v.String()
	case reflect.Array:
		// Fixed byte arrays ([20]/[32]/[33]byte) → hex.
		if v.Type().Elem().Kind() == reflect.Uint8 {
			b := make([]byte, v.Len())
			for i := 0; i < v.Len(); i++ {
				b[i] = byte(v.Index(i).Uint())
			}
			return hex.EncodeToString(b)
		}
		return sliceToJSON(v, legacy)
	case reflect.Slice:
		if v.Type().Elem().Kind() == reflect.Uint8 {
			// []byte (e.g. Extensions) → hex; empty → [].
			//
			// A single 0x00 is the fc encoding of an EMPTY extensions list, not
			// a one-byte blob. Rendering it as the string "00" made hived reject
			// the operation with "Invalid cast from string_type to Array",
			// because extensions is an array in JSON.
			if v.Len() == 0 || (v.Len() == 1 && v.Bytes()[0] == 0x00) {
				return []interface{}{}
			}
			return hex.EncodeToString(v.Bytes())
		}
		return sliceToJSON(v, legacy)
	case reflect.Struct:
		return structToMap(v, legacy)
	default:
		return v.Interface()
	}
}

func sliceToJSON(v reflect.Value, legacy bool) []interface{} {
	out := make([]interface{}, v.Len())
	for i := 0; i < v.Len(); i++ {
		out[i] = convertValue(v.Index(i), legacy)
	}
	return out
}

// snakeCase converts a Go field name to hived snake_case, treating runs of
// capitals as acronyms: JSONMetadata→json_metadata, RequestID→request_id,
// HBDAmount→hbd_amount, URL→url.
func snakeCase(s string) string {
	var b strings.Builder
	rs := []rune(s)
	for i, r := range rs {
		isUpper := r >= 'A' && r <= 'Z'
		if isUpper && i > 0 {
			prev := rs[i-1]
			prevLower := prev >= 'a' && prev <= 'z' || prev >= '0' && prev <= '9'
			nextLower := i+1 < len(rs) && (rs[i+1] >= 'a' && rs[i+1] <= 'z')
			if prevLower || nextLower {
				b.WriteByte('_')
			}
		}
		if isUpper {
			b.WriteRune(r - 'A' + 'a')
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}
