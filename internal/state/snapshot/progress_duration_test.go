package snapshot

import (
	"testing"
	"time"
)

func TestHumanDuration(t *testing.T) {
	for _, c := range []struct {
		d    time.Duration
		want string
	}{
		{400 * time.Millisecond, "0s"},
		{42 * time.Second, "42s"},
		{381 * time.Second, "6m21s"},
		{4200 * time.Second, "1h10m"},
		{3600 * time.Second, "1h00m"},
		{59*time.Minute + 59*time.Second + 600*time.Millisecond, "1h00m"},
	} {
		if got := HumanDuration(c.d); got != c.want {
			t.Errorf("HumanDuration(%v) = %q, want %q", c.d, got, c.want)
		}
	}
}
