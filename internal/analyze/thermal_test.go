package analyze

import (
	"math"
	"strings"
	"testing"
	"time"
)

// hourly readings are the thinnest cadence a margin trend is allowed to rest
// on, so the fixtures below reuse the hourly series helpers from ecc_test.go.

// wave builds n hourly margin readings oscillating by amplitude around mean,
// completing one cycle every period hours. This is what a healthy sensor on a
// machine doing real work looks like: margin falls when load rises and recovers
// when it drops.
func wave(component string, mean, amplitude float64, period, n int) Series {
	vals := make([]float64, n)
	for i := range vals {
		vals[i] = mean + amplitude*math.Sin(2*math.Pi*float64(i)/float64(period))
	}
	return series(component, vals...)
}

func TestAnalyzeThermal_StableLowMarginIsNotAFinding(t *testing.T) {
	// The mirror of TestAnalyzeECC_FlatHighCountIsNotUrgent, and the reason this
	// analyser trends the margin rather than reading it. A sensor sitting at 5°C
	// of headroom for a week is where the vendor set the fan curve — a thermal
	// design point, not a failure. A threshold alerter pages someone here.
	s := ramp("CPU1_Inlet", 5, 0, 168)

	trend, f := AnalyzeThermal("node-1", s)

	if trend.ShrinkPerHour != 0 {
		t.Errorf("flat margin should have zero shrink rate, got %.4f", trend.ShrinkPerHour)
	}
	if f != nil {
		t.Fatalf("a stable low margin must not produce a finding, got %s: %s", f.Severity, f.Summary)
	}
}

func TestAnalyzeThermal_ShrinkingHighMarginIsUrgent(t *testing.T) {
	// The other half. 40°C of headroom closing at half a degree an hour is a fan
	// that has stopped, a dust-blocked heatsink or a dry pump, and it crosses the
	// vendor's critical line on a schedule you can read off the slope. Absolute
	// margin says this sensor is the coolest one in the chassis.
	s := ramp("CPU0_Die", 40, -0.5, 48)

	trend, f := AnalyzeThermal("node-1", s)

	if f == nil {
		t.Fatal("a margin shrinking toward critical must produce a finding")
	}
	if f.Severity != SeverityUrgent {
		t.Errorf("severity = %s, want URGENT (shrink %.2f °C/h, margin now %.1f°C)",
			f.Severity, trend.ShrinkPerHour, trend.MarginSmoothed)
	}
	if trend.Margin <= 5 {
		t.Fatalf("guard: this case is meant to still have HIGH margin, got %.1f°C", trend.Margin)
	}
	hours, ok := trend.HoursToCritical()
	if !ok {
		t.Fatal("a clean linear fit must support a time-to-critical projection")
	}
	if !strings.Contains(f.Summary, "critical") {
		t.Errorf("summary should name the projection, got %q", f.Summary)
	}
	if hours <= 0 || hours > urgentHoursToCritical {
		t.Errorf("hours to critical = %.1f, want (0, %.0f]", hours, urgentHoursToCritical)
	}
}

func TestAnalyzeThermal_ShrinkingOutranksStableLowMargin(t *testing.T) {
	// Ranking check, the same inversion the ECC analyser is pinned on: the sensor
	// with less headroom right now must not outrank the one that is losing it.
	stable := ramp("CPU1_Inlet", 4, 0, 168)
	shrinking := ramp("CPU0_Die", 45, -0.4, 72)

	_, stableFinding := AnalyzeThermal("node-1", stable)
	_, shrinkFinding := AnalyzeThermal("node-1", shrinking)

	if shrinkFinding == nil {
		t.Fatal("the shrinking sensor should produce a finding")
	}
	if stableFinding != nil {
		t.Errorf("the stable sensor at 4°C produced %s: %s", stableFinding.Severity, stableFinding.Summary)
	}
}

func TestAnalyzeThermal_LoadOscillationIsNotShrinkage(t *testing.T) {
	// A gauge is noisy in a way a counter is not. Four days of a daily load cycle
	// swinging the margin 8°C either side of a flat mean is the normal operating
	// state of a busy node, and it must be silent. This is what the smoothing and
	// the minimum-shrinkage floor are for.
	s := wave("CPU0_Die", 30, 8, 24, 97)

	trend, f := AnalyzeThermal("node-1", s)

	if f != nil {
		t.Fatalf("load oscillation produced a finding: %s %s (shrink %.3f °C/h, %.2f°C over the window, R²=%.2f)",
			f.Severity, f.Summary, trend.ShrinkPerHour, trend.ShrunkBy(), trend.RSquared)
	}
	// Worth being precise about which floor does the work here, because it is not
	// the obvious one: bucket medians of a sine leave a small residual slope
	// behind — 3.7°C across this window, past the shrinkage floor — and what
	// rejects it is the fit. A line that explains a twentieth of the movement is
	// describing the phase the window started in, not cooling loss.
	if trend.RSquared >= minFitForTrend {
		t.Errorf("fit R²=%.2f over a flat-mean oscillation, want below the %.2f floor",
			trend.RSquared, minFitForTrend)
	}
}

func TestAnalyzeThermal_UnreportedSensorIsAbsentNotZero(t *testing.T) {
	// The project's central invariant, on a gauge. A sensor the BMC stopped
	// reporting has no readings — which is the opposite condition from 0°C of
	// margin, where it is sitting exactly on its critical threshold.
	trend, f := AnalyzeThermal("node-1", Series{Component: "CPU0_Die"})

	if trend.Samples != 0 {
		t.Errorf("samples = %d, want 0 for a sensor with no readings", trend.Samples)
	}
	if f != nil {
		t.Fatalf("an unreported sensor must say nothing, got %s: %s", f.Severity, f.Summary)
	}

	// And the opposite condition is a finding, from the same code path.
	_, atLine := AnalyzeThermal("node-1", ramp("CPU0_Die", 0, 0, 6))
	if atLine == nil {
		t.Fatal("0°C of margin is a sensor on its critical threshold, not a missing reading")
	}
}

func TestAnalyzeThermal_PastCriticalNeedsNoTrend(t *testing.T) {
	// A sensor the BMC reports past its upper critical threshold is a present
	// fact, not a prediction. It does not wait for a trend and it does not get a
	// confidence discount, because nothing is being extrapolated.
	s := series("CPU0_Die", 2, -1, -3)

	trend, f := AnalyzeThermal("node-1", s)

	if f == nil {
		t.Fatal("a margin past upper critical must be reported")
	}
	if f.Severity != SeverityUrgent {
		t.Errorf("severity = %s, want URGENT", f.Severity)
	}
	if f.Kind != "thermal.over-critical" {
		t.Errorf("kind = %q, want thermal.over-critical", f.Kind)
	}
	if f.Confidence != 1.0 {
		t.Errorf("confidence = %.2f, want 1.0 — this is an observation, not a projection", f.Confidence)
	}
	if trend.Trendable() {
		t.Error("guard: this fixture is meant to be too thin to trend")
	}
}

func TestAnalyzeThermal_ThinHistoryDoesNotGuess(t *testing.T) {
	// Deliberately different from AnalyzeECC, which surfaces a thin history as a
	// low-confidence note. A non-zero error count is abnormal on its own; a small
	// thermal margin is not, so there is nothing to surface until the trend is
	// claimable. Alerting here would bury the operator in fan curves.
	s := series("CPU1_Inlet", 6, 6, 5, 6)

	trend, f := AnalyzeThermal("node-1", s)

	if f != nil {
		t.Fatalf("four readings over three hours produced %s: %s", f.Severity, f.Summary)
	}
	if trend.Trendable() {
		t.Error("four readings over three hours must not be trendable")
	}
	if trend.Margin != 6 {
		t.Errorf("margin = %.1f, want the last reported reading (6)", trend.Margin)
	}
}

func TestAnalyzeThermal_AcceleratingShrinkIsRankedUp(t *testing.T) {
	// Acceleration means the same thing here as it does for a counter: a sensor
	// whose margin is closing faster than it was has a deadline. Two days flat at
	// 200°C of headroom, then a day of losing a degree an hour — far from critical
	// in absolute terms, and the thing to look at first.
	flat := make([]float64, 48)
	for i := range flat {
		flat[i] = 200
	}
	falling := make([]float64, 24)
	for i := range falling {
		falling[i] = 200 - float64(i)
	}
	s := series("CPU0_Die", append(flat, falling...)...)

	trend, f := AnalyzeThermal("node-1", s)

	if f == nil {
		t.Fatal("an accelerating margin must produce a finding")
	}
	if trend.Acceleration < accelerationRatio {
		t.Errorf("acceleration = %.2f, want >= %.1f", trend.Acceleration, accelerationRatio)
	}
	if f.Severity < SeverityPlan {
		t.Errorf("severity = %s, want at least PLAN", f.Severity)
	}
	var mentioned bool
	for _, e := range f.Evidence {
		if strings.Contains(e, "earlier half") {
			mentioned = true
		}
	}
	if !mentioned {
		t.Errorf("acceleration is the reason for the ranking and must be in the evidence: %q", f.Evidence)
	}
}

func TestAnalyzeThermal_AWeekOutIsPlanNotUrgent(t *testing.T) {
	// The grading an operator actually wants most of the time: a crossing far
	// enough out to put on a maintenance ticket rather than to walk to the rack
	// for. Losing a tenth of a degree an hour with 20°C left is about eight days.
	s := ramp("CPU0_Die", 30, -0.1, 96)

	trend, f := AnalyzeThermal("node-1", s)

	if f == nil {
		t.Fatal("a margin crossing inside two weeks must produce a finding")
	}
	if f.Severity != SeverityPlan {
		hours, _ := trend.HoursToCritical()
		t.Errorf("severity = %s, want PLAN (%.0fh to critical)", f.Severity, hours)
	}
	if trend.Acceleration >= accelerationRatio {
		t.Errorf("guard: a straight ramp is not accelerating, got %.2fx", trend.Acceleration)
	}
}

func TestAnalyzeThermal_AcceleratingIntoTheWindowIsUrgent(t *testing.T) {
	// Acceleration moves the deadline, which is the whole reason it outranks the
	// level. The same shape as the PLAN case above but with the loss starting
	// partway through: the crossing is still over a week out, and it will not stay
	// there.
	flat := make([]float64, 48)
	for i := range flat {
		flat[i] = 100
	}
	falling := make([]float64, 24)
	for i := range falling {
		falling[i] = 100 - float64(i)
	}
	s := series("CPU0_Die", append(flat, falling...)...)

	trend, f := AnalyzeThermal("node-1", s)

	if f == nil {
		t.Fatal("an accelerating margin inside the window must produce a finding")
	}
	hours, ok := trend.HoursToCritical()
	if !ok {
		t.Fatal("this fixture is meant to be projectable")
	}
	if hours <= urgentHoursToCritical || hours > planHoursToCritical {
		t.Fatalf("guard: %.0fh to critical, want the PLAN horizon (%.0f, %.0f]",
			hours, urgentHoursToCritical, planHoursToCritical)
	}
	if f.Severity != SeverityUrgent {
		t.Errorf("severity = %s, want URGENT — acceleration moves a plan-horizon crossing up", f.Severity)
	}
}

func TestAnalyzeThermal_ProjectsOnlyWhenTheFitJustifiesIt(t *testing.T) {
	// The actionable number is "crosses critical in ~6 days", and it is only
	// honest when a line actually described the readings. A sensor that is
	// genuinely losing headroom while swinging hard with load still gets the
	// rate; it does not get a date.
	clean := ramp("CPU0_Die", 60, -0.1, 240)
	// Ten days of a two-day load cycle swinging 12°C, under a real loss of a
	// degree every ten hours. The headroom is genuinely going; no straight line
	// describes how.
	noisy := wave("CPU1_Die", 90, 12, 48, 241)
	for i := range noisy.Samples {
		noisy.Samples[i].Value -= 0.09 * float64(i)
	}

	cleanTrend, cleanFinding := AnalyzeThermal("node-1", clean)
	noisyTrend, noisyFinding := AnalyzeThermal("node-1", noisy)

	if !cleanTrend.Projectable() {
		t.Errorf("a clean linear fit (R²=%.2f) should be projectable", cleanTrend.RSquared)
	}
	if cleanFinding == nil {
		t.Fatal("a sensor losing headroom should produce a finding")
	}
	if noisyTrend.RSquared >= minFitForProjection {
		t.Fatalf("guard: this fixture is meant to fit badly, got R²=%.2f", noisyTrend.RSquared)
	}
	if noisyTrend.Projectable() {
		t.Error("a poor fit must not be projected from")
	}
	if noisyFinding == nil {
		t.Fatal("a noisy sensor that is still losing headroom should be surfaced")
	}
	if _, ok := noisyTrend.HoursToCritical(); ok {
		t.Error("HoursToCritical must refuse a poor fit")
	}
	for _, e := range noisyFinding.Evidence {
		if strings.Contains(e, "critical in") {
			t.Errorf("a poor fit produced a projection anyway: %q", e)
		}
	}
}

func TestAnalyzeThermal_RecoveringMarginIsSilent(t *testing.T) {
	// A sensor whose headroom is growing — a fan that was replaced, a hall that
	// cooled down — is not a finding in either direction.
	_, f := AnalyzeThermal("node-1", ramp("CPU0_Die", 10, 0.2, 72))
	if f != nil {
		t.Fatalf("a recovering sensor produced %s: %s", f.Severity, f.Summary)
	}
}

func TestAnalyzeThermal_QuantisationIsNotATrend(t *testing.T) {
	// BMCs commonly report whole degrees. A single degree of apparent drift across
	// a whole window is the sensor rounding, not cooling loss.
	s := series("CPU1_Inlet", func() []float64 {
		vals := make([]float64, 72)
		for i := range vals {
			vals[i] = 30
			if i > 48 {
				vals[i] = 29
			}
		}
		return vals
	}()...)

	trend, f := AnalyzeThermal("node-1", s)

	if f != nil {
		t.Fatalf("one degree of drift produced %s: %s (%.2f°C fitted over the window)",
			f.Severity, f.Summary, trend.ShrunkBy())
	}
}

func TestSmoothShrink_BucketMedianIgnoresAWorkloadSpike(t *testing.T) {
	// Why the readings are smoothed before they are fitted: one hot sample during
	// a workload spike is a workload spike. A median has to be outvoted inside its
	// own bucket before it moves, so a spike shorter than half a bucket is gone.
	flat := ramp("CPU0_Die", 30, 0, 48)
	spiked := ramp("CPU0_Die", 30, 0, 48)
	spiked.Samples[24].Value = 1 // one poll caught a burst of real work

	want := smoothShrink(flat.Samples)
	got := smoothShrink(spiked.Samples)

	if len(got) != len(want) {
		t.Fatalf("bucket count changed: %d vs %d", len(got), len(want))
	}
	for i := range want {
		if got[i].Value != want[i].Value {
			t.Errorf("bucket %d moved from %.2f to %.2f — one sample should not shift a median",
				i, -want[i].Value, -got[i].Value)
		}
	}
}

func TestSmoothShrink_InvertsTheSignAndKeepsTheNewestReading(t *testing.T) {
	// The smoothed series carries shrinkage, not margin, so that the shared slope
	// and acceleration helpers see "bigger is worse" exactly as they do for a
	// counter. The sign inversion happens here, once.
	s := ramp("CPU0_Die", 40, -1, 48)

	got := smoothShrink(s.Samples)

	if len(got) < minThermalBuckets {
		t.Fatalf("got %d buckets, want at least %d", len(got), minThermalBuckets)
	}
	if got[0].Value >= got[len(got)-1].Value {
		t.Errorf("shrinkage should rise as margin falls: %.1f -> %.1f", got[0].Value, got[len(got)-1].Value)
	}
	// The newest bucket must cover the newest reading, or a projection is made
	// from a margin the sensor has already moved past.
	newest := s.Samples[len(s.Samples)-1].At
	if last := got[len(got)-1].At; last.After(newest) {
		t.Errorf("newest bucket stamped at %s, after the newest reading %s", last, newest)
	}
}

func TestSmoothShrink_DegenerateInputs(t *testing.T) {
	if got := smoothShrink(nil); got != nil {
		t.Errorf("smoothShrink(nil) = %+v, want nil", got)
	}
	if got := smoothShrink([]Sample{{At: base, Value: 7}}); len(got) != 1 || got[0].Value != -7 {
		t.Errorf("single reading = %+v, want one bucket at -7", got)
	}
	// Every reading shares a timestamp: no span to divide into buckets.
	same := []Sample{{At: base, Value: 3}, {At: base, Value: 9}, {At: base, Value: 6}}
	if got := smoothShrink(same); len(got) != 1 || got[0].Value != -6 {
		t.Errorf("zero-span readings = %+v, want one bucket at the median (-6)", got)
	}

	// The invariant AnalyzeThermal reads the newest bucket on: a non-empty input
	// always produces at least one bucket. Here the span is narrower in
	// nanoseconds than the number of buckets it would be divided into, so the
	// bucket width truncates to zero and every bucket boundary lands on the first
	// reading. No BMC produces that, but a hand-edited state file can, and what
	// keeps it from emptying the result is the last bucket taking the remainder.
	var narrow []Sample
	for i := 0; i < 30; i++ {
		narrow = append(narrow, Sample{At: base.Add(time.Duration(i % 5)), Value: float64(i)})
	}
	got := smoothShrink(narrow)
	if len(got) == 0 {
		t.Fatal("a span narrower than the bucket count produced no buckets at all")
	}
	if len(got) != 1 {
		t.Errorf("got %d buckets from a 4ns span, want 1", len(got))
	}
}

func TestThermalTrend_TrendableNeedsSamplesSpanAndBuckets(t *testing.T) {
	full := ThermalTrend{
		Samples: minSamplesForThermal, Span: minSpanForThermal, Buckets: minThermalBuckets,
	}
	if !full.Trendable() {
		t.Error("the exact floor should be trendable")
	}
	thin := full
	thin.Samples--
	if thin.Trendable() {
		t.Error("one reading below the floor should not be trendable")
	}
	short := full
	short.Span -= time.Minute
	if short.Trendable() {
		t.Error("one minute below the span floor should not be trendable")
	}
	coarse := full
	coarse.Buckets--
	if coarse.Trendable() {
		t.Error("too few buckets to split for acceleration should not be trendable")
	}
}

func BenchmarkAnalyzeThermal(b *testing.B) {
	// A week of one-minute readings, the realistic shape for a polled sensor.
	s := ramp("CPU0_Die", 40, -0.001, 7*24*60)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		AnalyzeThermal("node-1", s)
	}
}
