package analyze

import "time"

// Prune drops samples older than cutoff, keeping the ones the verdict rests on.
//
// Retention has to be a window on this analysis rather than a blind cut on the
// file, because the analysis reads shape, not levels. Two positions in a series
// are load-bearing regardless of their age:
//
//   - The sample on the high side of the latest counter reset. currentEpoch
//     identifies the epoch by looking for the counter going backwards; drop that
//     sample and the drop disappears with it, the reset count that discounts
//     confidence falls to zero, and the evidence line explaining the shortened
//     span goes too.
//
//   - Enough of the epoch to stay trendable. Cutting a long history down to two
//     points converts a real finding into a 20%-confidence insufficient-history
//     note — the tool contradicting itself between runs over data it chose to
//     delete.
//
// So the cut is the earliest of the three candidate positions, never the one age
// alone would pick.
func Prune(samples []Sample, cutoff time.Time) []Sample {
	if len(samples) == 0 {
		return samples
	}

	cut := 0
	for cut < len(samples) && samples[cut].At.Before(cutoff) {
		cut++
	}
	if cut == 0 {
		return samples
	}

	if start, _ := epochStart(samples); start > 0 && cut > start-1 {
		cut = start - 1
	}
	if floor := trendFloor(samples); cut > floor {
		cut = floor
	}
	return samples[cut:]
}

// trendFloor is the latest index a series may start at and still carry enough
// evidence for AnalyzeECC to claim a rate. Zero when it already does not — a
// series too thin to trend cannot be improved by pruning it further.
func trendFloor(samples []Sample) int {
	last := samples[len(samples)-1].At
	for i := len(samples) - minSamplesForTrend; i > 0; i-- {
		if last.Sub(samples[i].At) >= minSpanForTrend {
			return i
		}
	}
	return 0
}
