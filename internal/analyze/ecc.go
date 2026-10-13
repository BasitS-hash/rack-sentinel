package analyze

import (
	"fmt"
	"math"
	"time"
)

// ECC analysis thresholds. These are shape parameters, not error counts — the
// whole point is that absolute counts are a poor predictor on their own.
const (
	// minSamplesForTrend is the fewest points that can support a rate claim.
	// Two points define a line through any noise, so require more.
	minSamplesForTrend = 4

	// minSpanForTrend is the shortest window a rate may be extrapolated from.
	// A burst over ten minutes says nothing about the next week.
	minSpanForTrend = 30 * time.Minute

	// accelerationRatio is how much faster the recent half must be running than
	// the earlier half before the trend counts as accelerating.
	accelerationRatio = 2.0

	// urgentRatePerHour is a sustained correctable-error rate that warrants
	// action regardless of acceleration.
	urgentRatePerHour = 50.0

	// planRatePerHour is a sustained rate worth scheduling a swap for.
	planRatePerHour = 5.0

	// maxAcceleration caps the reported ratio. Going from a flat counter to a
	// rising one is an undefined ratio; clamping keeps the number printable and
	// comparable without changing which bucket it lands in.
	maxAcceleration = 999.0
)

// ECCTrend is the outcome of analysing one correctable-error counter.
type ECCTrend struct {
	Component string

	// RatePerHour is the least-squares slope over the current epoch.
	RatePerHour float64

	// Acceleration is the recent-half rate divided by the earlier-half rate.
	// Values above 1 mean the component is degrading faster than it was.
	// Zero means it could not be computed.
	Acceleration float64

	// RSquared is the goodness of fit of the linear model, 0..1. A low value
	// means the counter is bursty and the rate should not be trusted far.
	RSquared float64

	// Total is the counter value at the end of the current epoch.
	Total float64

	// Span is the time covered by the current epoch.
	Span time.Duration

	// Resets counts how many times the counter went backwards, which happens on
	// host power-cycle. Frequent resets make long-run rates meaningless.
	Resets int

	// Samples in the current epoch.
	Samples int
}

// Trendable reports whether the epoch carries enough evidence for RatePerHour
// and Acceleration to mean anything. Output formats that export the numbers
// rather than the prose have to ask the same question AnalyzeECC asks, or they
// publish a rate it refused to claim.
func (t ECCTrend) Trendable() bool {
	return t.Samples >= minSamplesForTrend && t.Span >= minSpanForTrend
}

// AnalyzeECC evaluates a correctable-error counter for predictive signal.
//
// Correctable errors are, by definition, errors the memory subsystem handled.
// They matter because a DIMM developing a hard fault — a stuck bit, a failing
// row, a degrading contact — produces them at a rising rate before it produces
// an uncorrectable one that halts the host. So the signal being looked for is
// the derivative, not the level.
//
// Returns a nil finding when there is nothing worth reporting.
func AnalyzeECC(host string, s Series) (ECCTrend, *Finding) {
	epoch, resets := currentEpoch(s.Samples)
	trend := ECCTrend{
		Component: s.Component,
		Resets:    resets,
		Samples:   len(epoch),
	}
	if len(epoch) == 0 {
		return trend, nil
	}

	trend.Total = epoch[len(epoch)-1].Value
	trend.Span = epoch[len(epoch)-1].At.Sub(epoch[0].At)

	// A counter that has never moved is the common case and is not a finding.
	if trend.Total == 0 {
		return trend, nil
	}

	if !trend.Trendable() {
		// Not enough evidence to claim a rate. Say so rather than inventing one:
		// a scary-looking extrapolation from three points in five minutes is how
		// operators learn to ignore a tool.
		return trend, &Finding{
			Host:       host,
			Component:  s.Component,
			Kind:       "ecc.insufficient-history",
			Severity:   SeverityWatch,
			Summary:    fmt.Sprintf("%s has %.0f correctable errors but too little history to trend", s.Component, trend.Total),
			Evidence:   []string{fmt.Sprintf("%d samples over %s (need %d over %s)", len(epoch), trend.Span.Round(time.Minute), minSamplesForTrend, minSpanForTrend)},
			Confidence: 0.2,
		}
	}

	slope, r2 := leastSquaresPerHour(epoch)
	trend.RatePerHour = slope
	trend.RSquared = r2
	trend.Acceleration = accelerationOf(epoch)

	sev, why := gradeECC(trend)
	if sev == SeverityInfo {
		return trend, nil
	}

	ev := []string{
		fmt.Sprintf("rate %.1f errors/hour over %s (%d samples)", trend.RatePerHour, trend.Span.Round(time.Minute), trend.Samples),
		fmt.Sprintf("total %.0f correctable errors this epoch", trend.Total),
		fmt.Sprintf("fit R²=%.2f", trend.RSquared),
	}
	if trend.Acceleration > 0 {
		ev = append(ev, fmt.Sprintf("recent half running %.1fx the earlier half", trend.Acceleration))
	}
	if trend.Resets > 0 {
		ev = append(ev, fmt.Sprintf("%d counter reset(s) seen — rate covers the latest epoch only", trend.Resets))
	}

	return trend, &Finding{
		Host:      host,
		Component: s.Component,
		Kind:      "ecc.degrading",
		Severity:  sev,
		Summary:   why,
		Evidence:  ev,
		Confidence: confidenceFor(fit{
			Points: trend.Samples, RSquared: trend.RSquared,
			Resets: trend.Resets, Span: trend.Span,
		}),
	}
}

// gradeECC maps a trend onto a severity and a one-line explanation.
//
// Acceleration outranks absolute rate. A part that is getting worse is a
// different problem from a part that is merely bad and stable, and only the
// first one has a deadline.
func gradeECC(t ECCTrend) (Severity, string) {
	accelerating := t.Acceleration >= accelerationRatio

	switch {
	case accelerating && t.RatePerHour >= planRatePerHour:
		return SeverityUrgent, fmt.Sprintf(
			"%s correctable errors accelerating (%.1fx) at %.1f/hour — replace before the next uncorrectable",
			t.Component, t.Acceleration, t.RatePerHour)

	case t.RatePerHour >= urgentRatePerHour:
		return SeverityUrgent, fmt.Sprintf(
			"%s sustaining %.1f correctable errors/hour — replace",
			t.Component, t.RatePerHour)

	case accelerating:
		return SeverityPlan, fmt.Sprintf(
			"%s correctable errors accelerating (%.1fx) from a low base — schedule a swap",
			t.Component, t.Acceleration)

	case t.RatePerHour >= planRatePerHour:
		return SeverityPlan, fmt.Sprintf(
			"%s at %.1f correctable errors/hour — schedule a swap",
			t.Component, t.RatePerHour)

	case t.Total > 0 && t.RatePerHour > 0:
		return SeverityWatch, fmt.Sprintf(
			"%s accumulating correctable errors slowly (%.2f/hour)",
			t.Component, t.RatePerHour)
	}

	// Non-zero total but a flat or falling rate: ECC is doing its job and the
	// part is not degrading. Explicitly not a finding.
	return SeverityInfo, ""
}

// fit is the evidence-quality side of a trend, as opposed to what the trend
// says: how many points the slope rests on, how well a line described them, how
// long they cover, and how many times the series discontinued.
//
// Both analysers produce one and both are scored by confidenceFor below. A
// second confidence scale would mean an operator could not compare a thermal
// finding at 60% with an ECC finding at 80%, which is most of what the number is
// for.
type fit struct {
	Points   int
	RSquared float64
	Resets   int
	Span     time.Duration
}

// confidenceFor scores how much the trend should be believed.
//
// Three things erode confidence: too few points, a poor linear fit (a bursty
// counter, a load-driven gauge), and frequent counter resets.
func confidenceFor(f fit) float64 {
	c := 1.0

	if f.Points < 10 {
		c *= 0.5 + 0.05*float64(f.Points-minSamplesForTrend)
	}
	// A bursty counter still carries signal, so floor the penalty rather than
	// letting a low R² zero it out.
	c *= math.Max(0.4, f.RSquared)

	if f.Resets > 0 {
		c *= math.Max(0.5, 1.0-0.15*float64(f.Resets))
	}
	if f.Span < 6*time.Hour {
		c *= 0.8
	}

	return math.Min(1.0, math.Max(0.05, c))
}

// currentEpoch returns the samples since the last counter reset, plus the number
// of resets observed.
//
// BMC counters restart at zero when the host power-cycles. Treating that drop as
// a negative rate would mask a genuinely degrading part, so the series is split
// and only the most recent epoch is trended.
func currentEpoch(samples []Sample) ([]Sample, int) {
	if len(samples) == 0 {
		return nil, 0
	}
	start, resets := epochStart(samples)
	return samples[start:], resets
}

// epochStart returns the index of the first sample in the latest epoch, plus the
// number of resets seen across the whole series. Retention needs the index on
// its own, and there must be exactly one definition of where an epoch begins.
func epochStart(samples []Sample) (start, resets int) {
	for i := 1; i < len(samples); i++ {
		if samples[i].Value < samples[i-1].Value {
			resets++
			start = i
		}
	}
	return start, resets
}

// leastSquaresPerHour fits value against time and returns the slope in units per
// hour along with the coefficient of determination.
func leastSquaresPerHour(samples []Sample) (slope, rSquared float64) {
	n := float64(len(samples))
	if n < 2 {
		return 0, 0
	}

	t0 := samples[0].At
	var sumX, sumY, sumXY, sumXX float64
	for _, s := range samples {
		x := s.At.Sub(t0).Hours()
		y := s.Value
		sumX += x
		sumY += y
		sumXY += x * y
		sumXX += x * x
	}

	denom := n*sumXX - sumX*sumX
	if denom == 0 {
		// Every sample shares a timestamp; no slope is defined.
		return 0, 0
	}
	slope = (n*sumXY - sumX*sumY) / denom
	intercept := (sumY - slope*sumX) / n

	meanY := sumY / n
	var ssRes, ssTot float64
	for _, s := range samples {
		x := s.At.Sub(t0).Hours()
		pred := intercept + slope*x
		ssRes += (s.Value - pred) * (s.Value - pred)
		ssTot += (s.Value - meanY) * (s.Value - meanY)
	}
	if ssTot == 0 {
		// A perfectly flat series is perfectly explained.
		return slope, 1
	}
	return slope, math.Max(0, 1-ssRes/ssTot)
}

// accelerationOf compares the rate over the recent half of the epoch with the
// rate over the earlier half. Returns 0 when it cannot be computed.
//
// This is what separates "bad and stable" from "getting worse". A part that has
// always thrown a few errors per hour is tolerable; one whose rate doubled this
// week is not, even at a lower absolute count.
func accelerationOf(samples []Sample) float64 {
	if len(samples) < minSamplesForTrend*2 {
		return 0
	}
	mid := len(samples) / 2

	early, _ := leastSquaresPerHour(samples[:mid+1])
	recent, _ := leastSquaresPerHour(samples[mid:])

	if early <= 0 {
		if recent > 0 {
			// Flat-then-rising is the strongest shape there is, but the ratio is
			// undefined. Report a large finite value rather than an infinity,
			// which would render as "+Inf" in an operator-facing evidence line
			// and compares badly downstream.
			return maxAcceleration
		}
		return 0
	}
	return math.Min(recent/early, maxAcceleration)
}
