package api

import "gopherd/internal/protocol/op"

// OpToJSON renders an operation in hived's legacy ["op_name", {fields}] form.
//
// Exported for the testnet load generator, which has to submit transactions as
// JSON (both hived and gopherd take a JSON transaction object, not hex) while
// signing over the binary serialisation. The renderer is reflection-based, so
// it covers every operation without a per-op switch — which is what makes
// simulating the full operation surface tractable.
func OpToJSON(o op.Op, legacy bool) interface{} { return opToJSON(o, legacy) }
