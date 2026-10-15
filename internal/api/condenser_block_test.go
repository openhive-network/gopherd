package api

import (
	"encoding/json"
	"testing"
)

// TestCondenserBlockResultsAreBare pins condenser_api's shape: hived returns
// the optional header / block itself (condenser_api.cpp: get_block_header(...)
// .header, get_block(...).block), so a missing block is null, not the
// block_api wrapper {"header":null}.
func TestCondenserBlockResultsAreBare(t *testing.T) {
	srv := compatServer(t)
	for _, c := range []struct{ method, want string }{
		{"condenser_api.get_block_header", `null`},
		{"condenser_api.get_block", `null`},
		{"block_api.get_block_header", `{"header":null}`},
		{"block_api.get_block", `{"block":null}`},
	} {
		params := `[999999]`
		if c.method[0] == 'b' {
			params = `{"block_num":999999}`
		}
		resp := call(t, srv, c.method, params)
		got, _ := json.Marshal(resp.Result)
		if string(got) != c.want {
			t.Errorf("%s = %s, want %s", c.method, got, c.want)
		}
	}
}
