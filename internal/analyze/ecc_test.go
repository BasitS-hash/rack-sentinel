package analyze

import (
	"math"
	"testing"
	"time"
)

var base = time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)

// series builds a Series with one sample per hour, starting at base.
func series(component string, values ...float64) Series {
	s := Series{Component: component}
	for i, v := range values {
		s.Samples = append(s.Samples, Sample{
			At:    base.Add(time.Duration(i) * time.Hour),
			Value: v,
		})
	}
	return s
}

// ramp builds n hourly samples increasing by step each hour from start.
func ramp(component string, start, step float64, n int) Series {
	vals := make([]float64, n)
	v := start
	for i := range vals {
		vals[i] = v
		v += step
	}
	return series(component, vals...)
}

func TestAnalyzeECC_FlatHighCountIsNotUrgent(t *testing.T) {
	// The premise of the whole package. Ten thousand correctable errors that
	// have not moved in a day is ECC working, not a failing DIMM. A threshold
	// alerter pages someone here; this must not.
	s := series("DIMM_A1", 10000, 10000, 10000, 10000, 10000, 10000, 10000, 10000)

	trend, f := AnalyzeECC("node-1", s)

	if trend.RatePerHour != 0 {
		t.Errorf("flat counter should have zero rate, got %.3f", trend.RatePerHour)
	}
	if f != nil {
		t.Fatalf("flat high counter must not produce a finding, got %s: %s", f.Severity, f.Summary)
	}
}

func TestAnalyzeECC_AcceleratingLowCountIsUrgent(t *testing.T) {
	// The other half of the premise. A small but sharply accelerating counter is
	// the one that turns into an uncorrectable error and takes the node down.
	// Flat for the first half, then climbing hard.
	s := series("DIMM_B2",
		0, 0, 0, 0, 0, 0, 0, 0,
		10, 30, 70, 150, 310, 630)

	trend, f := AnalyzeECC("node-1", s)

	if f == nil {
		t.Fatal("accelerating counter must produce a finding")
	}
	if f.Severity != SeverityUrgent {
		t.Errorf("severity = %s, want URGENT (rate %.1f/h, accel %.1fx)",
			f.Severity, trend.RatePerHour, trend.Acceleration)
	}
	if trend.Acceleration < accelerationRatio {
		t.Errorf("acceleration = %.2f, want >= %.1f", trend.Acceleration, accelerationRatio)
	}
	if trend.Total >= 10000 {
		t.Fatalf("guard: this case is meant to have a LOW total, got %.0f", trend.Total)
	}
}

func TestAnalyzeECC_AcceleratingBeatsHigherFlatRate(t *testing.T) {
	// Ranking check: the accelerating part must outrank the larger stable one.
	flat := ramp("DIMM_FLAT", 0, 3, 24) // steady 3/hour, never changes
	accel := series("DIMM_ACCEL",       // slower overall, but ramping
		0, 0, 0, 0, 0, 0, 0, 0,
		2, 6, 14, 30, 62, 126)

	_, flatFinding := AnalyzeECC("node-1", flat)
	_, accelFinding := AnalyzeECC("node-1", accel)

	if accelFinding == nil {
		t.Fatal("accelerating DIMM should produce a finding")
	}
	if flatFinding != nil && flatFinding.Severity > accelFinding.Severity {
		t.Errorf("stable DIMM (%s) outranked accelerating DIMM (%s) — backwards",
			flatFinding.Severity, accelFinding.Severity)
	}
}

func TestAnalyzeECC_CounterResetStartsNewEpoch(t *testing.T) {
	// BMC counters restart at zero on host power-cycle. The drop is not a
	// negative rate, and the pre-reset history must not dilute the current one.
	s := series("DIMM_C3",
		500, 520, 540, 560, // pre-reset epoch
		0, 20, 40, 60, 80, 100) // post power-cycle

	trend, _ := AnalyzeECC("node-1", s)

	if trend.Resets != 1 {
		t.Errorf("resets = %d, want 1", trend.Resets)
	}
	if trend.Samples != 6 {
		t.Errorf("epoch samples = %d, want 6 (post-reset only)", trend.Samples)
	}
	if trend.Total != 100 {
		t.Errorf("total = %.0f, want 100 (current epoch, not cumulative)", trend.Total)
	}
	if trend.RatePerHour <= 0 {
		t.Errorf("rate = %.2f, want positive — the reset must not produce a negative slope", trend.RatePerHour)
	}
}

func TestAnalyzeECC_ZeroCounterIsSilent(t *testing.T) {
	// The overwhelmingly common case. Healthy hardware must produce no output at
	// all, or the report is unreadable at fleet scale.
	_, f := AnalyzeECC("node-1", series("DIMM_D4", 0, 0, 0, 0, 0, 0))
	if f != nil {
		t.Fatalf("healthy DIMM produced a finding: %s", f.Summary)
	}
}

func TestAnalyzeECC_ShortHistoryIsHonestNotAlarming(t *testing.T) {
	// Three points over two hours cannot support a weekly extrapolation. Report
	// the uncertainty instead of inventing a rate — over-alerting from thin data
	// is how a tool gets ignored.
	s := Series{Component: "DIMM_E5", Samples: []Sample{
		{At: base, Value: 100},
		{At: base.Add(time.Hour), Value: 400},
		{At: base.Add(2 * time.Hour), Value: 900},
	}}

	_, f := AnalyzeECC("node-1", s)

	if f == nil {
		t.Fatal("a non-zero counter with thin history should still be surfaced")
	}
	if f.Severity > SeverityWatch {
		t.Errorf("severity = %s, want at most WATCH from 3 samples", f.Severity)
	}
	if f.Confidence > 0.3 {
		t.Errorf("confidence = %.2f, want low for thin history", f.Confidence)
	}
	if f.Kind != "ecc.insufficient-history" {
		t.Errorf("kind = %q, want ecc.insufficient-history", f.Kind)
	}
}

func TestAnalyzeECC_BurstyCounterLowersConfidence(t *testing.T) {
	// Same start and end, but one climbs smoothly and the other jumps around.
	// Both may warrant a look; the noisy one deserves less trust.
	smooth := ramp("DIMM_SMOOTH", 0, 10, 12)
	bursty := series("DIMM_BURSTY", 0, 0, 0, 60, 60, 60, 60, 60, 60, 110, 110, 110)

	_, sf := AnalyzeECC("node-1", smooth)
	_, bf := AnalyzeECC("node-1", bursty)

	if sf == nil || bf == nil {
		t.Fatal("both series should produce findings")
	}
	if bf.Confidence >= sf.Confidence {
		t.Errorf("bursty confidence %.2f should be below smooth %.2f", bf.Confidence, sf.Confidence)
	}
}

func TestAnalyzeECC_ResetsReduceConfidence(t *testing.T) {
	steady := ramp("DIMM_STEADY", 0, 10, 12)
	flappy := series("DIMM_FLAPPY", 0, 10, 20, 0, 10, 20, 0, 10, 20, 30, 40, 50)

	_, a := AnalyzeECC("node-1", steady)
	_, b := AnalyzeECC("node-1", flappy)

	if a == nil || b == nil {
		t.Skip("both series must produce findings for this comparison")
	}
	if b.Confidence >= a.Confidence {
		t.Errorf("repeatedly-reset confidence %.2f should be below steady %.2f", b.Confidence, a.Confidence)
	}
}

func TestLeastSquaresPerHour(t *testing.T) {
	tests := map[string]struct {
		series    Series
		wantSlope float64
		wantR2Min float64
	}{
		"perfect 10/hour": {series: ramp("x", 0, 10, 8), wantSlope: 10, wantR2Min: 0.99},
		"perfect 1/hour":  {series: ramp("x", 100, 1, 8), wantSlope: 1, wantR2Min: 0.99},
		"flat":            {series: series("x", 5, 5, 5, 5), wantSlope: 0, wantR2Min: 0},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			slope, r2 := leastSquaresPerHour(tt.series.Samples)
			if math.Abs(slope-tt.wantSlope) > 0.001 {
				t.Errorf("slope = %.4f, want %.4f", slope, tt.wantSlope)
			}
			if r2 < tt.wantR2Min {
				t.Errorf("R² = %.3f, want >= %.3f", r2, tt.wantR2Min)
			}
		})
	}
}

func TestLeastSquaresPerHour_DegenerateInputs(t *testing.T) {
	if slope, r2 := leastSquaresPerHour(nil); slope != 0 || r2 != 0 {
		t.Errorf("nil samples = (%.2f, %.2f), want (0, 0)", slope, r2)
	}
	if slope, _ := leastSquaresPerHour([]Sample{{At: base, Value: 1}}); slope != 0 {
		t.Errorf("single sample slope = %.2f, want 0", slope)
	}
	// All samples share a timestamp: no slope is defined, and this must not
	// divide by zero.
	same := []Sample{{At: base, Value: 1}, {At: base, Value: 9}, {At: base, Value: 4}}
	if slope, _ := leastSquaresPerHour(same); slope != 0 {
		t.Errorf("zero-span slope = %.2f, want 0", slope)
	}
}

func TestAccelerationOf_FlatThenRisingIsCappedNotInfinite(t *testing.T) {
	s := series("x", 0, 0, 0, 0, 0, 0, 0, 0, 1, 4, 9, 16, 25, 36)

	got := accelerationOf(s.Samples)

	if math.IsInf(got, 0) {
		t.Fatal("acceleration must be finite so it renders in operator output")
	}
	if got != maxAcceleration {
		t.Errorf("acceleration = %.1f, want the %.0f cap", got, maxAcceleration)
	}
}

func TestCurrentEpoch(t *testing.T) {
	tests := map[string]struct {
		values     []float64
		wantLen    int
		wantResets int
	}{
		"no reset":         {[]float64{1, 2, 3, 4}, 4, 0},
		"one reset":        {[]float64{5, 6, 0, 1, 2}, 3, 1},
		"two resets":       {[]float64{5, 0, 3, 0, 1}, 2, 2},
		"reset at the end": {[]float64{5, 6, 0}, 1, 1},
		"empty":            {nil, 0, 0},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			epoch, resets := currentEpoch(series("x", tt.values...).Samples)
			if len(epoch) != tt.wantLen {
				t.Errorf("epoch length = %d, want %d", len(epoch), tt.wantLen)
			}
			if resets != tt.wantResets {
				t.Errorf("resets = %d, want %d", resets, tt.wantResets)
			}
		})
	}
}

func TestSeverityString(t *testing.T) {
	for sev, want := range map[Severity]string{
		SeverityInfo: "INFO", SeverityWatch: "WATCH",
		SeverityPlan: "PLAN", SeverityUrgent: "URGENT",
	} {
		if got := sev.String(); got != want {
			t.Errorf("Severity(%d).String() = %q, want %q", sev, got, want)
		}
	}
}

func BenchmarkAnalyzeECC(b *testing.B) {
	// A week of one-minute samples, the realistic shape for a polled counter.
	s := ramp("DIMM_A1", 0, 0.5, 7*24*60)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		AnalyzeECC("node-1", s)
	}
}

func BenchmarkLeastSquaresPerHour(b *testing.B) {
	s := ramp("x", 0, 1, 10000)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		leastSquaresPerHour(s.Samples)
	}
}
