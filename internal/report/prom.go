// Package report renders an analysed fleet poll for other programs to consume.
//
// The human-readable report lives with the CLI, because its shape is a CLI
// concern and it can change whenever it reads better. What lives here is output
// something else parses, where the shape is a contract and getting it subtly
// wrong is worse than not emitting it at all.
package report

import (
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"

	"github.com/BasitS-hash/rack-sentinel/internal/analyze"
	"github.com/BasitS-hash/rack-sentinel/internal/atomicfile"
)

// Metric names, declared once so the HELP, TYPE and sample lines of a family
// cannot drift apart. Prefix and unit suffixes follow the Prometheus naming
// conventions: a counter ends in _total, a ratio in _ratio, seconds in _seconds.
const (
	mPollTimestamp  = "rack_sentinel_poll_timestamp_seconds"
	mHostsPolled    = "rack_sentinel_hosts_polled"
	mHostsFailed    = "rack_sentinel_hosts_failed"
	mBMCUnreachable = "rack_sentinel_bmc_unreachable"
	mECCTotal       = "rack_sentinel_ecc_correctable_errors_total"
	mECCRate        = "rack_sentinel_ecc_correctable_errors_per_hour"
	mECCAccel       = "rack_sentinel_ecc_acceleration_ratio"
	mThermalMargin  = "rack_sentinel_thermal_margin_celsius"
	mThermalShrink  = "rack_sentinel_thermal_margin_shrink_celsius_per_hour"
	mThermalAccel   = "rack_sentinel_thermal_shrink_acceleration_ratio"
	mThermalHours   = "rack_sentinel_thermal_hours_to_critical"
	mSeverity       = "rack_sentinel_finding_severity"
	mConfidence     = "rack_sentinel_finding_confidence_ratio"
)

// label is one label on one sample.
type label struct {
	name  string
	value string
}

// WriteProm renders the report in the Prometheus text exposition format.
func WriteProm(w io.Writer, r analyze.Report) error {
	if _, err := w.Write(promText(r)); err != nil {
		return fmt.Errorf("write metrics: %w", err)
	}
	return nil
}

// WritePromFile renders the report to path for node_exporter's textfile
// collector, replacing the previous file in one rename.
//
// node_exporter reads the file on its own schedule and will happily read one
// mid-write, so this is not a nicety: a partial file is a parse error that drops
// every metric in it, which is indistinguishable from the fleet being quiet.
func WritePromFile(path string, r analyze.Report) error {
	// 0644 because the collector usually runs as its own user, and a file it
	// cannot read is the same outage as a file that was never written.
	if err := atomicfile.Write(path, promText(r), 0o644); err != nil {
		return fmt.Errorf("write metrics %s: %w", path, err)
	}
	return nil
}

// promText renders the whole exposition.
//
// What is absent from the output matters as much as what is in it. A counter the
// BMC never returned has no series here at all, and a rate the analyser refused
// to claim is not published as 0 — in a metrics pipeline a fabricated zero is
// not a missing measurement, it is a healthy DIMM, which is the one conclusion
// this tool exists to avoid reaching by accident.
func promText(r analyze.Report) []byte {
	var b strings.Builder

	// Sorted copies: identical state must produce an identical file so a diff of
	// two polls shows only what changed, and the caller's report is shared with
	// the other output formats.
	trends := append([]analyze.HostTrend(nil), r.Trends...)
	sort.SliceStable(trends, func(i, j int) bool {
		if trends[i].Host != trends[j].Host {
			return trends[i].Host < trends[j].Host
		}
		return trends[i].Trend.Component < trends[j].Trend.Component
	})
	thermal := append([]analyze.HostThermalTrend(nil), r.ThermalTrends...)
	sort.SliceStable(thermal, func(i, j int) bool {
		if thermal[i].Host != thermal[j].Host {
			return thermal[i].Host < thermal[j].Host
		}
		return thermal[i].Trend.Component < thermal[j].Trend.Component
	})
	findings := append([]analyze.Finding(nil), r.Findings...)
	sort.SliceStable(findings, func(i, j int) bool {
		if findings[i].Host != findings[j].Host {
			return findings[i].Host < findings[j].Host
		}
		if findings[i].Component != findings[j].Component {
			return findings[i].Component < findings[j].Component
		}
		return findings[i].Kind < findings[j].Kind
	})

	// Freshness is exported as a value because it cannot be exported as one:
	// node_exporter's textfile collector rejects a file whose samples carry
	// explicit timestamps, so every sample below is stamped at scrape time
	// regardless of when the BMC was actually read.
	declare(&b, mPollTimestamp, "gauge",
		"Unix time of the poll this file was written from; alert on it going stale.")
	sample(&b, mPollTimestamp, float64(r.GeneratedAt.Unix()))

	declare(&b, mHostsPolled, "gauge", "Hosts attempted in this poll.")
	sample(&b, mHostsPolled, float64(r.HostsPolled))

	declare(&b, mHostsFailed, "gauge", "Hosts whose BMC could not be reached in this poll.")
	sample(&b, mHostsFailed, float64(r.HostsFailed))

	// Only the hosts that failed get a series. A host that answered is reported
	// by the fleet totals above; inventing a 0 for every reachable host would
	// double the file for a value nothing alerts on.
	declare(&b, mBMCUnreachable, "gauge",
		"1 when the BMC did not answer this poll. Hosts that answered are absent, not 0.")
	for _, f := range findings {
		if f.Kind == "bmc.unreachable" {
			sample(&b, mBMCUnreachable, 1, label{"host", f.Host})
		}
	}

	declare(&b, mECCTotal, "counter",
		"Correctable ECC errors the BMC reported for this DIMM in the current counter epoch.")
	for _, t := range trends {
		// No samples means no counter value, not a zero one.
		if t.Trend.Samples == 0 {
			continue
		}
		sample(&b, mECCTotal, t.Trend.Total, label{"host", t.Host}, label{"dimm", t.Trend.Component})
	}

	declare(&b, mECCRate, "gauge",
		"Least-squares correctable-error rate over the current epoch. Absent when the epoch is too short to trend.")
	for _, t := range trends {
		if !t.Trend.Trendable() {
			continue
		}
		sample(&b, mECCRate, t.Trend.RatePerHour, label{"host", t.Host}, label{"dimm", t.Trend.Component})
	}

	declare(&b, mECCAccel, "gauge",
		"Recent-half error rate divided by the earlier-half rate; above 1 means degrading faster. Absent when it cannot be computed.")
	for _, t := range trends {
		if !t.Trend.Trendable() || t.Trend.Acceleration == 0 {
			continue
		}
		sample(&b, mECCAccel, t.Trend.Acceleration, label{"host", t.Host}, label{"dimm", t.Trend.Component})
	}

	// The same absence rule as the counters above, and the inversion matters more
	// here: 0 is not a neutral value for a margin, it is a sensor sitting exactly
	// on the threshold the vendor said not to cross. A sensor the BMC stopped
	// reporting must have no series at all.
	declare(&b, mThermalMargin, "gauge",
		"Degrees Celsius of headroom before this sensor's upper critical threshold; negative means exceeded. Sensors the BMC did not report are absent, not 0.")
	for _, t := range thermal {
		if t.Trend.Samples == 0 {
			continue
		}
		sample(&b, mThermalMargin, t.Trend.Margin, label{"host", t.Host}, label{"sensor", t.Trend.Component})
	}

	declare(&b, mThermalShrink, "gauge",
		"Degrees of headroom lost per hour, from the smoothed fit; positive means closing. Absent when the history is too short to trend.")
	for _, t := range thermal {
		if !t.Trend.Trendable() {
			continue
		}
		sample(&b, mThermalShrink, t.Trend.ShrinkPerHour, label{"host", t.Host}, label{"sensor", t.Trend.Component})
	}

	declare(&b, mThermalAccel, "gauge",
		"Recent-half shrink rate divided by the earlier-half rate; above 1 means the margin is closing faster than it was. Absent unless the margin is closing.")
	for _, t := range thermal {
		// Gated on the margin actually closing, not merely on the ratio being
		// computable. On a sensor that is only swinging with load the earlier half
		// is flat and the recent half happens to be rising, which the ratio
		// reports as its 999 cap — a number nothing should be alerted on, and one
		// that flips back next poll.
		if !t.Trend.Trendable() || !t.Trend.Shrinking() || t.Trend.Acceleration == 0 {
			continue
		}
		sample(&b, mThermalAccel, t.Trend.Acceleration, label{"host", t.Host}, label{"sensor", t.Trend.Component})
	}

	// Published only where the analysis would publish it. A crossing date is the
	// number an operator plans around, so a fabricated one is worse than none:
	// this is absent for a sensor whose readings no straight line described, even
	// though dividing the margin by the slope would always produce a figure.
	declare(&b, mThermalHours, "gauge",
		"Projected hours until this sensor reaches upper critical at the current rate. Absent unless the margin is closing and the fit supports extrapolation.")
	for _, t := range thermal {
		hours, ok := t.Trend.HoursToCritical()
		if !ok {
			continue
		}
		sample(&b, mThermalHours, hours, label{"host", t.Host}, label{"sensor", t.Trend.Component})
	}

	declare(&b, mSeverity, "gauge",
		"Severity of an open finding: 1 watch, 2 plan, 3 urgent.")
	for _, f := range findings {
		sample(&b, mSeverity, float64(f.Severity),
			label{"host", f.Host}, label{"component", f.Component}, label{"kind", f.Kind})
	}

	declare(&b, mConfidence, "gauge",
		"How far a finding should be believed, 0..1. Thin history and bursty counters lower it.")
	for _, f := range findings {
		sample(&b, mConfidence, f.Confidence,
			label{"host", f.Host}, label{"component", f.Component}, label{"kind", f.Kind},
			label{"severity", f.Severity.String()})
	}

	return []byte(b.String())
}

// declare writes the two lines that open a metric family.
//
// Exactly one HELP and one TYPE per family: node_exporter rejects a file with a
// duplicate or an undeclared family, and it rejects the whole file.
func declare(b *strings.Builder, name, kind, help string) {
	if b.Len() > 0 {
		b.WriteByte('\n')
	}
	fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, kind)
}

// sample writes one sample line, labels in the order given.
func sample(b *strings.Builder, name string, v float64, labels ...label) {
	b.WriteString(name)
	for i, l := range labels {
		if i == 0 {
			b.WriteByte('{')
		} else {
			b.WriteByte(',')
		}
		b.WriteString(l.name)
		b.WriteString(`="`)
		b.WriteString(escapeLabel(l.value))
		b.WriteByte('"')
	}
	if len(labels) > 0 {
		b.WriteByte('}')
	}
	b.WriteByte(' ')
	b.WriteString(formatValue(v))
	b.WriteByte('\n')
}

// escapeLabel escapes a label value as the text format requires: backslash,
// double quote, newline.
//
// Vendors have shipped component identifiers containing all three. An unescaped
// one does not corrupt its own sample, it corrupts the file from that line on.
func escapeLabel(s string) string {
	if !strings.ContainsAny(s, "\\\"\n") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 8)
	for _, r := range s {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case '\n':
			b.WriteString(`\n`)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// formatValue renders a sample value.
//
// 'f' rather than 'g' keeps counters legible as integers instead of exponent
// notation, and spells NaN and the infinities the way the text format wants
// them.
func formatValue(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}
