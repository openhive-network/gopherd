package parallel

// PlanBatches groups txs into ordered batches such that within each
// batch all txs are pairwise non-conflicting. Batches must be applied
// in order; txs within a batch can be applied in any order (and
// concurrently) without changing the deterministic state.
//
// Algorithm: greedy first-fit graph coloring. O(N²) in number of txs
// per block, which is fine — mainnet rarely exceeds ~50 txs per block,
// and even at N=100 the check budget is ~10K AccountSet.Intersects
// calls (each ~50 ns) = 0.5 ms per block — negligible vs the apply
// budget.
//
// Returns the result as colors[i] = batch number for tx i; the caller
// then groups by color preserving in-batch tx order for determinism.
func PlanBatches(touch []TouchSet) (colors []int, numBatches int) {
	colors = make([]int, len(touch))
	if len(touch) == 0 {
		return colors, 0
	}
	// usedColors is a per-iteration bitset of colors already taken by
	// some prior conflicting tx. Reused across iterations to avoid
	// per-iter allocation.
	var usedColors []bool
	for i := range touch {
		// Reset the usedColors buffer to numBatches entries.
		if cap(usedColors) >= numBatches {
			usedColors = usedColors[:numBatches]
			for k := range usedColors {
				usedColors[k] = false
			}
		} else {
			usedColors = make([]bool, numBatches)
		}
		for j := 0; j < i; j++ {
			if touch[i].ConflictsWith(&touch[j]) {
				c := colors[j]
				if c < len(usedColors) {
					usedColors[c] = true
				}
			}
		}
		// Assign the smallest unused color.
		assigned := -1
		for c, used := range usedColors {
			if !used {
				assigned = c
				break
			}
		}
		if assigned == -1 {
			assigned = numBatches
			numBatches++
		}
		colors[i] = assigned
	}
	return colors, numBatches
}

// GroupByBatch turns colors[] into a slice of batches; each batch is
// a slice of tx indexes in their original order. Used by the
// parallel-apply driver to schedule batches sequentially with
// goroutines per batch.
func GroupByBatch(colors []int, numBatches int) [][]int {
	if numBatches == 0 {
		return nil
	}
	out := make([][]int, numBatches)
	for i, c := range colors {
		out[c] = append(out[c], i)
	}
	return out
}
