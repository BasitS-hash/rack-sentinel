package analyze

import (
	"fmt"
	"math"
	"sort"
	"time"
)

// Thermal analysis thresholds. As with the ECC constants these describe shape,
// not temperature: the level a sensor sits at is a design decision somebody made
// with a fan curve, and only its movement says anything about cooling.
const (
	// minSamplesForThermal is the fewest readings a margin trend may rest on.
	// Higher than the ECC floor because the readings are smoothed before they are
	// fitted, and a bucket median needs several readings under it to be a median
	// of anything.
	minSamplesForThermal = 24

	// minSpanForThermal is the shortest window a margin trend may be claimed
	// over. Much longer than the ECC floor, and for a reason specific to gauges:
	// both load and hall ambient cycle daily, so any window shorter than a day
	// can show half a cycle, which looks exactly like a trend. A day of hourly
	// readings is the thinnest thing this will speak about.
	minSpanForThermal = 24 * time.Hour

	// minShrinkCelsius is how much headroom the fit must account for across the
	// whole window before the slope is called shrinkage. BMCs commonly report
	// whole degrees, so a drift smaller than a few of them is the sensor
	// rounding.
	minShrinkCelsius = 3.0

	// minFitForTrend is the fit below which the slope is not called a trend at
	// all. A line that explains a twentieth of the movement is not describing
	// cooling loss, it is describing where the window happened to start and stop
	// in the load cycle — which is exactly what a quarter-cycle of a daily swing
	// looks like. This is the floor an oscillating sensor is caught by, below
	// which nothing is reported however large the apparent slope.
	minFitForTrend = 0.25

	// minFitForProjection is the fit below which a time-to-critical is not
	// published. The rate still is — a sensor swinging hard with load can be
	// genuinely losing ground — but "crosses critical on Thursday" from a line
	// that did not describe the readings is a number an operator will plan
	// around and then stop trusting.
	minFitForProjection = 0.5

	// urgentHoursToCritical is the projected horizon inside which the sensor is
	// expected to cross its upper critical threshold before any normal
	// maintenance window could be arranged.
	urgentHoursToCritical = 48.0

	// planHoursToCritical is the horizon worth scheduling work for. Two weeks,
	// matching the default retention, because a projection from a window this
	// tool no longer holds is one it cannot re-derive next week.
	planHoursToCritical = 14 * 24.0

	// minReadingsPerBucket, minThermalBuckets and maxThermalBuckets size the
	// smoother. The bucket count follows the reading count so that the fit's
	// resolution does not depend on how often the operator polls; the floor is
	// set where accelerationOf can still split the series in half, and the
	// ceiling keeps the fit cheap on a series at the 2,000-sample cap.
	minReadingsPerBucket = 3
	minThermalBuckets    = 8
	maxThermalBuckets    = 24
)

// ThermalTrend is the outcome of analysing one sensor's margin to its upper
// critical threshold.
type ThermalTrend struct {
	Component string

	// Margin is the most recent reading, in degrees Celsius remaining before the
	// sensor's upper critical threshold. Negative means the threshold is already
	// exceeded.
	Margin float64

	// MarginSmoothed is the median margin of the newest bucket. Projections are
	// made from this rather than from Margin, because one hot reading during a
	// workload spike is a workload spike.
	MarginSmoothed float64

	// ShrinkPerHour is degrees of headroom lost per hour: the least-squares
	// slope of the smoothed series. Positive means the margin is closing.
	ShrinkPerHour float64

	// Acceleration is the recent-half shrink rate divided by the earlier-half
	// rate. Above 1 means the margin is closing faster than it was. Zero means
	// it could not be computed.
	Acceleration float64

	// RSquared is the goodness of fit of the linear model over the smoothed
	// series, 0..1. Low means load is moving the sensor more than cooling loss
	// is, and the slope should not be extrapolated.
	RSquared float64

	// Span is the time covered by the readings.
	Span time.Duration

	// Samples is how many readings the BMC returned. Zero means the sensor was
	// not reported at all, which is not the same as no margin.
	Samples int

	// Buckets is how many smoothed points the fit rests on.
	Buckets int
}

// Trendable reports whether there is enough evidence for ShrinkPerHour and
// Acceleration to mean anything. Output formats that export the numbers rather
// than the prose have to ask the same question AnalyzeThermal asks, or they
// publish a rate it refused to claim.
func (t ThermalTrend) Trendable() bool {
	return t.Samples >= minSamplesForThermal &&
		t.Span >= minSpanForThermal &&
		t.Buckets >= minThermalBuckets
}

// Projectable reports whether the fit is good enough to extrapolate a crossing
// date from. Trendable is about how much data there is; this is about whether a
// straight line described it.
func (t ThermalTrend) Projectable() bool {
	return t.Trendable() && t.RSquared >= minFitForProjection
}

// ShrunkBy is the headroom the fit accounts for across the whole window, in
// degrees. Negative when the margin is recovering.
//
// This, rather than the slope, is what gets compared against the noise floor: a
// slope is only as meaningful as the distance it covers, and 0.01 °C/hour is a
// trend over a month and rounding over an afternoon.
func (t ThermalTrend) ShrunkBy() float64 {
	return t.ShrinkPerHour * t.Span.Hours()
}

// Shrinking reports whether the margin is actually closing, as opposed to
// drifting within the noise a gauge carries anyway.
//
// Three things have to hold, and all three are specific to gauges. The slope has
// to point the dangerous way; it has to cover more ground across the window than
// a sensor reporting whole degrees can produce by rounding; and a line has to
// have described the readings at all, because a slope that explains a twentieth
// of the movement is reporting which part of the load cycle the window happened
// to start in.
//
// Both the grading and the exporter ask this, for the same reason Trendable is
// exported: a ratio or a crossing date published for a sensor the analysis
// declined to speak about is the metrics layer making the claim instead.
func (t ThermalTrend) Shrinking() bool {
	return t.ShrinkPerHour > 0 &&
		t.ShrunkBy() >= minShrinkCelsius &&
		t.RSquared >= minFitForTrend
}

// HoursToCritical projects when the margin reaches zero at the current rate,
// reporting false when the projection is not justified.
func (t ThermalTrend) HoursToCritical() (float64, bool) {
	if !t.Shrinking() || !t.Projectable() || t.MarginSmoothed <= 0 {
		return 0, false
	}
	return t.MarginSmoothed / t.ShrinkPerHour, true
}

// AnalyzeThermal evaluates a sensor's thermal margin for predictive signal.
//
// This is the ECC analysis applied to a second, differently-shaped measurement,
// and the difference is the interesting part. A correctable-error count is a
// monotonic counter whose level is at least suggestive and whose only
// discontinuity is a power-cycle reset. A thermal margin is a gauge: it rises
// and falls with load, ambient and fan response all day, it has no reset
// semantics to find an epoch with, and its level is a design point — the vendor
// chose where the fan curve holds it.
//
// So the inversion is exact. For a counter, high-and-flat is fine and
// low-and-rising is the emergency. For a margin, low-and-flat is fine — a
// sensor that has held 5°C of headroom for a month is doing what it was built to
// do — and high-and-shrinking is the emergency: 40°C of headroom that is now 20
// is a fan that stopped, a dust-blocked heatsink or a dry pump, and it will cross
// the vendor's critical line on a schedule you can read off the slope.
//
// Returns a nil finding when there is nothing worth reporting.
func AnalyzeThermal(host string, s Series) (ThermalTrend, *Finding) {
	trend := ThermalTrend{Component: s.Component, Samples: len(s.Samples)}
	if len(s.Samples) == 0 {
		// The BMC reported nothing for this sensor. Absent is not 0°C of margin —
		// those are opposite conditions, one being "no visibility" and the other
		// "sitting exactly on the critical threshold" — so there is nothing to
		// say and nothing to publish.
		return trend, nil
	}

	trend.Margin = s.Samples[len(s.Samples)-1].Value
	trend.Span = s.Samples[len(s.Samples)-1].At.Sub(s.Samples[0].At)

	shrink := smoothShrink(s.Samples)
	trend.Buckets = len(shrink)
	trend.MarginSmoothed = -shrink[len(shrink)-1].Value

	// A sensor at or past the line the vendor drew is a present fact, not a
	// prediction. It does not wait for a trend and it takes no confidence
	// discount, because nothing is being extrapolated.
	if trend.Margin <= 0 {
		recent, window := pastCritical(s.Samples)
		return trend, &Finding{
			Host:      host,
			Component: s.Component,
			Kind:      "thermal.over-critical",
			Severity:  SeverityUrgent,
			Summary: fmt.Sprintf("%s is %.1f°C past its upper critical threshold now — check airflow before the BMC throttles or trips the node",
				s.Component, -trend.Margin),
			Evidence: []string{
				fmt.Sprintf("latest reading %.1f°C of margin", trend.Margin),
				fmt.Sprintf("%d of the last %d readings at or past the threshold", recent, window),
			},
			Confidence: 1.0,
		}
	}

	if !trend.Trendable() {
		// Deliberately silent, and deliberately unlike AnalyzeECC, which surfaces
		// a thin history as a low-confidence note. It can do that because a
		// non-zero error count is abnormal on its own, so there is something to
		// report while the trend is still forming. A small thermal margin is not
		// abnormal — it is the fan curve — so reporting one here would mean
		// opening the report with every tight sensor in the fleet and saying
		// nothing about any of them.
		return trend, nil
	}

	trend.ShrinkPerHour, trend.RSquared = leastSquaresPerHour(shrink)
	trend.Acceleration = accelerationOf(shrink)

	sev, why := gradeThermal(trend)
	if sev == SeverityInfo {
		return trend, nil
	}

	// Every line has to carry something the summary does not, or an operator
	// learns to skip the block. The smoothed margin and the latest reading are
	// both here because the gap between them is how much load is moving this
	// sensor right now, and the projection is made from the first.
	ev := []string{
		fmt.Sprintf("margin %.1f°C smoothed, %.1f°C latest reading", trend.MarginSmoothed, trend.Margin),
		fmt.Sprintf("%.1f°C of headroom lost over %s (%d readings in %d buckets)",
			trend.ShrunkBy(), trend.Span.Round(time.Minute), trend.Samples, trend.Buckets),
		fmt.Sprintf("fit R²=%.2f", trend.RSquared),
	}
	if trend.Acceleration > 0 {
		ev = append(ev, fmt.Sprintf("recent half closing %.1fx the earlier half", trend.Acceleration))
	}
	if hours, ok := trend.HoursToCritical(); ok {
		ev = append(ev, fmt.Sprintf("crosses upper critical in %s if the rate holds — extrapolated past the last reading, not measured", aboutDuration(hours)))
	} else {
		ev = append(ev, fmt.Sprintf("no crossing projected: fit below R²=%.2f, so load is still moving this sensor as much as cooling loss is", minFitForProjection))
	}

	return trend, &Finding{
		Host:      host,
		Component: s.Component,
		Kind:      "thermal.margin-shrinking",
		Severity:  sev,
		Summary:   why,
		Evidence:  ev,
		Confidence: confidenceFor(fit{
			// Buckets, not readings: the slope rests on the smoothed series, so
			// that is how many points it actually has under it. A gauge has no
			// reset semantics, so the reset penalty never applies here.
			Points: trend.Buckets, RSquared: trend.RSquared, Span: trend.Span,
		}),
	}
}

// gradeThermal maps a margin trend onto a severity and a one-line explanation.
//
// Every branch requires the margin to actually be shrinking by more than the
// noise floor. That is the gauge-specific discipline: acceleration on its own is
// meaningless here, because a sensor whose swings got wider this week has not
// lost any headroom, and a load pattern that changed is not a cooling fault.
func gradeThermal(t ThermalTrend) (Severity, string) {
	if !t.Shrinking() {
		return SeverityInfo, ""
	}
	accelerating := t.Acceleration >= accelerationRatio
	hours, projected := t.HoursToCritical()

	switch {
	case projected && hours <= urgentHoursToCritical:
		return SeverityUrgent, fmt.Sprintf(
			"%s reaches upper critical in %s at %.2f°C/hour — find the airflow fault now",
			t.Component, aboutDuration(hours), t.ShrinkPerHour)

	case projected && accelerating && hours <= planHoursToCritical:
		return SeverityUrgent, fmt.Sprintf(
			"%s losing headroom %.1fx faster than it was, reaching upper critical in %s — find the airflow fault now",
			t.Component, t.Acceleration, aboutDuration(hours))

	case projected && hours <= planHoursToCritical:
		return SeverityPlan, fmt.Sprintf(
			"%s reaches upper critical in %s at %.2f°C/hour — schedule a fan and airflow check",
			t.Component, aboutDuration(hours), t.ShrinkPerHour)

	case accelerating:
		return SeverityPlan, fmt.Sprintf(
			"%s losing headroom %.1fx faster than it was (%.1f°C gone this window) — schedule a fan and airflow check",
			t.Component, t.Acceleration, t.ShrunkBy())

	default:
		return SeverityWatch, fmt.Sprintf(
			"%s losing headroom slowly — %.2f°C/hour, %.1f°C left",
			t.Component, t.ShrinkPerHour, t.MarginSmoothed)
	}
}

// aboutDuration renders a projected horizon the way an operator says it: hours
// inside a couple of days, whole days beyond.
//
// Round numbers only. The projection is a slope extrapolated past the end of the
// data; printing "143h12m" would dress it up as a precision it does not have.
func aboutDuration(hours float64) string {
	if hours < 48 {
		return fmt.Sprintf("~%.0fh", hours)
	}
	return fmt.Sprintf("~%.0fd", math.Round(hours/24))
}

// pastCritical counts how many of the most recent readings sit at or past the
// threshold, and over how many it looked.
//
// One reading over the line could be a workload spike caught mid-poll; eight of
// the last ten is a cooling fault. The finding fires either way — the vendor
// drew that line, not this tool — but an operator deciding whether to walk to
// the rack needs to know which one they have.
func pastCritical(samples []Sample) (count, window int) {
	window = min(len(samples), 10)
	for _, s := range samples[len(samples)-window:] {
		if s.Value <= 0 {
			count++
		}
	}
	return count, window
}

// smoothShrink collapses readings into the median shrinkage of each of a handful
// of equal-duration buckets, newest last.
//
// Two things happen here. First, the sign flips: the series carries shrinkage
// rather than margin, so that leastSquaresPerHour and accelerationOf — shared
// with the ECC analyser — see "bigger is worse" exactly as they do for a counter,
// and "flat then rising" means the same thing in both. Doing the inversion once,
// at this boundary, is why the grading above reads like gradeECC.
//
// Second, the median. A margin dips every time load rises and recovers when it
// falls, so a least-squares fit over raw readings is pulled around by whatever
// the node happened to be doing. A bucket median has to be outvoted inside its
// own bucket before it moves, which discards spikes shorter than half a bucket
// while keeping a sustained change. The alternative — a robust regression over
// every reading — resists the same noise but costs the pairwise slopes of up to
// 2,000 points per sensor, and this tool analyses thousands of series per poll.
//
// What neither choice fixes: a cycle longer than the window still looks like a
// trend. The minimum span covers the daily one.
//
// A non-empty input always yields at least one bucket, which is what lets the
// caller read the newest one unconditionally. That holds however degenerate the
// timestamps are, because the last bucket takes whatever the boundaries left.
func smoothShrink(samples []Sample) []Sample {
	n := len(samples)
	if n == 0 {
		return nil
	}

	buckets := min(max(n/minReadingsPerBucket, 1), maxThermalBuckets)
	span := samples[n-1].At.Sub(samples[0].At)
	if span <= 0 {
		// Every reading shares a timestamp. There is no duration to divide, so
		// the whole set is one bucket.
		buckets = 1
	}

	out := make([]Sample, 0, buckets)
	// One scratch buffer for every bucket's median: sorting needs a copy, and at
	// fleet scale an allocation per bucket is an allocation per sensor per poll.
	// Sized for an evenly-filled bucket rather than for the whole series, which
	// is 24x smaller and the difference between 28MB and 660MB of garbage on a
	// fleet-wide poll; append covers the irregular case.
	buf := make([]float64, 0, n/buckets+minReadingsPerBucket)

	start := 0
	for b := 0; b < buckets && start < n; b++ {
		limit := samples[0].At.Add(time.Duration(b+1) * (span / time.Duration(buckets)))
		end := start
		// The last bucket takes whatever is left, so the newest reading is never
		// dropped by a rounding error in the bucket width.
		for end < n && (b == buckets-1 || samples[end].At.Before(limit)) {
			end++
		}
		if end == start {
			// Empty bucket: polling stopped for a while, or restarted on a
			// different cadence. Skipping it leaves a gap in the fit's x axis,
			// which is correct — least squares reads the timestamps, not the
			// positions.
			continue
		}

		buf = buf[:0]
		for _, s := range samples[start:end] {
			buf = append(buf, s.Value)
		}
		out = append(out, Sample{
			// Stamped at the middle reading of the bucket, which is inside real
			// data rather than at a boundary the polling may never have hit.
			At:    samples[start+(end-start)/2].At,
			Value: -medianOf(buf),
		})
		start = end
	}
	return out
}

// medianOf returns the median of vs, sorting it in place. Callers pass a buffer
// they own.
func medianOf(vs []float64) float64 {
	sort.Float64s(vs)
	n := len(vs)
	if n%2 == 1 {
		return vs[n/2]
	}
	return (vs[n/2-1] + vs[n/2]) / 2
}
