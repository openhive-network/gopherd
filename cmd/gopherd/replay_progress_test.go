package main

import (
	"strings"
	"testing"
	"time"

	"gopherd/internal/protocol/block"
)

func TestGroupDigits(t *testing.T) {
	for v, want := range map[uint64]string{0: "0", 999: "999", 1000: "1,000", 110747311: "110,747,311", 100000: "100,000"} {
		if got := groupDigits(v); got != want {
			t.Errorf("groupDigits(%d) = %q, want %q", v, got, want)
		}
	}
}

func TestReplayProgressLine(t *testing.T) {
	t0 := time.Date(2026, 10, 13, 20, 36, 0, 0, time.UTC)
	p := newReplayProgress(1, 110_000_000, t0)
	sh := block.SignedHeader{Header: block.Header{Timestamp: uint32(time.Date(2017, 11, 2, 0, 0, 0, 0, time.UTC).Unix()), Extensions: []byte{0}}}
	raw := block.MarshalSignedHeader(nil, &sh)

	// 10M blocks in the first 10 minutes, then 1M in the next 10: the "now"
	// rate and the ETA must follow the recent pace, not the average.
	p.line(10_000_000, raw, t0.Add(10*time.Minute))
	got := p.line(11_000_000, raw, t0.Add(20*time.Minute))
	for _, want := range []string{
		"replay: 11,000,000 of 110,000,000 blocks (10.00%)",
		"at 2017-11-02",
		"1,667 blocks/s now",
		"9,167 avg",
		"elapsed 20m00s",
		"~16h30m left at this speed",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("line %q lacks %q", got, want)
		}
	}
	if d := p.done(t0.Add(20 * time.Hour)); !strings.Contains(d, "replay: done — 110,000,000 blocks in 20h00m (1,528 blocks/s avg)") {
		t.Errorf("done line %q", d)
	}
}
