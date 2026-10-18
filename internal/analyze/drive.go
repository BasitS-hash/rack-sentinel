package analyze

import (
	"fmt"
	"math"
	"time"
)

// Drive wear thresholds. As with the ECC and thermal constants these describe
// shape, not a level: how much life a drive has left is a function of how long
// it has been in service, and only the rate it is spending it at says whether
// anybody needs to do something.
const (
	// fullLifePercent is a fresh drive's media-life estimate, and the top of the
	// scale the reported percentage is inverted against.
	fullLifePercent = 100.0

	// minSamplesForWear is the fewest readings a wear trend may rest on. The real
	// floor is the step count below; this one only asks for a few readings a day,
	// so that a step is located to better than a day of the window it is fitted
	// over.
	minSamplesForWear = 24

	// minSpanForWear is the shortest window a wear rate may be claimed over. A
	// week, for a reason specific to this signal: the estimate steps by whole
	// percent, and even a drive on course to be spent within the year moves about
	// one step a day. A shorter window cannot hold enough steps to fit anything
	// to. It also has to fit inside the default two-week retention with room for
	// the acceleration split either side.
	minSpanForWear = 7 * 24 * time.Hour

	// minWearSteps is the fewest times the estimate must have changed before a
	// rate is claimed, and it is the floor that does the work here.
	//
	// The signal is coarse and integral: a drive sits on one whole percent for
	// days and then drops a point. A line fitted to one step is a statement about
	// where that single edge landed in the window and nothing else; with k steps
	// the edges average out and the rate starts to mean something. Three is the
	// fewest that can be wrong in both directions.
	//
	// Why this rather than the thermal analyser's minShrinkCelsius, which is the
	// floor the same problem gets there: a magnitude floor in percentage points
	// would in fact reject the common case — one whole-percent step fits to about
	// 1.4 points across a fortnight — but it is not the right instrument twice
	// over. Three degrees is a statement about sensor rounding and means the same
	// on every sensor; three percentage points means almost nothing on a drive at
	// 95% and most of what is left of a drive at 4%. And it is not invariant to
	// the quantum: Redfish types this field as a float, and a firmware reporting
	// in fives clears a three-point floor on its first step. A step count
	// measures the evidence itself, whatever size the steps come in.
	minWearSteps = 3

	// minStepsForProjection is the fewest steps a replacement horizon may be
	// extrapolated from. Deliberately higher than minWearSteps and deliberately
	// not an R² floor, which is what the thermal analyser gates its projection
	// on: a staircase fits a straight line well — a single step scores R²≈0.7 —
	// so a good fit here is not evidence that the slope is stable. More steps is.
	minStepsForProjection = 5

	// minReplacementDrop is the fall in wear that counts as a different drive in
	// the same bay rather than the firmware revising its estimate. A swap moves
	// the estimate by tens of points; a recalculation moves it by one or two.
	//
	// What this does not catch: a used spare fitted in the bay at close to the
	// old drive's wear. The series then carries a small upward jump in life left,
	// the fit reads it as a drive that is not wearing, and the bay goes quiet
	// until the new drive has worn past the old one's last reading.
	minReplacementDrop = 20.0

	// urgentDaysToSpent is the projected horizon inside which a replacement has
	// to be ordered rather than scheduled. A month, because the constraint on a
	// disk is procurement rather than a maintenance window: a drive nobody
	// ordered is not replaceable on the day its media runs out.
	urgentDaysToSpent = 30.0

	// planDaysToSpent is the horizon worth raising a ticket for.
	planDaysToSpent = 90.0

	// vendorEstimateConfidence discounts every projection made off
	// PredictedMediaLifeLeftPercent.
	//
	// The other two signals are measurements: an error count the BMC observed, a
	// temperature a sensor read. This one is a model — the firmware's own guess at
	// remaining endurance, computed differently by every vendor and not
	// comparable between two drives in the same chassis. Trending it is still the
	// right thing to do, because its *derivative* is a real statement about how
	// hard this drive is being written, but a date extrapolated from somebody
	// else's model has one more layer of someone else's assumptions in it than a
	// date extrapolated from a counter. The discount is flat and visible rather
	// than folded into the fit, so an operator comparing a 70%-confidence drive
	// finding with a 90%-confidence DIMM finding is comparing the right things.
	vendorEstimateConfidence = 0.75

	// hoursPerDay converts the shared least-squares slope into the unit this
	// signal is reported in.
	hoursPerDay = 24.0

	// daysPerMonth is the mean Gregorian month, used only to render a horizon.
	daysPerMonth = 30.44
)

// DriveWearTrend is the outcome of analysing one drive's media-life estimate.
type DriveWearTrend struct {
	Component string

	// LifeLeftPercent is the most recent reported estimate of media life
	// remaining, 100 down to 0. Zero means the vendor's endurance model is
	// exhausted.
	LifeLeftPercent float64

	// WearPerDay is percentage points of media life spent per day: the
	// least-squares slope of the inverted series over the current drive's epoch.
	// Positive means the drive is wearing.
	//
	// Per day, where the other two trends are per hour, because this signal moves
	// on a weekly scale: a drive on course to be spent inside the year wears
	// about 0.04%/hour, and four leading zeros is not a number anybody reads off
	// a report or a dashboard. The conversion happens once, where the slope is
	// computed, rather than in every caller that wants to print it.
	WearPerDay float64

	// Acceleration is the recent-half wear rate divided by the earlier-half rate.
	// Above 1 means the drive is being written harder than it was. Zero means it
	// could not be computed.
	Acceleration float64

	// RSquared is the goodness of fit of the linear model, 0..1. Reported because
	// it is worth seeing, but not used as a gate — see minStepsForProjection.
	RSquared float64

	// Span is the time covered by the current drive's readings.
	Span time.Duration

	// Samples is how many readings the current drive contributed. Zero means the
	// BMC did not report an estimate for this drive at all, which is not the same
	// as no life left.
	Samples int

	// Steps is how many times the estimate fell within the epoch. This, not
	// Samples, is how much evidence the rate actually has under it.
	Steps int

	// Replacements counts how many times the estimate jumped back up by more than
	// minReplacementDrop, which is a different drive in the same bay.
	Replacements int

	// VendorFailurePredicted is the drive's own FailurePredicted flag from this
	// poll — the firmware's absolute-threshold opinion, carried alongside this
	// analysis rather than instead of it.
	//
	// False is not "the vendor says healthy". Redfish types FailurePredicted as a
	// plain boolean with no null, so a firmware that does not implement it is
	// indistinguishable from one reporting a clear drive.
	VendorFailurePredicted bool
}

// Trendable reports whether there is enough evidence for WearPerDay and
// Acceleration to mean anything. Output formats that export the numbers rather
// than the prose have to ask the same question AnalyzeDriveWear asks, or they
// publish a rate it refused to claim.
func (t DriveWearTrend) Trendable() bool {
	return t.Samples >= minSamplesForWear &&
		t.Span >= minSpanForWear &&
		t.Steps >= minWearSteps
}

// Wearing reports whether the drive is actually spending media life, as opposed
// to holding an estimate that has not moved.
func (t DriveWearTrend) Wearing() bool {
	return t.Trendable() && t.WearPerDay > 0
}

// Projectable reports whether the rate rests on enough steps to put a
// replacement horizon on. Trendable is about whether there is a rate at all;
// this is about whether it is stable enough to extrapolate.
func (t DriveWearTrend) Projectable() bool {
	return t.Wearing() && t.Steps >= minStepsForProjection
}

// DaysToSpent projects when the estimate reaches zero at the current rate,
// reporting false when the projection is not justified.
//
// Projected from the latest reported reading rather than from a smoothed one,
// which is the opposite of what the thermal analyser does and for a reason: a
// temperature is pulled around by whatever the node is doing this minute, so the
// newest reading is the worst one to extrapolate from. An integral life estimate
// has no such noise — the newest value is exactly what the firmware believes.
func (t DriveWearTrend) DaysToSpent() (float64, bool) {
	if !t.Projectable() || t.LifeLeftPercent <= 0 {
		return 0, false
	}
	return t.LifeLeftPercent / t.WearPerDay, true
}

// AnalyzeDriveWear evaluates a drive's media-life estimate for predictive
// signal, alongside the vendor's own failure prediction for the same drive.
//
// This is the third shape the same method is applied to, and the one where it
// can be checked against a competing answer. A correctable-error count is a
// monotonic counter; a thermal margin is a continuous gauge that moves with
// load; this is a coarse, integral, monotonically declining estimate — and
// unlike the other two, the vendor ships its own verdict on the same part.
//
// The inversion is the same one. A drive reporting 20% of media life left that
// has reported 20% for a year is doing fine and will outlive the node. A drive
// that went 90% to 60% in a month will cross zero on a date you can read off the
// slope, and every absolute-threshold rule in the industry looks at the first and
// not the second.
//
// Three things make it different enough to be worth having:
//
//   - The signal is quantised in a way the other two are not. It steps by whole
//     percent and sits still between steps, so the evidence behind a rate is the
//     number of steps, not the number of readings. That is a different floor
//     from the thermal one, not a scaled version of it: bucket medians exist to
//     outvote amplitude noise, and there is no amplitude noise here — only
//     uncertainty about when each step happened. See minWearSteps for why a
//     magnitude floor, which would also have rejected the common case, is the
//     wrong instrument for it.
//   - It is an estimate, not a measurement, so every projection off it is
//     discounted. See vendorEstimateConfidence.
//   - The vendor's FailurePredicted boolean is the naive signal this project
//     argues against, on the same component, at the same moment. Both are
//     reported, and neither suppresses the other: a clear flag over a drive that
//     is visibly burning its media is the false negative this tool exists for.
//
// Returns a nil finding when there is nothing worth reporting.
func AnalyzeDriveWear(host string, s Series, vendorFailurePredicted bool) (DriveWearTrend, *Finding) {
	trend := DriveWearTrend{
		Component:              s.Component,
		VendorFailurePredicted: vendorFailurePredicted,
	}
	if len(s.Samples) == 0 {
		// The BMC reported no estimate for this drive. Absent is not 0% of life
		// left — those are opposite conditions, one being "no visibility" and the
		// other "the vendor says this drive is finished" — and the gap matters
		// more here than anywhere else in this project, because most firmware
		// populates the field on NVMe and omits it entirely on SAS and SATA.
		// Coercing absent to zero would condemn a chassis of healthy disks.
		//
		// A flagged drive is still reported, which is exactly the SAS case: the
		// firmware says it is failing and never gave an estimate to trend.
		return trend, vendorFailureFinding(host, s.Component, vendorFailurePredicted)
	}

	trend.LifeLeftPercent = s.Samples[len(s.Samples)-1].Value

	// Inverted once, here, so that the shared slope, acceleration and epoch
	// helpers all see "bigger is worse" exactly as they do for a counter. Doing
	// it at this boundary is why the grading below reads like gradeECC rather
	// than carrying a sign flip in every comparison.
	wear := wearOf(s.Samples)
	start, replacements := epochStartBeyond(wear, minReplacementDrop)
	epoch := wear[start:]

	trend.Replacements = replacements
	trend.Samples = len(epoch)
	trend.Span = epoch[len(epoch)-1].At.Sub(epoch[0].At)
	trend.Steps = stepsIn(epoch)

	// A drive the vendor's own model says is out of media life is a present fact,
	// not a prediction. It does not wait for a trend and it takes no estimate
	// discount: the discount is on extrapolating that model forward, not on
	// repeating what it already said.
	if trend.LifeLeftPercent <= 0 {
		return trend, &Finding{
			Host:      host,
			Component: s.Component,
			Kind:      "drive.media-spent",
			Severity:  SeverityUrgent,
			Summary: fmt.Sprintf("%s reports no media life left — replace it before it goes read-only or drops out of the array",
				s.Component),
			Evidence: []string{
				fmt.Sprintf("latest estimate %.0f%% of media life remaining", trend.LifeLeftPercent),
				vendorVerdict(vendorFailurePredicted, false),
			},
			Confidence: 1.0,
		}
	}

	if !trend.Trendable() {
		// Silent, like AnalyzeThermal and unlike AnalyzeECC. A non-zero error
		// count is abnormal on its own, so a thin ECC history still has something
		// to report. An estimate below 100% is not abnormal — it is every drive
		// that has ever been written to — so there is nothing to surface until the
		// trend is claimable. The vendor's flag still is.
		return trend, vendorFailureFinding(host, s.Component, vendorFailurePredicted)
	}

	// The shared helper works in hours because the other two signals do; this is
	// the single place the unit changes.
	perHour, r2 := leastSquaresPerHour(epoch)
	trend.WearPerDay = perHour * hoursPerDay
	trend.RSquared = r2
	trend.Acceleration = accelerationOf(epoch)

	sev, why := gradeDriveWear(trend)
	if sev == SeverityInfo {
		return trend, vendorFailureFinding(host, s.Component, vendorFailurePredicted)
	}

	ev := []string{
		fmt.Sprintf("%.0f%% of media life left, wearing %.2f%%/day", trend.LifeLeftPercent, trend.WearPerDay),
		// Steps first, readings second, because the first number is the evidence
		// and the second is only how often the BMC was asked.
		fmt.Sprintf("%d steps in the estimate over %s (%d readings)",
			trend.Steps, trend.Span.Round(time.Minute), trend.Samples),
		fmt.Sprintf("fit R²=%.2f", trend.RSquared),
	}
	if trend.Acceleration > 0 {
		ev = append(ev, fmt.Sprintf("recent half wearing %.1fx the earlier half", trend.Acceleration))
	}
	if trend.Replacements > 0 {
		ev = append(ev, fmt.Sprintf("%d drive replacement(s) seen in this bay — rate covers the current drive only", trend.Replacements))
	}
	if days, ok := trend.DaysToSpent(); ok {
		ev = append(ev, fmt.Sprintf("spent in %s if the rate holds — extrapolated from the vendor's own life estimate, not measured", aboutHorizon(days)))
	} else {
		ev = append(ev, fmt.Sprintf("no replacement horizon: %d steps, fewer than the %d a date is extrapolated from", trend.Steps, minStepsForProjection))
	}
	ev = append(ev, vendorVerdict(vendorFailurePredicted, true))

	return trend, &Finding{
		Host:      host,
		Component: s.Component,
		Kind:      "drive.wearing",
		Severity:  sev,
		Summary:   why,
		Evidence:  ev,
		Confidence: vendorEstimateConfidence * confidenceFor(fit{
			// Steps, not readings: a thousand identical readings carry no more
			// information about the rate than the handful of step edges between
			// them, so that is how many points the slope actually has under it.
			// Same reasoning as the thermal analyser counting buckets.
			Points: trend.Steps, RSquared: trend.RSquared,
			Resets: trend.Replacements, Span: trend.Span,
		}),
	}
}

// gradeDriveWear maps a wear trend onto a severity and a one-line explanation.
//
// Every branch requires the drive to actually be wearing. Acceleration on its
// own is not a finding here for the same reason it is not one on a thermal
// margin: a drive whose write load changed shape has not spent any more media
// than it had.
func gradeDriveWear(t DriveWearTrend) (Severity, string) {
	if !t.Wearing() {
		return SeverityInfo, ""
	}
	accelerating := t.Acceleration >= accelerationRatio
	days, projected := t.DaysToSpent()

	switch {
	case projected && days <= urgentDaysToSpent:
		return SeverityUrgent, fmt.Sprintf(
			"%s has %.0f%% media life left and is spent in %s at %.2f%%/day — order a replacement now",
			t.Component, t.LifeLeftPercent, aboutHorizon(days), t.WearPerDay)

	case projected && accelerating && days <= planDaysToSpent:
		return SeverityUrgent, fmt.Sprintf(
			"%s wearing %.1fx faster than it was and spent in %s — order a replacement now",
			t.Component, t.Acceleration, aboutHorizon(days))

	case projected && days <= planDaysToSpent:
		return SeverityPlan, fmt.Sprintf(
			"%s spent in %s at %.2f%%/day — schedule a swap",
			t.Component, aboutHorizon(days), t.WearPerDay)

	case accelerating:
		return SeverityPlan, fmt.Sprintf(
			"%s wearing %.1fx faster than it was (%.0f%% left) — schedule a swap",
			t.Component, t.Acceleration, t.LifeLeftPercent)

	default:
		return SeverityWatch, fmt.Sprintf(
			"%s wearing slowly — %.2f%%/day, %.0f%% left",
			t.Component, t.WearPerDay, t.LifeLeftPercent)
	}
}

// vendorFailureFinding reports the drive's own FailurePredicted flag, for the
// drives this analysis has nothing of its own to say about.
//
// It exists so the two signals never both describe one bay: where the trend
// fires, the agreement or disagreement is a line in its evidence instead. Two
// rows for one drive is how a report stops being read top-down.
//
// PLAN rather than URGENT, and at a lower confidence than a trend finding of the
// same severity, because this is a threshold somebody else chose, on a model this
// tool cannot inspect, with no rate behind it. It is still reported, because a
// vendor that has decided a drive is failing usually knows something — just not
// when.
func vendorFailureFinding(host, component string, flagged bool) *Finding {
	if !flagged {
		return nil
	}
	return &Finding{
		Host:      host,
		Component: component,
		Kind:      "drive.vendor-failure-predicted",
		Severity:  SeverityPlan,
		Summary: fmt.Sprintf("%s is flagged FailurePredicted by its own firmware — schedule a swap",
			component),
		Evidence: []string{
			"the drive's vendor-defined failure prediction, not a trend measured here",
			"no media-life trend of our own supports or contradicts it — too few steps, or no estimate reported",
		},
		Confidence: 0.4,
	}
}

// vendorVerdict renders the firmware's own prediction as an evidence line.
//
// Present in every drive finding, agreeing or not. Where the two signals
// disagree that is the single most interesting thing on the row — a cleared flag
// over a drive visibly burning its media is the false negative this analysis
// exists to catch — and where they agree, saying so is what stops an operator
// wondering whether the flag was even checked.
func vendorVerdict(flagged, haveTrend bool) string {
	switch {
	case flagged && haveTrend:
		return "vendor FailurePredicted=true — the firmware's own threshold agrees"
	case flagged:
		return "vendor FailurePredicted=true"
	case haveTrend:
		return "vendor FailurePredicted=false — the firmware disagrees, though Redfish cannot tell a false from an unimplemented field"
	default:
		return "vendor FailurePredicted=false, which Redfish does not distinguish from an unimplemented field"
	}
}

// wearOf converts reported media life left into media life spent, so that
// "bigger is worse" holds for the shared helpers.
//
// One slice per drive per poll — about 43KB on a fortnight of 15-minute
// readings. That is a handful of copies on most nodes, which is why this is a
// plain copy where the thermal path, running over every sensor in the chassis,
// collapses its series into buckets first. A twenty-four-bay storage node is the
// case where that stops being obviously true, and the number to watch is in the
// benchmark table.
//
// It cannot be avoided by negating the slope afterwards. The slope and the fit
// would survive that, but accelerationOf branches on the sign of the earlier
// half — flat-then-rising is a shape, not a ratio — so it has to be handed a
// series that already points the dangerous way.
func wearOf(samples []Sample) []Sample {
	if len(samples) == 0 {
		return nil
	}
	out := make([]Sample, len(samples))
	for i, s := range samples {
		out[i] = Sample{At: s.At, Value: fullLifePercent - s.Value}
	}
	return out
}

// stepsIn counts how many times the series moved to a worse value.
//
// This is the sample count that matters on a quantised signal. A drive polled
// every fifteen minutes for a fortnight returns 1,344 readings and perhaps
// fourteen distinct values; the fourteen are the measurements and the rest is
// the same measurement repeated.
func stepsIn(samples []Sample) int {
	var steps int
	for i := 1; i < len(samples); i++ {
		if samples[i].Value > samples[i-1].Value {
			steps++
		}
	}
	return steps
}

// aboutHorizon renders a projected replacement horizon the way an operator plans
// one: days inside a fortnight, whole weeks out to a few months, whole months
// beyond.
//
// Coarser than the thermal analyser's aboutDuration, deliberately. That one
// extrapolates a measured temperature slope and a crossing a couple of days out
// is worth stating in hours. This one extrapolates a vendor's endurance model,
// and a replacement date to the day off somebody else's guess is a precision
// nobody has.
func aboutHorizon(days float64) string {
	switch {
	case days < 14:
		return fmt.Sprintf("~%.0fd", days)
	case days < 120:
		return fmt.Sprintf("~%.0fw", math.Round(days/7))
	default:
		return fmt.Sprintf("~%.0fmo", math.Round(days/daysPerMonth))
	}
}
