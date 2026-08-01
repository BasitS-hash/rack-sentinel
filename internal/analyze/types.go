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

// Sample is one observation of a monotonic counter at a point in time.
//
// Counters from a BMC reset when the host power-cycles, so a decrease is not an
// error — it is a new epoch. Callers must not assume the series is monotonic;
// the analysis handles resets explicitly.
type Sample struct {
	At    time.Time
	Value float64
}

// Series is a time-ordered set of samples for one counter on one component.
type Series struct {
	// Component identifies the physical part, e.g. "DIMM_A1" or "Disk.Bay.3".
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

// Report is the full result of analysing one fleet poll.
type Report struct {
	GeneratedAt time.Time
	HostsPolled int
	HostsFailed int
	Findings    []Finding
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
