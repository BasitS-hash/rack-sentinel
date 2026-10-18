package analyze

import (
	"math"
	"strings"
	"testing"
	"time"
)

// lifeLeft builds n hourly readings of a vendor life-left estimate, starting at
// start and declining by perDay percent, floored to whole percent.
//
// The flooring is the point. Redfish types PredictedMediaLifeLeftPercent as a
// float, but the firmware behind it is counting erase cycles against a
// manufacturer-rated budget and almost always publishes whole percent. A fixture
// that declined smoothly would be testing an analyser against a signal no drive
// produces: what comes back is a staircase that sits on one value for days.
func lifeLeft(component string, start, perDay float64, n int) Series {
	vals := make([]float64, n)
	for i := range vals {
		vals[i] = math.Floor(start - perDay*float64(i)/24)
	}
	return series(component, vals...)
}

func TestAnalyzeDriveWear_StableLowLifeIsNotAFinding(t *testing.T) {
	// The premise on the third signal, and the half a threshold alerter gets
	// wrong. A drive reporting 8% of media life left that has reported 8% for a
	// fortnight is wearing at a rate nothing can measure — it will outlive the
	// node. Every absolute-threshold rule in the industry pages someone here.
	s := lifeLeft("Bay 1", 8, 0, 336)

	trend, f := AnalyzeDriveWear("node-1", s, false)

	if trend.Steps != 0 {
		t.Errorf("a flat estimate should have stepped zero times, got %d", trend.Steps)
	}
	if f != nil {
		t.Fatalf("a stable low life-left must not produce a finding, got %s: %s", f.Severity, f.Summary)
	}
}

func TestAnalyzeDriveWear_FastDecliningHighLifeAlerts(t *testing.T) {
	// The other half. 81% of media life left is the healthiest-looking drive in
	// the chassis by any absolute reading, and it has burned 14% in a fortnight.
	// Divide what is left by that rate and it has about eleven weeks.
	s := lifeLeft("Bay 2", 95, 1, 336)

	trend, f := AnalyzeDriveWear("node-1", s, false)

	if f == nil {
		t.Fatal("a drive burning media life steadily must produce a finding")
	}
	if f.Severity < SeverityPlan {
		t.Errorf("severity = %s, want at least PLAN (%.2f%%/day, %.0f%% left)",
			f.Severity, trend.WearPerDay, trend.LifeLeftPercent)
	}
	if trend.LifeLeftPercent < 70 {
		t.Fatalf("guard: this case is meant to still look HEALTHY by absolute life left, got %.0f%%",
			trend.LifeLeftPercent)
	}
	days, ok := trend.DaysToSpent()
	if !ok {
		t.Fatal("a drive with this many steps behind the rate should be projectable")
	}
	if days <= 0 || days > planDaysToSpent {
		t.Errorf("days to spent = %.1f, want (0, %.0f]", days, planDaysToSpent)
	}
	if f.Kind != "drive.wearing" {
		t.Errorf("kind = %q, want drive.wearing", f.Kind)
	}
}

func TestAnalyzeDriveWear_DecliningOutranksStableLowLife(t *testing.T) {
	// Ranking check, the same inversion both other analysers are pinned on: the
	// drive with less life left right now must not outrank the one losing it.
	stable := lifeLeft("Bay 1", 6, 0, 336)
	declining := lifeLeft("Bay 2", 95, 1, 336)

	_, stableFinding := AnalyzeDriveWear("node-1", stable, false)
	_, decliningFinding := AnalyzeDriveWear("node-1", declining, false)

	if decliningFinding == nil {
		t.Fatal("the declining drive should produce a finding")
	}
	if stableFinding != nil {
		t.Errorf("the stable drive at 6%% produced %s: %s", stableFinding.Severity, stableFinding.Summary)
	}
}

func TestAnalyzeDriveWear_OneStepIsNotATrend(t *testing.T) {
	// The floor this analyser needs and the other two do not, stated where it can
	// be disagreed with.
	//
	// A fortnight of readings that sat on 30 and then sat on 29 contains exactly
	// one measurement: that a step happened, somewhere. The rate fitted to it is
	// almost entirely a statement about where in the window that edge landed, and
	// the window is the only thing that would change it.
	vals := make([]float64, 336)
	for i := range vals {
		vals[i] = 30
		if i >= 200 {
			vals[i] = 29
		}
	}
	s := series("Bay 1", vals...)

	trend, f := AnalyzeDriveWear("node-1", s, false)

	if f != nil {
		t.Fatalf("a single one-percent step produced %s: %s", f.Severity, f.Summary)
	}
	if trend.Steps != 1 {
		t.Fatalf("guard: this fixture is meant to step exactly once, got %d", trend.Steps)
	}
	if trend.Trendable() {
		t.Error("one step must not be trendable however many readings sit either side of it")
	}

	// And the two reasons the thermal treatment was not reused here, one of which
	// is not the one you would guess.
	//
	// R² is no defence at all on a staircase. A straight line explains nearly
	// three quarters of a single step, comfortably past both thermal fit floors,
	// so that analyser would have published a rate and a replacement date off one
	// edge.
	slope, r2 := leastSquaresPerHour(wearOf(s.Samples))
	if r2 < minFitForProjection {
		t.Fatalf("guard: a single step is meant to fit a line well, got R²=%.2f", r2)
	}

	// The magnitude floor, honestly, would have caught this particular fixture:
	// the fit accounts for about 1.4 points across the window, under the three
	// the thermal analyser demands. It is still the wrong instrument, for a
	// reason this fixture cannot show. Three degrees is a statement about sensor
	// rounding and means the same thing on every sensor; three percentage points
	// means almost nothing on a drive at 95% and most of the remaining life of a
	// drive at 4%, and it is not invariant to the quantum — Redfish types this
	// field as a float, and a firmware reporting in fives clears a three-point
	// floor on its first step. Counting steps measures the evidence itself:
	// how many independent observations the slope rests on, whatever size they
	// come in.
	if magnitude := slope * trend.Span.Hours(); magnitude >= minShrinkCelsius {
		t.Errorf("guard: this fixture is meant to sit under a magnitude floor too, got %.2f points", magnitude)
	}
}

func TestAnalyzeDriveWear_ThinHistoryDoesNotGuess(t *testing.T) {
	// Silent, like the thermal analyser and deliberately unlike the ECC one. A
	// non-zero error count is abnormal on its own, so AnalyzeECC has something to
	// report while the trend forms. A life-left estimate below 100 is not
	// abnormal — it is every drive that has ever been written to — so there is
	// nothing to surface until the trend is claimable.
	s := series("Bay 1", 92, 92, 91, 91, 90, 90)

	trend, f := AnalyzeDriveWear("node-1", s, false)

	if f != nil {
		t.Fatalf("six readings over five hours produced %s: %s", f.Severity, f.Summary)
	}
	if trend.Trendable() {
		t.Error("six readings over five hours must not be trendable")
	}
	if trend.LifeLeftPercent != 90 {
		t.Errorf("life left = %.0f, want the last reported reading (90)", trend.LifeLeftPercent)
	}
}

func TestAnalyzeDriveWear_AcceleratingDeclineIsUrgent(t *testing.T) {
	// Acceleration means the same thing here as it does for a counter and a
	// margin, and on this signal it is more trustworthy than on either: the
	// estimate is integral, so a half that did not step did not wear. There is no
	// smoothing residual to argue about and no load cycle underneath it.
	//
	// A week flat at 90%, then a week burning 2% a day. Still 76% left.
	flat := make([]float64, 168)
	for i := range flat {
		flat[i] = 90
	}
	falling := lifeLeft("Bay 2", 90, 2, 168)
	vals := flat
	for _, s := range falling.Samples {
		vals = append(vals, s.Value)
	}
	s := series("Bay 2", vals...)

	trend, f := AnalyzeDriveWear("node-1", s, false)

	if f == nil {
		t.Fatal("an accelerating decline must produce a finding")
	}
	if trend.Acceleration < accelerationRatio {
		t.Errorf("acceleration = %.2f, want >= %.1f", trend.Acceleration, accelerationRatio)
	}
	days, ok := trend.DaysToSpent()
	if !ok {
		t.Fatal("this fixture is meant to be projectable")
	}
	if days <= urgentDaysToSpent || days > planDaysToSpent {
		t.Fatalf("guard: %.0f days to spent, want the PLAN horizon (%.0f, %.0f] so that acceleration is what moves it up",
			days, urgentDaysToSpent, planDaysToSpent)
	}
	if f.Severity != SeverityUrgent {
		t.Errorf("severity = %s, want URGENT — acceleration moves a plan-horizon crossing up", f.Severity)
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

func TestAnalyzeDriveWear_WeeksOutIsUrgent(t *testing.T) {
	// A drive at 12% with a fortnight of 2%-a-day behind it has about six days.
	// Procurement lead time is the reason this horizon is in weeks rather than
	// hours: a drive nobody ordered is not replaceable on the day it is spent.
	s := lifeLeft("Bay 3", 40, 2, 336)

	trend, f := AnalyzeDriveWear("node-1", s, false)

	if f == nil {
		t.Fatal("a drive weeks from spent must produce a finding")
	}
	days, _ := trend.DaysToSpent()
	if f.Severity != SeverityUrgent {
		t.Errorf("severity = %s, want URGENT (%.1f days to spent)", f.Severity, days)
	}
	if trend.Acceleration >= accelerationRatio {
		t.Errorf("guard: a straight decline is not accelerating, got %.2fx", trend.Acceleration)
	}
}

func TestAnalyzeDriveWear_ProjectionIsCoarseAndLabelled(t *testing.T) {
	// The honesty requirement that separates this signal from the other two.
	// PredictedMediaLifeLeftPercent is not a measurement — it is the vendor's own
	// endurance model, computed differently by every firmware, and projecting a
	// replacement date off someone else's model is a second extrapolation stacked
	// on a first. So the horizon is rounded to weeks and says what it is.
	s := lifeLeft("Bay 2", 95, 1, 336)

	trend, f := AnalyzeDriveWear("node-1", s, false)

	if f == nil {
		t.Fatal("expected a finding")
	}
	var projection string
	for _, e := range f.Evidence {
		if strings.Contains(e, "spent in") {
			projection = e
		}
	}
	if projection == "" {
		t.Fatalf("no projection in the evidence: %q", f.Evidence)
	}
	if !strings.Contains(projection, "vendor") {
		t.Errorf("a projection off a vendor estimate must say so: %q", projection)
	}
	days, ok := trend.DaysToSpent()
	if !ok {
		t.Fatal("this fixture is meant to be projectable")
	}
	if !strings.Contains(projection, aboutHorizon(days)) {
		t.Errorf("the horizon is rounded, not quoted to the hour: %q carries %.3f days", projection, days)
	}

	// And the same discount on the number an operator reads as a percentage.
	measured := confidenceFor(fit{Points: trend.Steps, RSquared: trend.RSquared, Span: trend.Span})
	if f.Confidence >= measured {
		t.Errorf("confidence %.2f is not discounted below the %.2f an equivalent measured series would earn",
			f.Confidence, measured)
	}
}

func TestAnalyzeDriveWear_UnreportedDriveIsAbsentNotZero(t *testing.T) {
	// The project's central invariant, and it is sharper here than anywhere else:
	// most BMCs populate PredictedMediaLifeLeftPercent on NVMe and omit it
	// entirely on SAS and SATA. A drive that did not report is unknown; a drive
	// reporting 0 is spent. Coercing the first into the second would declare a
	// chassis of SAS disks dead.
	trend, f := AnalyzeDriveWear("node-1", Series{Component: "Bay 1"}, false)

	if trend.Samples != 0 {
		t.Errorf("samples = %d, want 0 for a drive with no readings", trend.Samples)
	}
	if f != nil {
		t.Fatalf("an unreported drive must say nothing, got %s: %s", f.Severity, f.Summary)
	}

	// And the opposite condition is a finding, from the same code path.
	_, spent := AnalyzeDriveWear("node-1", series("Bay 1", 3, 2, 1, 0), false)
	if spent == nil {
		t.Fatal("0% of media life left is a spent drive, not a missing reading")
	}
}

func TestAnalyzeDriveWear_SpentNeedsNoTrend(t *testing.T) {
	// A drive the vendor's own model says is out of media life is a present fact,
	// not a prediction. It does not wait for a trend and it takes no estimate
	// discount, because nothing is being extrapolated — the discount is on
	// projecting forward from the model, not on repeating what it said.
	s := series("Bay 1", 3, 2, 1, 0)

	trend, f := AnalyzeDriveWear("node-1", s, false)

	if f == nil {
		t.Fatal("a drive at 0% media life left must be reported")
	}
	if f.Severity != SeverityUrgent {
		t.Errorf("severity = %s, want URGENT", f.Severity)
	}
	if f.Kind != "drive.media-spent" {
		t.Errorf("kind = %q, want drive.media-spent", f.Kind)
	}
	if f.Confidence != 1.0 {
		t.Errorf("confidence = %.2f, want 1.0 — this is the vendor's own verdict, not an extrapolation of it", f.Confidence)
	}
	if trend.Trendable() {
		t.Error("guard: this fixture is meant to be too thin to trend")
	}
}

func TestAnalyzeDriveWear_ReplacementStartsNewEpoch(t *testing.T) {
	// A drive swapped in a bay keeps the bay's identifier and reports a fresh
	// estimate, which is this signal's equivalent of a counter reset — and the
	// reason the epoch logic is shared rather than reimplemented.
	//
	// Fitting across the swap would hand back a strongly negative wear rate and
	// silence the bay for as long as the old drive's readings stayed in the
	// window, which is the retention bug the prune rules exist to prevent,
	// arriving from the other direction.
	old := lifeLeft("Bay 1", 25, 1, 200)
	fresh := lifeLeft("Bay 1", 100, 1, 200)
	vals := make([]float64, 0, 400)
	for _, s := range old.Samples {
		vals = append(vals, s.Value)
	}
	for _, s := range fresh.Samples {
		vals = append(vals, s.Value)
	}
	s := series("Bay 1", vals...)

	trend, f := AnalyzeDriveWear("node-1", s, false)

	if trend.Replacements != 1 {
		t.Fatalf("replacements = %d, want 1", trend.Replacements)
	}
	if trend.Samples != 200 {
		t.Errorf("samples = %d, want 200 — only the current drive's readings", trend.Samples)
	}
	if trend.LifeLeftPercent != 91 {
		t.Errorf("life left = %.0f, want 91 (the fresh drive's latest reading)", trend.LifeLeftPercent)
	}
	if f == nil {
		t.Fatal("the fresh drive is wearing and must still be trended after the swap")
	}
	if trend.WearPerDay <= 0 {
		t.Errorf("wear rate = %.4f, want positive — the fit spanned the swap", trend.WearPerDay)
	}
}

func TestAnalyzeDriveWear_RecalibrationIsNotAReplacement(t *testing.T) {
	// Where this signal stops looking like a counter. A BMC counter only ever
	// resets to zero, so any decrease is unambiguously a new epoch. A vendor life
	// estimate is a model output and can tick back up by a point when the
	// firmware recalculates: treating that as a new drive would discard a
	// fortnight of history and silence a drive that is genuinely wearing.
	s := lifeLeft("Bay 1", 90, 1, 336)
	s.Samples[200].Value++ // firmware revised its estimate upward by a point

	trend, f := AnalyzeDriveWear("node-1", s, false)

	if trend.Replacements != 0 {
		t.Errorf("replacements = %d, want 0 — a one-point revision is not a new drive", trend.Replacements)
	}
	if trend.Samples != 336 {
		t.Errorf("samples = %d, want all 336 — the epoch was split by a rounding wobble", trend.Samples)
	}
	if f == nil {
		t.Fatal("a one-point upward revision silenced a wearing drive")
	}
}

func TestAnalyzeDriveWear_RecoveringLifeLeftIsSilent(t *testing.T) {
	// An estimate that is climbing without a replacement-sized jump — a used
	// spare fitted in the bay, or firmware revising upward over several polls —
	// is not a finding in either direction.
	_, f := AnalyzeDriveWear("node-1", lifeLeft("Bay 1", 70, -1, 336), false)
	if f != nil {
		t.Fatalf("a rising life-left estimate produced %s: %s", f.Severity, f.Summary)
	}
}

func TestAnalyzeDriveWear_VendorFlagDoesNotSuppressOurFinding(t *testing.T) {
	// The requirement that makes this signal worth having next to the vendor's
	// own. FailurePredicted is an absolute-threshold opinion computed by the
	// firmware; a drive burning 1% of its media a day with the flag clear is
	// exactly the false negative this tool exists to catch, so the trend finding
	// stands and carries the disagreement.
	s := lifeLeft("Bay 2", 95, 1, 336)

	_, f := AnalyzeDriveWear("node-1", s, false)

	if f == nil {
		t.Fatal("a clear vendor flag must not suppress the trend finding")
	}
	var disagreement string
	for _, e := range f.Evidence {
		if strings.Contains(e, "FailurePredicted") {
			disagreement = e
		}
	}
	if disagreement == "" {
		t.Fatalf("the vendor's verdict belongs in the evidence either way: %q", f.Evidence)
	}
	if !strings.Contains(disagreement, "disagree") {
		t.Errorf("a clear flag under a trend finding is a disagreement and should say so: %q", disagreement)
	}

	// And where they agree, the same line says that instead of going quiet.
	_, agreed := AnalyzeDriveWear("node-1", s, true)
	if agreed == nil {
		t.Fatal("expected a finding")
	}
	var agreement string
	for _, e := range agreed.Evidence {
		if strings.Contains(e, "FailurePredicted") {
			agreement = e
		}
	}
	if !strings.Contains(agreement, "agree") {
		t.Errorf("agreement should be stated as plainly as disagreement: %q", agreement)
	}
}

func TestAnalyzeDriveWear_VendorFlagAloneIsStillReported(t *testing.T) {
	// The converse false negative — ours. A drive the firmware has flagged while
	// its life-left estimate sits flat gets reported as what it is: the vendor's
	// opinion, at the vendor's confidence, under its own kind so an operator can
	// see which signal spoke.
	trend, f := AnalyzeDriveWear("node-1", lifeLeft("Bay 1", 40, 0, 336), true)

	if f == nil {
		t.Fatal("a vendor-flagged drive must be reported even when the trend says nothing")
	}
	if f.Kind != "drive.vendor-failure-predicted" {
		t.Errorf("kind = %q, want drive.vendor-failure-predicted", f.Kind)
	}
	if f.Severity != SeverityPlan {
		t.Errorf("severity = %s, want PLAN", f.Severity)
	}
	if !trend.VendorFailurePredicted {
		t.Error("the flag must reach the trend so the exporter can publish it")
	}

	// Including when there is no series at all, which is the common SAS case: the
	// firmware flags the drive and never populates a life-left estimate to trend.
	_, bare := AnalyzeDriveWear("node-1", Series{Component: "Bay 1"}, true)
	if bare == nil {
		t.Fatal("a flagged drive with no life-left estimate must not vanish")
	}
	if bare.Kind != "drive.vendor-failure-predicted" {
		t.Errorf("kind = %q, want drive.vendor-failure-predicted", bare.Kind)
	}
}

func TestAnalyzeDriveWear_FindingsNeverDuplicateADrive(t *testing.T) {
	// One drive, one row. When both signals fire the trend finding carries the
	// agreement in its evidence rather than a second finding appearing beside it,
	// because two rows for one bay is how a report stops being read top-down.
	_, f := AnalyzeDriveWear("node-1", lifeLeft("Bay 2", 95, 1, 336), true)
	if f == nil {
		t.Fatal("expected a finding")
	}
	if f.Kind != "drive.wearing" {
		t.Errorf("kind = %q, want drive.wearing — the measured trend outranks the vendor boolean", f.Kind)
	}
}

func TestDriveWearTrend_TrendableNeedsSamplesSpanAndSteps(t *testing.T) {
	full := DriveWearTrend{
		Samples: minSamplesForWear, Span: minSpanForWear, Steps: minWearSteps,
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
	coarse.Steps--
	if coarse.Trendable() {
		t.Error("too few steps to separate the rate from the step times should not be trendable")
	}

	// Projection asks for more steps than a rate does, and nothing else. R² is
	// not the gate it is on a thermal margin, because a staircase fits a line
	// well while the slope is still mostly a statement about two step edges.
	rated := full
	rated.WearPerDay = 0.96
	rated.RSquared = 0.99
	if rated.Projectable() {
		t.Errorf("%d steps earns a rate, not a replacement date", rated.Steps)
	}
	projectable := rated
	projectable.Steps = minStepsForProjection
	if !projectable.Projectable() {
		t.Error("the projection floor should be projectable")
	}
	noisy := projectable
	noisy.RSquared = 0.05
	if !noisy.Projectable() {
		t.Error("R² is deliberately not the projection gate on a quantised signal")
	}
}

func TestWearOf_InvertsTheSignOnce(t *testing.T) {
	// The shared slope and acceleration helpers read "bigger is worse". A life
	// estimate declines, so it is inverted into wear here, once, and every helper
	// downstream — including the epoch split — sees the same shape it sees for a
	// counter.
	got := wearOf(series("Bay 1", 100, 90, 40).Samples)

	want := []float64{0, 10, 60}
	if len(got) != len(want) {
		t.Fatalf("got %d samples, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].Value != want[i] {
			t.Errorf("sample %d = %.0f, want %.0f", i, got[i].Value, want[i])
		}
		if got[i].At != series("Bay 1", 100, 90, 40).Samples[i].At {
			t.Errorf("sample %d lost its timestamp", i)
		}
	}
	if got := wearOf(nil); got != nil {
		t.Errorf("wearOf(nil) = %+v, want nil", got)
	}
}

func TestAboutHorizon(t *testing.T) {
	// Round numbers only, and never finer than the data. A replacement date to
	// the day off a vendor's endurance model is a precision nobody has.
	tests := map[float64]string{
		3:   "~3d",
		13:  "~13d",
		21:  "~3w",
		80:  "~11w",
		200: "~7mo",
	}
	for days, want := range tests {
		if got := aboutHorizon(days); got != want {
			t.Errorf("aboutHorizon(%.0f) = %q, want %q", days, got, want)
		}
	}
}

func BenchmarkAnalyzeDriveWear(b *testing.B) {
	// A fortnight of 15-minute readings, the realistic shape for a polled drive.
	s := lifeLeft("Bay 1", 90, 1, 14*24*4)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		AnalyzeDriveWear("node-1", s, false)
	}
}

func TestAnalyzeDriveWear_AcceleratingWithoutAHorizonIsPlan(t *testing.T) {
	// Where the two floors come apart. Four steps is enough to say the drive has
	// started wearing and is not enough to say when it will be spent, so the
	// finding carries the rate and the acceleration and no replacement date. That
	// is the shape a drive has in the fortnight after its write load changes,
	// which is exactly when somebody should be told.
	vals := make([]float64, 336)
	for i := range vals {
		vals[i] = 50
		if i >= 250 {
			vals[i] = 50 - math.Floor(float64(i-250)/20)
		}
	}
	s := series("Bay 4", vals...)

	trend, f := AnalyzeDriveWear("node-1", s, false)

	if trend.Steps != 4 {
		t.Fatalf("guard: this fixture is meant to step four times, got %d", trend.Steps)
	}
	if !trend.Trendable() || trend.Projectable() {
		t.Fatalf("guard: four steps should be trendable (%v) and not projectable (%v)",
			trend.Trendable(), trend.Projectable())
	}
	if f == nil {
		t.Fatal("a drive that has started wearing hard must not wait for a projection")
	}
	if f.Severity != SeverityPlan {
		t.Errorf("severity = %s, want PLAN", f.Severity)
	}
	var said bool
	for _, e := range f.Evidence {
		if strings.Contains(e, "no replacement horizon") {
			said = true
		}
	}
	if !said {
		t.Errorf("a finding without a horizon has to say why it has none: %q", f.Evidence)
	}
}

func TestAnalyzeDriveWear_SlowWearIsOnlyWatch(t *testing.T) {
	// A drive with nearly half a year of media left is wearing, measurably, and
	// is nobody's problem this quarter. It is reported at WATCH so it shows up
	// under -min-severity info and stays out of the way otherwise.
	s := lifeLeft("Bay 5", 95, 0.5, 336)

	trend, f := AnalyzeDriveWear("node-1", s, false)

	if f == nil {
		t.Fatal("expected a WATCH finding")
	}
	days, ok := trend.DaysToSpent()
	if !ok || days <= planDaysToSpent {
		t.Fatalf("guard: %.0f days to spent (projectable=%v), want beyond the %.0f-day planning horizon",
			days, ok, planDaysToSpent)
	}
	if f.Severity != SeverityWatch {
		t.Errorf("severity = %s, want WATCH", f.Severity)
	}
}

func TestAnalyzeDriveWear_SpentDriveCarriesTheVendorVerdictToo(t *testing.T) {
	// Both signals on the one drive where they are least likely to disagree, and
	// the line still appears: an operator who reads "no media life left" wants to
	// know whether the firmware had also noticed.
	_, f := AnalyzeDriveWear("node-1", series("Bay 1", 3, 2, 1, 0), true)

	if f == nil || f.Kind != "drive.media-spent" {
		t.Fatalf("expected a drive.media-spent finding, got %+v", f)
	}
	var verdict string
	for _, e := range f.Evidence {
		if strings.Contains(e, "FailurePredicted") {
			verdict = e
		}
	}
	if !strings.Contains(verdict, "FailurePredicted=true") {
		t.Errorf("the vendor's verdict is missing from a spent drive: %q", f.Evidence)
	}
}
