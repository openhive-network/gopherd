package chain

import (
	"testing"

	"gopherd/internal/protocol/block"
)

// A signed header followed by a 2^32-1 transaction count reached
// RecoverBlockTxSignerKeys on the p2p receive goroutine before any validation
// and made it allocate ~200 GiB: a fatal out-of-memory from one block message.
func TestBlockBodyRefusesImpossibleTxCount(t *testing.T) {
	var sh block.SignedHeader
	raw := block.MarshalSignedHeader(nil, &sh)
	hdrLen := len(raw)
	raw = append(raw, 0xff, 0xff, 0xff, 0xff, 0x0f)

	if got := RecoverBlockTxSignerKeys(raw, 1); got.TxKeys != nil {
		t.Fatal("recovered keys for an impossible block")
	}
	if err := validateMerkleRoot(&sh, raw, hdrLen, 1); err == nil {
		t.Fatal("validateMerkleRoot accepted an impossible tx count")
	}
	if ids := BlockTransactionIDs(raw); ids != nil {
		t.Fatal("BlockTransactionIDs returned ids for an impossible block")
	}
	if _, err := BlockTransactionBodies(raw); err == nil {
		t.Fatal("BlockTransactionBodies accepted an impossible tx count")
	}
}
