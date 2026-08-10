package sync

import "testing"

// TestSkipByHeightForkAware pins the fetch gate that kept the node wedged at
// block 108887429 on 2026-08-10.
//
// After the node produced a block the network rejected, it sat on its own
// orphan. Making the produced block reversible (so fork choice COULD undo it)
// was necessary but not sufficient: the inventory path still dismissed offered
// blocks whose embedded height was <= our head, so we never fetched the
// network's real block at that same height. Its child then buffered forever
// waiting for a parent we refused to ask for.
//
// Fork-aware must fetch same-height rivals; linear must not (it can only apply
// head+1, and refetching at-or-below head would also re-pull history during
// catch-up). Both must still skip blocks strictly below head.
func TestSkipByHeightForkAware(t *testing.T) {
	const head = 1000

	cases := []struct {
		name      string
		forkAware bool
		num       uint32
		wantSkip  bool
		why       string
	}{
		{"forkaware/same-height rival", true, head, false,
			"a block at our head height is a COMPETING block; refusing to fetch it makes reorg impossible"},
		{"forkaware/next block", true, head + 1, false,
			"the ordinary forward block must always be fetchable"},
		{"forkaware/below head", true, head - 1, true,
			"deep history must stay skipped or catch-up re-pulls millions of blocks"},
		{"linear/same-height rival", false, head, true,
			"a linear node can only apply head+1, so a same-height block is useless to it"},
		{"linear/next block", false, head + 1, false,
			"the ordinary forward block must always be fetchable"},
		{"linear/below head", false, head - 1, true,
			"deep history must stay skipped"},
		// num==0 means the ID carried no usable height; never skip on height
		// alone, fall through to the ID-based checks.
		{"forkaware/unknown height", true, 0, false, "unknown height must fall through to the ID check"},
		{"linear/unknown height", false, 0, false, "unknown height must fall through to the ID check"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := &Delegate{cfg: Config{ForkAware: tc.forkAware}}
			if got := d.skipByHeight(tc.num, head); got != tc.wantSkip {
				t.Fatalf("skipByHeight(num=%d, head=%d) with ForkAware=%v = %v, want %v — %s",
					tc.num, head, tc.forkAware, got, tc.wantSkip, tc.why)
			}
		})
	}
}
