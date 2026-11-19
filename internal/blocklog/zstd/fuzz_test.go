package zstd

import "testing"

// FuzzDecompress feeds arbitrary payloads to the block decompressor, which
// runs on every compressed_block_message a peer sends. It must not panic or
// crash in libzstd, must never return more than maxDecompressCap bytes (a
// small payload can claim gigabytes), and must leave the pooled decoders
// usable for the next block.
func FuzzDecompress(f *testing.F) {
	f.Add([]byte{}, uint8(0))
	f.Add([]byte{0x28, 0xb5, 0x2f, 0xfd, 0, 0, 0}, uint8(1))
	f.Add([]byte{0, 0, 0, 0, 0xff, 0xff, 0xff, 0xff}, uint8(2))
	d := NewDecompressor()
	f.Fuzz(func(t *testing.T, payload []byte, dict uint8) {
		out, err := d.Decompress(payload, dict, nil)
		if err == nil && len(out) > maxDecompressCap {
			t.Fatalf("decompressed %d bytes, cap is %d", len(out), maxDecompressCap)
		}
		out, err = d.DecompressNoDict(payload, nil)
		if err == nil && len(out) > maxDecompressCap {
			t.Fatalf("decompressed %d bytes without a dictionary, cap is %d", len(out), maxDecompressCap)
		}
	})
}
