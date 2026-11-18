package api

import (
	"encoding/json"
	"fmt"
	"strings"

	"gopherd/internal/fcraw"
)

// recurrent_transfer extensions are a flat_set<static_variant<void_t,
// recurrent_transfer_pair_id>>; pair_id is a uint8. They used to go through
// the generic []byte path: rendered as a hex string and parsed only as one,
// so neither hived's legacy [[1,{"pair_id":3}]] nor its
// [{"type":"recurrent_transfer_pair_id","value":{"pair_id":3}}] round-tripped,
// and a recurrent_transfer with a pair_id could not be broadcast through the
// API (hived #813, 6e9ce8d7e, fixed the legacy form on its side).

var recurrentExtNames = []string{"void_t", "recurrent_transfer_pair_id"}

// recurrentTransferExtJSON renders the raw extensions; legacy uses the
// [tag, value] form condenser_api emits.
func recurrentTransferExtJSON(raw []byte, legacy bool) interface{} {
	out := []interface{}{}
	if len(raw) == 0 {
		return out
	}
	r := fcraw.NewReader(raw)
	n, err := r.ReadVarUint()
	if err != nil {
		return out
	}
	for i := uint32(0); i < n; i++ {
		tag, err := r.ReadVarUint()
		if err != nil || int(tag) >= len(recurrentExtNames) {
			return out
		}
		value := map[string]interface{}{}
		if tag == 1 {
			b, err := r.ReadRaw(1)
			if err != nil {
				return out
			}
			value["pair_id"] = b[0]
		}
		if legacy {
			out = append(out, []interface{}{tag, value})
		} else {
			out = append(out, map[string]interface{}{"type": recurrentExtNames[tag], "value": value})
		}
	}
	return out
}

// parseRecurrentTransferExt accepts each extension as [tag|name, value] or
// {"type": name, "value": value} and returns the fc encoding.
func parseRecurrentTransferExt(raw json.RawMessage) ([]byte, error) {
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, fmt.Errorf("extensions: %w", err)
	}
	type ext struct {
		tag    uint32
		pairID uint8
	}
	var exts []ext
	for i, it := range items {
		var tagRaw, valRaw json.RawMessage
		var pair []json.RawMessage
		if json.Unmarshal(it, &pair) == nil && len(pair) == 2 {
			tagRaw, valRaw = pair[0], pair[1]
		} else {
			var tv struct {
				Type  json.RawMessage `json:"type"`
				Value json.RawMessage `json:"value"`
			}
			if err := json.Unmarshal(it, &tv); err != nil || tv.Type == nil {
				return nil, fmt.Errorf("extensions[%d]: want [tag, value] or {type, value}", i)
			}
			tagRaw, valRaw = tv.Type, tv.Value
		}
		tag, err := recurrentExtTag(tagRaw)
		if err != nil {
			return nil, fmt.Errorf("extensions[%d]: %w", i, err)
		}
		e := ext{tag: tag}
		if tag == 1 {
			var v struct {
				PairID *uint8 `json:"pair_id"`
			}
			if err := json.Unmarshal(valRaw, &v); err != nil || v.PairID == nil {
				return nil, fmt.Errorf("extensions[%d]: recurrent_transfer_pair_id needs a pair_id 0..255", i)
			}
			e.pairID = *v.PairID
		}
		exts = append(exts, e)
	}
	out := fcraw.AppendVarUint(nil, uint32(len(exts)))
	for _, e := range exts {
		out = fcraw.AppendVarUint(out, e.tag)
		if e.tag == 1 {
			out = append(out, e.pairID)
		}
	}
	return out, nil
}

func recurrentExtTag(raw json.RawMessage) (uint32, error) {
	var n uint32
	if json.Unmarshal(raw, &n) == nil {
		if int(n) < len(recurrentExtNames) {
			return n, nil
		}
		return 0, fmt.Errorf("unknown extension tag %d", n)
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return 0, fmt.Errorf("extension type: %w", err)
	}
	s = strings.TrimPrefix(s, "hive::protocol::")
	for i, name := range recurrentExtNames {
		if s == name {
			return uint32(i), nil
		}
	}
	return 0, fmt.Errorf("unknown extension type %q", s)
}
