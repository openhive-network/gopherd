package snapshot

import (
	"fmt"
	"io"
	"os"
	"time"
)

// progressReader reports periodic progress while a snapshot subsystem loads.
//
// WHY: comment_store is ~26 GB and takes 60-90s to load. Before this the node
// printed one line AFTER it finished, so a fresh start looked hung for over a
// minute — no output, no indication it was working, no way to tell a slow disk
// from a wedged process. Startup is also the moment an operator is most likely
// to be watching, and the least likely to have anything else to look at.
//
// Only files big enough to be slow get progress (progressMinBytes); small
// subsystems still print just their one completion line, so a normal start is
// not drowned in noise.
type progressReader struct {
	r     io.Reader
	name  string
	total int64
	read  int64
	start time.Time
	last  time.Time
	shown bool
}

const (
	// progressMinBytes: below this a load finishes fast enough that progress
	// lines would be pure noise.
	progressMinBytes = 512 << 20
	// progressEvery paces the lines. 5s over a 90s load is ~18 lines: enough
	// to show life and let you estimate the finish, few enough to stay
	// readable in a log file.
	progressEvery = 5 * time.Second
	// progressGrace suppresses the first report so a load that turns out to be
	// quick prints nothing extra.
	progressGrace = 3 * time.Second
)

func newProgressReader(r io.Reader, name string, total int64) io.Reader {
	if total < progressMinBytes {
		return r
	}
	now := time.Now()
	return &progressReader{r: r, name: name, total: total, start: now, last: now}
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	p.read += int64(n)
	now := time.Now()
	if now.Sub(p.last) >= progressEvery && now.Sub(p.start) >= progressGrace {
		p.last = now
		p.report(now)
	}
	return n, err
}

func (p *progressReader) report(now time.Time) {
	el := now.Sub(p.start).Seconds()
	if el <= 0 {
		return
	}
	mbps := float64(p.read) / (1 << 20) / el
	pct := float64(p.read) * 100 / float64(p.total)
	// ETA from the rate so far. Honest about being an estimate: early in a
	// load the rate is still climbing (page cache, decompressor warm-up), so
	// the first few readings run pessimistic.
	eta := "?"
	if mbps > 0 {
		remain := float64(p.total-p.read) / (1 << 20) / mbps
		if remain >= 0 {
			eta = "~" + HumanDuration(time.Duration(remain*float64(time.Second))) + " left"
		}
	}
	fmt.Fprintf(os.Stderr, "snapshot: loading %s — %.0f%% (%.1f/%.1f GB, %.0f MB/s, %s)\n",
		p.name, pct,
		float64(p.read)/(1<<30), float64(p.total)/(1<<30), mbps, eta)
	p.shown = true
}

// HumanDuration renders a load/save time the way a person reads it: "42s",
// "6m21s", "1h10m". Seconds are dropped once the duration passes an hour,
// where they are noise next to the estimate's own error.
func HumanDuration(d time.Duration) string {
	d = d.Round(time.Second)
	h := d / time.Hour
	m := (d % time.Hour) / time.Minute
	s := (d % time.Minute) / time.Second
	switch {
	case h > 0:
		return fmt.Sprintf("%dh%02dm", h, m)
	case m > 0:
		return fmt.Sprintf("%dm%02ds", m, s)
	default:
		return fmt.Sprintf("%ds", s)
	}
}
