// Package analyze turns raw BMC telemetry into ranked, actionable hardware findings.
//
// The distinction this package exists to make: a component that is *degrading*
// matters more than one that is merely *dirty*. A DIMM sitting at ten thousand
// correctable errors that has been flat for a year is doing its job — ECC caught
// them. A DIMM at two hundred that doubled this week is on its way to an
// uncorrectable error and an unplanned node outage. Threshold alerting reports
// the first and misses the second, which is backwards.
package analyze

import "time"

// Severity ranks a finding by how soon it needs a human.
type Severity int

const (
	// SeverityInfo is a normal observation worth recording but not acting on.
	SeverityInfo Severity = iota
	// SeverityWatch means a trend is visible but not yet urgent — recheck later.
	SeverityWatch
	// SeverityPlan means schedule a replacement in the next maintenance window.
	SeverityPlan
	// SeverityUrgent means act before the next window; failure is likely sooner.
	SeverityUrgent
)

func (s Severity) String() string {
	switch s {
	case SeverityUrgent:
		return "URGENT"
	case SeverityPlan:
		return "PLAN"
	case SeverityWatch:
		return "WATCH"
	default:
		return "INFO"
	}
}

// Sample is one observation of one measurement at a point in time.
//
// Counters from a BMC reset when the host power-cycles, so a decrease is not an
// error — it is a new epoch. Callers must not assume the series is monotonic;
// the analysis handles resets explicitly. A gauge, such as a thermal margin,
// moves in both directions all day and has no epochs at all. A vendor-computed
// estimate, such as a drive's media life left, declines in coarse steps and has
// epochs that are not as clean as a counter's — the firmware can revise it
// upward without a drive having been swapped. That is why the three are analysed
// and pruned by different rules.
type Sample struct {
	At    time.Time
	Value float64
}

// Series is a time-ordered set of samples for one measurement on one component.
//
// The series does not say which of the three it is. That is the caller's
// business: it knows whether it is handing over a counter, a gauge or a vendor
// estimate, and handing one to the wrong analyser is a mistake no amount of
// inspection at this level could catch — a falling counter, a falling margin and
// a falling life estimate look identical here.
type Series struct {
	// Component identifies the physical part, e.g. "DIMM_A1" or "CPU0_Die".
	Component string
	Samples   []Sample
}

// Finding is one actionable conclusion about one component.
type Finding struct {
	Host      string
	Component string
	Kind      string
	Severity  Severity

	// Summary is a single line an operator can act on without reading further.
	Summary string

	// Evidence lists the specific numbers behind the conclusion, so a human can
	// disagree with the tool rather than having to trust it.
	Evidence []string

	// Confidence is 0..1. Short or noisy series produce low confidence even when
	// the trend looks alarming, because they should.
	Confidence float64
}

// HostTrend is one counter's trend together with the host it belongs to.
type HostTrend struct {
	Host  string
	Trend ECCTrend
}

// HostThermalTrend is one sensor's margin trend together with its host.
//
// Separate from HostTrend rather than an interface over both: the two trends
// share their reasoning, not their fields, and the exporter publishes different
// metric families from each. A common interface would only move the type switch
// somewhere less obvious.
type HostThermalTrend struct {
	Host  string
	Trend ThermalTrend
}

// HostDriveTrend is one drive's media-wear trend together with its host.
//
// A third sibling rather than a third case in a type switch, for the reason
// given above: the three trends share the reasoning behind them and almost none
// of their fields, and the exporter publishes a different metric family from
// each.
type HostDriveTrend struct {
	Host  string
	Trend DriveWearTrend
}

// Report is the full result of analysing one fleet poll.
type Report struct {
	GeneratedAt time.Time
	HostsPolled int
	HostsFailed int
	Findings    []Finding

	// Trends carries the numbers behind the findings — and behind the counters
	// that produced none — for output formats that export measurements rather
	// than prose. It is excluded from the JSON report, whose shape is already a
	// contract with whatever is parsing it.
	Trends []HostTrend `json:"-"`

	// ThermalTrends carries the margin trends, for the same reason and with the
	// same exclusion from the JSON report.
	ThermalTrends []HostThermalTrend `json:"-"`

	// DriveTrends carries the drive media-wear trends, likewise. It also carries
	// the drives that have no trend at all but whose firmware flagged them, so
	// that the vendor's own prediction is exportable for a drive this analysis
	// could not speak about.
	DriveTrends []HostDriveTrend `json:"-"`
}

// Worst returns the highest severity present, or SeverityInfo when clean.
func (r Report) Worst() Severity {
	worst := SeverityInfo
	for _, f := range r.Findings {
		if f.Severity > worst {
			worst = f.Severity
		}
	}
	return worst
}
