package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"gopherd/internal/protocol/block"
	snap "gopherd/internal/state/snapshot"
)

// replayProgressEvery is how often a replay reports, in blocks. hived prints
// its "N of M blocks = P%" line every 100,000 blocks
// (chain_plugin.cpp:1286); gopherd follows it. That is a line every few
// seconds through the empty early chain and every couple of minutes through
// the dense years.
const replayProgressEvery = 100_000

// replayRateWindow is how far back the "now" rate and the ETA look. Block
// density changes by an order of magnitude across the chain, so the average
// since the start says little about the next hour; ten minutes of recent
// history tracks the era being replayed without jittering line to line.
const replayRateWindow = 10 * time.Minute

type replaySample struct {
	at  time.Time
	num uint32
}

// replayProgress renders replay status lines in the shape of hived's
//
//	17300000 of 110747311 blocks = 15.62%   (… shared memory free)
//
// plus what an operator waiting on it actually wants: the date of the block
// being replayed, the current and average speed, elapsed time, an ETA at the
// current speed, and resident memory.
type replayProgress struct {
	start, target uint32
	startedAt     time.Time
	samples       []replaySample
}

func newReplayProgress(start, target uint32, now time.Time) *replayProgress {
	return &replayProgress{start: start, target: target, startedAt: now,
		samples: []replaySample{{at: now, num: start - 1}}}
}

// line reports block n, just applied. raw is its serialized form, read only
// for the timestamp.
func (p *replayProgress) line(n uint32, raw []byte, now time.Time) string {
	p.samples = append(p.samples, replaySample{at: now, num: n})
	cut := 0
	for cut < len(p.samples)-2 && now.Sub(p.samples[cut+1].at) >= replayRateWindow {
		cut++
	}
	p.samples = p.samples[cut:]

	done := float64(n-p.start+1) / float64(p.target-p.start+1)
	elapsed := now.Sub(p.startedAt)
	avg := float64(n-p.start+1) / elapsed.Seconds()
	first := p.samples[0]
	recent := avg
	if dt := now.Sub(first.at).Seconds(); dt > 0 && n > first.num {
		recent = float64(n-first.num) / dt
	}

	var b strings.Builder
	fmt.Fprintf(&b, "replay: %s of %s blocks (%.2f%%)", groupDigits(uint64(n)), groupDigits(uint64(p.target)), done*100)
	if sh, _, err := block.UnmarshalSignedHeader(raw); err == nil {
		fmt.Fprintf(&b, ", at %s", time.Unix(int64(sh.Timestamp), 0).UTC().Format("2006-01-02"))
	}
	fmt.Fprintf(&b, " — %s blocks/s now, %s avg — elapsed %s",
		groupDigits(uint64(recent+0.5)), groupDigits(uint64(avg+0.5)), snap.HumanDuration(elapsed))
	if n < p.target && recent > 0 {
		eta := time.Duration(float64(p.target-n) / recent * float64(time.Second))
		fmt.Fprintf(&b, ", ~%s left at this speed", snap.HumanDuration(eta))
	}
	if rss, ok := residentBytes(); ok {
		fmt.Fprintf(&b, " — RSS %.1f GiB", float64(rss)/(1<<30))
	}
	return b.String()
}

// done is the closing line, after hived's "Done reindexing, elapsed time".
func (p *replayProgress) done(now time.Time) string {
	elapsed := now.Sub(p.startedAt)
	n := uint64(p.target - p.start + 1)
	return fmt.Sprintf("replay: done — %s blocks in %s (%s blocks/s avg)",
		groupDigits(n), snap.HumanDuration(elapsed), groupDigits(uint64(float64(n)/elapsed.Seconds()+0.5)))
}

// groupDigits renders 110747311 as "110,747,311".
func groupDigits(v uint64) string {
	s := strconv.FormatUint(v, 10)
	if len(s) <= 3 {
		return s
	}
	var b strings.Builder
	lead := len(s) % 3
	if lead > 0 {
		b.WriteString(s[:lead])
	}
	for i := lead; i < len(s); i += 3 {
		if b.Len() > 0 {
			b.WriteByte(',')
		}
		b.WriteString(s[i : i+3])
	}
	return b.String()
}

// residentBytes reads this process's resident set size from /proc. Best
// effort: off Linux, or if /proc is unreadable, the line simply omits it.
func residentBytes() (uint64, bool) {
	raw, err := os.ReadFile("/proc/self/statm")
	if err != nil {
		return 0, false
	}
	f := strings.Fields(string(raw))
	if len(f) < 2 {
		return 0, false
	}
	pages, err := strconv.ParseUint(f[1], 10, 64)
	if err != nil {
		return 0, false
	}
	return pages * uint64(os.Getpagesize()), true
}
