package analyze

import (
	"testing"
	"time"
)

func TestPrune_KeepsTheResetBoundaryAndTheVerdict(t *testing.T) {
	// The interaction this function exists for. Six samples of an old epoch, a
	// power-cycle reset, then a DIMM turning. A prefix cut by age alone would
	// take the reset with it, and the analysis would silently lose both the
	// epoch boundary and the reset count that scores its confidence.
	s := series("DIMM_A1",
		100, 110, 120, 130, 140, 150,
		5, 8, 12, 20, 35, 60, 110, 210, 400, 780)

	wantTrend, want := AnalyzeECC("node-1", s)
	if want == nil {
		t.Fatal("fixture produced no finding; the test cannot show the verdict is preserved")
	}

	// A cutoff half an hour past the reset: by age, everything up to and
	// including the first sample of the new epoch is expired.
	pruned := PruneCounter(s.Samples, base.Add(6*time.Hour+30*time.Minute))
	if len(pruned) >= len(s.Samples) {
		t.Fatalf("kept %d of %d samples — nothing was pruned", len(pruned), len(s.Samples))
	}

	gotTrend, got := AnalyzeECC("node-1", Series{Component: s.Component, Samples: pruned})
	if got == nil {
		t.Fatal("pruning silenced the finding entirely")
	}
	if gotTrend.Resets != wantTrend.Resets {
		t.Errorf("resets = %d, want %d — the reset boundary did not survive pruning",
			gotTrend.Resets, wantTrend.Resets)
	}
	if gotTrend.Samples != wantTrend.Samples || gotTrend.Span != wantTrend.Span {
		t.Errorf("epoch = %d samples over %s, want %d over %s",
			gotTrend.Samples, gotTrend.Span, wantTrend.Samples, wantTrend.Span)
	}
	if gotTrend.RatePerHour != wantTrend.RatePerHour || gotTrend.Acceleration != wantTrend.Acceleration {
		t.Errorf("rate/acceleration = %.3f/%.3f, want %.3f/%.3f",
			gotTrend.RatePerHour, gotTrend.Acceleration, wantTrend.RatePerHour, wantTrend.Acceleration)
	}
	if got.Severity != want.Severity || got.Confidence != want.Confidence {
		t.Errorf("verdict = %s at %.3f confidence, want %s at %.3f",
			got.Severity, got.Confidence, want.Severity, want.Confidence)
	}
	if got.Summary != want.Summary {
		t.Errorf("summary changed:\n got %q\nwant %q", got.Summary, want.Summary)
	}
}

func TestPrune_WillNotStarveATrendableSeries(t *testing.T) {
	// A long flat-then-rising history pruned down to two points reads as
	// insufficient-history, which drops a real URGENT finding to a 20%-confidence
	// note. Retention is a window, not a licence to delete the evidence.
	s := ramp("DIMM_B2", 100, 60, 12)

	pruned := PruneCounter(s.Samples, base.Add(11*time.Hour))

	if len(pruned) < minSamplesForTrend {
		t.Errorf("kept %d samples, want at least %d", len(pruned), minSamplesForTrend)
	}
	if span := pruned[len(pruned)-1].At.Sub(pruned[0].At); span < minSpanForTrend {
		t.Errorf("kept %s of span, want at least %s", span, minSpanForTrend)
	}
	_, f := AnalyzeECC("node-1", Series{Component: s.Component, Samples: pruned})
	if f == nil || f.Kind != "ecc.degrading" {
		t.Fatalf("pruning turned a degrading DIMM into %+v", f)
	}
}

func TestPrune_DropsSamplesOutsideTheWindow(t *testing.T) {
	// The point of the exercise: old samples actually go away.
	s := ramp("DIMM_C3", 0, 1, 24)

	cutoff := base.Add(12 * time.Hour)
	pruned := PruneCounter(s.Samples, cutoff)

	if len(pruned) != 12 {
		t.Fatalf("kept %d samples, want 12", len(pruned))
	}
	if !pruned[0].At.Equal(cutoff) {
		t.Errorf("oldest kept sample at %s, want %s", pruned[0].At, cutoff)
	}
}

func TestPrune_EdgeCases(t *testing.T) {
	if got := PruneCounter(nil, base); got != nil {
		t.Errorf("PruneCounter(nil) = %+v, want nil", got)
	}

	s := ramp("DIMM_D4", 0, 1, 6)
	if got := PruneCounter(s.Samples, base); len(got) != 6 {
		t.Errorf("a cutoff at the oldest sample dropped %d samples", 6-len(got))
	}

	// Already shorter than the trend floor: there is nothing safe to drop.
	short := series("DIMM_E5", 1, 2)
	if got := PruneCounter(short.Samples, base.Add(100*time.Hour)); len(got) != 2 {
		t.Errorf("kept %d of 2 samples; an untrendable series cannot be improved by pruning it", len(got))
	}
}

func TestPruneGauge_KeepsEnoughToStillSpeakAboutTheSensor(t *testing.T) {
	// Why a gauge needs its own retention, and it is not the reset rule — on a
	// margin the last dip is usually the last reading, so that clamp mostly does
	// nothing. It is the floor. PruneCounter protects four samples over thirty
	// minutes, which is what AnalyzeECC needs and a fraction of what
	// AnalyzeThermal will speak about, so a sensor pruned by the counter rule
	// survives into silence.
	s := ramp("CPU0_Die", 40, -0.4, 96)
	cutoff := base.Add(90 * time.Hour)

	byCounterRule := PruneCounter(s.Samples, cutoff)
	byGaugeRule := PruneGauge(s.Samples, cutoff)

	counterTrend, _ := AnalyzeThermal("node-1", Series{Component: s.Component, Samples: byCounterRule})
	if counterTrend.Trendable() {
		t.Fatalf("guard: the counter floor was expected to leave an untrendable sensor, got %d readings over %s",
			counterTrend.Samples, counterTrend.Span)
	}

	gaugeTrend, f := AnalyzeThermal("node-1", Series{Component: s.Component, Samples: byGaugeRule})
	if !gaugeTrend.Trendable() {
		t.Errorf("the gauge rule left %d readings over %s, below the %d over %s the analyser needs",
			gaugeTrend.Samples, gaugeTrend.Span, minSamplesForThermal, minSpanForThermal)
	}
	if f == nil {
		t.Fatal("pruning silenced a sensor that is losing headroom")
	}
	if len(byGaugeRule) >= len(s.Samples) {
		t.Errorf("kept %d of %d readings — retention has to actually delete something",
			len(byGaugeRule), len(s.Samples))
	}
}

func TestPruneGauge_TrimsALoadCycleToTheWindow(t *testing.T) {
	// The point of the exercise: on a sensor with plenty of history, old readings
	// actually go away, dips and all.
	s := wave("CPU0_Die", 30, 5, 6, 96)
	cutoff := base.Add(48 * time.Hour)

	pruned := PruneGauge(s.Samples, cutoff)

	if len(pruned) != 48 {
		t.Fatalf("kept %d readings, want 48", len(pruned))
	}
	if !pruned[0].At.Equal(cutoff) {
		t.Errorf("oldest kept reading at %s, want %s", pruned[0].At, cutoff)
	}
}

func TestPruneGauge_EdgeCases(t *testing.T) {
	if got := PruneGauge(nil, base); got != nil {
		t.Errorf("PruneGauge(nil) = %+v, want nil", got)
	}
	s := ramp("CPU0_Die", 30, 0, 48)
	if got := PruneGauge(s.Samples, base); len(got) != 48 {
		t.Errorf("a cutoff at the oldest reading dropped %d readings", 48-len(got))
	}
	// Already thinner than the thermal trend floor: nothing is safe to drop.
	short := series("CPU1_Inlet", 5, 5, 5)
	if got := PruneGauge(short.Samples, base.Add(1000*time.Hour)); len(got) != 3 {
		t.Errorf("kept %d of 3 readings; an untrendable sensor cannot be improved by pruning it", len(got))
	}
}
