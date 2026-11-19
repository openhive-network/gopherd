package jsonrpc

import (
	"context"
	"encoding/json"
	"testing"
)

// TestDispatchRecoversHandlerPanic pins that a panicking handler is answered
// with an internal error, reported to the panic hook, and leaves the
// registry serving the next request.
func TestDispatchRecoversHandlerPanic(t *testing.T) {
	r := NewRegistry()
	r.Register("t.boom", func(context.Context, json.RawMessage) (interface{}, error) { panic("boom") })
	r.Register("t.ok", func(context.Context, json.RawMessage) (interface{}, error) { return 1, nil })
	var hooked string
	r.SetPanicHook(func(method string, p any, stack []byte) { hooked = method })

	resp := r.Dispatch(context.Background(), Request{Method: "t.boom"})
	if resp.Error == nil || resp.Error.Code != CodeInternalError {
		t.Fatalf("panicking handler answered %+v, want internal error", resp)
	}
	if hooked != "t.boom" {
		t.Fatalf("panic hook saw %q", hooked)
	}
	if resp := r.Dispatch(context.Background(), Request{Method: "t.ok"}); resp.Error != nil || resp.Result != 1 {
		t.Fatalf("registry broken after a panic: %+v", resp)
	}
}

// TestHandleBatchAndCall pins the envelope forms: a batch answers each
// request, the legacy "call" form is unwrapped, an empty or oversized batch
// and malformed JSON are refused with the matching codes.
func TestHandleBatchAndCall(t *testing.T) {
	r := NewRegistry()
	r.Register("a.echo", func(_ context.Context, p json.RawMessage) (interface{}, error) { return string(p), nil })

	out := r.Handle(context.Background(), []byte(`[{"id":1,"method":"a.echo","params":[1]},{"id":2,"method":"call","params":["a","echo",[2]]}]`))
	batch, ok := out.([]Response)
	if !ok || len(batch) != 2 || batch[0].Result != "[1]" || batch[1].Result != "[2]" {
		t.Fatalf("batch: %+v", out)
	}
	for body, code := range map[string]int{
		`[]`:                       CodeInvalidRequest,
		`{`:                        CodeParseError,
		`{"id":1,"method":"nope"}`: CodeMethodNotFound,
	} {
		resp, ok := r.Handle(context.Background(), []byte(body)).(Response)
		if !ok || resp.Error == nil || resp.Error.Code != code {
			t.Errorf("%s: %+v, want code %d", body, resp, code)
		}
	}
	big := []byte("[")
	for i := 0; i <= maxBatchRequests; i++ {
		if i > 0 {
			big = append(big, ',')
		}
		big = append(big, `{"method":"a.echo"}`...)
	}
	big = append(big, ']')
	if resp, ok := r.Handle(context.Background(), big).(Response); !ok || resp.Error == nil || resp.Error.Code != CodeInvalidRequest {
		t.Errorf("oversized batch: %+v", resp)
	}
}
