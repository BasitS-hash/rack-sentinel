package analyze

import "time"

// PruneCounter drops samples older than cutoff from a monotonic counter series,
// keeping the ones the verdict rests on.
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
func PruneCounter(samples []Sample, cutoff time.Time) []Sample {
	if len(samples) == 0 {
		return samples
	}

	cut := ageCut(samples, cutoff)
	if cut == 0 {
		return samples
	}

	if start, _ := epochStart(samples); start > 0 && cut > start-1 {
		cut = start - 1
	}
	if floor := trendFloor(samples, minSamplesForTrend, minSpanForTrend); cut > floor {
		cut = floor
	}
	return samples[cut:]
}

// PruneGauge drops readings older than cutoff from a gauge series.
//
// This exists because running a gauge through PruneCounter is wrong in both
// directions, and only one of them is obvious.
//
// The floor is the one that bites. PruneCounter protects enough of the series
// for AnalyzeECC to claim a rate — four samples over thirty minutes — which is
// far below what AnalyzeThermal will speak about. A sensor held to the counter's
// floor is pruned down to a handful of readings from this afternoon, goes
// untrendable, and falls silent over data retention chose to delete. That is the
// failure the counter prune was written to prevent, arriving by the same route.
//
// The reset clamp is the quieter one. A gauge has no epochs — every dip in a
// thermal margin is load, not a power-cycle — so there is no boundary to
// protect, and the clamp does nothing most of the time because the last dip sits
// near the newest reading. Where it does fire is a sensor whose BMC went dark
// and came back: it refuses to drop the stale block from before the gap, and the
// fit then spans it. Borrowing a rule because it usually looks harmless is how
// that gets shipped.
func PruneGauge(samples []Sample, cutoff time.Time) []Sample {
	if len(samples) == 0 {
		return samples
	}

	cut := ageCut(samples, cutoff)
	if cut == 0 {
		return samples
	}

	if floor := trendFloor(samples, minSamplesForThermal, minSpanForThermal); cut > floor {
		cut = floor
	}
	return samples[cut:]
}

// PruneWear drops readings older than cutoff from a drive's media-life estimate.
//
// The third series shape in one file, and it is the only one that needs both
// halves of both other rules — which is why it is its own function rather than
// a call to one of them.
//
// It needs the epoch clamp, which PruneGauge does not have. A drive swapped into
// the same bay reports a fresh estimate, and that jump is where the current
// drive's history begins; drop the reading on the worn side of it and the jump
// goes too, the epoch split stops firing, and the fit spans two drives and
// reports the new one as recovering. Unlike a counter the clamp is found on the
// inverted series, because what a replacement does to a declining estimate is
// the opposite of what a power-cycle does to a counter, and the direction is
// decided once in wearOf rather than twice here.
//
// It needs a far higher floor than PruneCounter protects, for the same reason
// PruneGauge does but more so: the estimate steps about once a day on a drive
// that will be spent within the year, so a week of readings is the thinnest
// thing AnalyzeDriveWear will speak about. Held to the counter's four samples
// over thirty minutes, or even to the gauge's day, a drive is pruned down to
// one step and falls silent over data retention chose to delete.
func PruneWear(samples []Sample, cutoff time.Time) []Sample {
	if len(samples) == 0 {
		return samples
	}

	cut := ageCut(samples, cutoff)
	if cut == 0 {
		return samples
	}

	if start, _ := epochStartBeyond(wearOf(samples), minReplacementDrop); start > 0 && cut > start-1 {
		cut = start - 1
	}
	if floor := trendFloor(samples, minSamplesForWear, minSpanForWear); cut > floor {
		cut = floor
	}
	return samples[cut:]
}

// ageCut is the index age alone would cut at: the first sample inside the
// window. Every rule above starts here and then refuses to cut that far.
func ageCut(samples []Sample, cutoff time.Time) int {
	cut := 0
	for cut < len(samples) && samples[cut].At.Before(cutoff) {
		cut++
	}
	return cut
}

// trendFloor is the latest index a series may start at and still carry enough
// evidence for its analyser to claim a rate: points samples covering minSpan.
// Zero when it already does not — a series too thin to trend cannot be improved
// by pruning it further.
//
// The floors are passed in rather than read from the constants because the two
// analysers want different ones, and a gauge held to the counter's floor would
// be pruned below what AnalyzeThermal will speak about.
func trendFloor(samples []Sample, points int, minSpan time.Duration) int {
	last := samples[len(samples)-1].At
	for i := len(samples) - points; i > 0; i-- {
		if last.Sub(samples[i].At) >= minSpan {
			return i
		}
	}
	return 0
}
