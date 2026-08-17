package api

import (
	"encoding/hex"
	"reflect"
	"strings"

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
	return []interface{}{name, structToMap(val, legacy)}
}

// opToVariant renders the modern appbase form {"type":"transfer_operation","value":{...}}.
func opToVariant(o op.Op, legacy bool) interface{} {
	val := reflect.ValueOf(o.Operation())
	return map[string]interface{}{
		"type":  o.Tag.Name() + "_operation",
		"value": structToMap(val, legacy),
	}
}

func structToMap(v reflect.Value, legacy bool) map[string]interface{} {
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
		out[snakeCase(f.Name)] = convertValue(v.Field(i), legacy)
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
			if v.Len() == 0 {
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
