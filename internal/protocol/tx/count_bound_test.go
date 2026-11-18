package tx

import (
	"strings"
	"testing"

	"gopherd/internal/fcraw"
)

// A tiny transaction declaring 2^32-1 operations used to make the decoder
// reserve ~64 GiB before reading one: a fatal, unrecoverable out-of-memory
// from a single 15-byte p2p trx message. It must now fail as malformed.
func TestReadSignedTransactionRefusesImpossibleOpCount(t *testing.T) {
	raw := []byte{
		0, 0, // ref_block_num
		0, 0, 0, 0, // ref_block_prefix
		0, 0, 0, 0, // expiration
		0xff, 0xff, 0xff, 0xff, 0x0f, // operations count = 4294967295
	}
	_, err := ReadSignedTransaction(fcraw.NewReader(raw))
	if err == nil || !strings.Contains(err.Error(), "tx.operations") {
		t.Fatalf("want an operations-count error, got %v", err)
	}
}

func TestReadBlockTxCount(t *testing.T) {
	huge := []byte{0xff, 0xff, 0xff, 0xff, 0x0f}
	if _, err := ReadBlockTxCount(fcraw.NewReader(huge)); err == nil {
		t.Fatal("2^32-1 transactions in 0 bytes accepted")
	}
	// Two transactions need at least 26 bytes after the count.
	two := append([]byte{2}, make([]byte, 2*MinSignedTxSize-1)...)
	if _, err := ReadBlockTxCount(fcraw.NewReader(two)); err == nil {
		t.Fatal("2 transactions in 25 bytes accepted")
	}
	two = append(two, 0)
	if n, err := ReadBlockTxCount(fcraw.NewReader(two)); err != nil || n != 2 {
		t.Fatalf("2 transactions in 26 bytes: n=%d err=%v", n, err)
	}
}
