package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/BasitS-hash/rack-sentinel/internal/analyze"
	"github.com/BasitS-hash/rack-sentinel/internal/atomicfile"
	"github.com/BasitS-hash/rack-sentinel/internal/collect"
)

// maxSamplesPerSeries bounds retained history per counter.
//
// Trends care about shape, not archaeology. At a 15-minute poll this keeps
// roughly three weeks — long enough to see a DIMM turn, short enough that the
// state file stays small on a fleet of thousands.
const maxSamplesPerSeries = 2000

// defaultRetention is how far back a run keeps history when -retain is unset.
//
// The sample cap above bounds one counter; it says nothing about how many
// counters the file holds, and a fleet accumulates those permanently — a
// decommissioned node keeps its series forever otherwise. Two weeks leaves a
// week on each side of the acceleration split and still lets a node that left
// the rack fall out of the file inside a sprint.
const defaultRetention = 14 * 24 * time.Hour

// Series key prefixes. One file holds both signals, and the prefix is what says
// which analyser and which retention rule a series belongs to — so it is
// declared once rather than spelled out at each of the three places that reads
// it.
const (
	keyECC           = "ecc.correctable/"
	keyThermalMargin = "thermal.margin/"
	keyDriveLife     = "drive.life/"
)

// history is the persisted per-counter time series across runs.
//
// Keyed host -> component -> samples. Without it every run would see a single
// point and could report only levels, never whether something is degrading —
// which is the entire question this tool exists to answer.
type history struct {
	Series map[string]map[string][]analyze.Sample `json:"series"`

	// polled and failed describe the run just completed, not the file.
	polled int
	failed int
}

func newHistory() *history {
	return &history{Series: map[string]map[string][]analyze.Sample{}}
}

// loadHistory reads a state file. A missing file is not an error — the first run
// legitimately has no history.
func loadHistory(path string) (*history, error) {
	h := newHistory()
	if path == "" {
		return h, nil
	}

	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return h, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read history: %w", err)
	}
	if len(data) == 0 {
		return h, nil
	}
	if err := json.Unmarshal(data, h); err != nil {
		return nil, fmt.Errorf("parse history %s: %w", path, err)
	}
	if h.Series == nil {
		h.Series = map[string]map[string][]analyze.Sample{}
	}
	return h, nil
}

// append folds one poll's telemetry into the series.
func (h *history) append(telemetry []collect.HostTelemetry) {
	for _, t := range telemetry {
		h.polled++
		if t.Err != nil {
			h.failed++
			// A failed poll contributes no samples. Recording a zero would look
			// like a counter reset and corrupt the trend.
			continue
		}

		byComponent, ok := h.Series[t.Host]
		if !ok {
			byComponent = map[string][]analyze.Sample{}
			h.Series[t.Host] = byComponent
		}

		for dimm, count := range t.CorrectableErrors {
			byComponent[keyECC+dimm] = appendCapped(byComponent[keyECC+dimm], analyze.Sample{
				At: t.CollectedAt, Value: count,
			})
		}

		// Only the sensors this BMC actually reported. A sensor absent from the
		// map was not read as 0°C of margin — that would be a sensor sitting
		// exactly on its critical threshold, which is the opposite condition.
		for sensor, margin := range t.ThermalMargins {
			key := keyThermalMargin + sensor
			byComponent[key] = appendCapped(byComponent[key], analyze.Sample{
				At: t.CollectedAt, Value: margin,
			})
		}

		// Same rule again, and the consequence of breaking it is worse here: a
		// drive absent from the map reported no media-life estimate, and 0% of
		// media life left is a spent drive. Most firmware omits the field
		// entirely on SAS and SATA.
		for drive, lifeLeft := range t.MediaLifeLeft {
			key := keyDriveLife + drive
			byComponent[key] = appendCapped(byComponent[key], analyze.Sample{
				At: t.CollectedAt, Value: lifeLeft,
			})
		}

		// FailurePredicted is deliberately not persisted. It is the vendor's
		// verdict right now, not a measurement with a history: trending a boolean
		// would say only how many polls ago the firmware changed its mind, and
		// the current poll already carries it to the analysis.
	}
}

func appendCapped(samples []analyze.Sample, s analyze.Sample) []analyze.Sample {
	samples = append(samples, s)
	if len(samples) > maxSamplesPerSeries {
		samples = samples[len(samples)-maxSamplesPerSeries:]
	}
	return samples
}

// prune drops history outside the retention window.
//
// It runs between the append and the analysis, so what a run reports is what the
// next run will still have: pruning afterwards would report a trend computed
// from samples the following poll has already thrown away, and the verdict would
// flip for no reason an operator could see.
//
// retain <= 0 keeps everything.
func (h *history) prune(now time.Time, retain time.Duration) {
	if retain <= 0 {
		return
	}
	cutoff := now.Add(-retain)

	for host, components := range h.Series {
		for key, samples := range components {
			// A series whose newest sample predates the window describes hardware
			// nobody can see any more: a decommissioned node, a renamed DIMM, a
			// BMC dark for weeks. Picking its trend back up later would mean
			// trending across the gap, so the whole series goes.
			if len(samples) == 0 || samples[len(samples)-1].At.Before(cutoff) {
				delete(components, key)
				continue
			}
			switch {
			case strings.HasPrefix(key, keyECC):
				components[key] = analyze.PruneCounter(samples, cutoff)
			case strings.HasPrefix(key, keyThermalMargin):
				components[key] = analyze.PruneGauge(samples, cutoff)
			case strings.HasPrefix(key, keyDriveLife):
				components[key] = analyze.PruneWear(samples, cutoff)
			}
			// A key this version does not recognise is left whole. It was written
			// by a different build and trimming it by rules that may not apply to
			// it is worse than carrying it — the stale-series check above still
			// removes it once nothing is refreshing it.
		}
		if len(components) == 0 {
			delete(h.Series, host)
		}
	}
}

// analyze runs every retained series through the analysers.
func (h *history) analyze(telemetry []collect.HostTelemetry) analyze.Report {
	report := analyze.Report{
		GeneratedAt: time.Now(),
		HostsPolled: h.polled,
		HostsFailed: h.failed,
	}

	// An unreachable BMC is itself an operational finding — a node nobody can
	// see is not a node that is fine.
	for _, t := range telemetry {
		if t.Err != nil {
			report.Findings = append(report.Findings, analyze.Finding{
				Host:       t.Host,
				Component:  "bmc",
				Kind:       "bmc.unreachable",
				Severity:   analyze.SeverityWatch,
				Summary:    fmt.Sprintf("%s BMC unreachable — no hardware visibility", t.Host),
				Evidence:   []string{t.Err.Error()},
				Confidence: 1.0,
			})
		}
	}

	// Deterministic ordering so repeated runs over identical state produce
	// identical output, which matters for diffing and for tests.
	hosts := make([]string, 0, len(h.Series))
	series := 0
	for host, components := range h.Series {
		hosts = append(hosts, host)
		series += len(components)
	}
	sort.Strings(hosts)

	// One trend per series, including the flat ones: the metrics exporter needs
	// the numbers behind every series, not only the ones that produced a
	// finding. Preallocated because at fleet scale this is one per DIMM, sensor
	// and drive bay.
	report.Trends = make([]analyze.HostTrend, 0, series)
	report.ThermalTrends = make([]analyze.HostThermalTrend, 0, series)
	report.DriveTrends = make([]analyze.HostDriveTrend, 0, series)

	// The vendor's own failure prediction comes from this poll rather than from
	// the file, so it has to be indexed before the series are walked.
	flagged := flaggedDrives(telemetry)

	for _, host := range hosts {
		components := h.Series[host]
		keys := make([]string, 0, len(components))
		for k := range components {
			keys = append(keys, k)
		}
		sort.Strings(keys)

		for _, key := range keys {
			var finding *analyze.Finding

			switch name, prefix := trimKey(key); prefix {
			case keyECC:
				trend, f := analyze.AnalyzeECC(host, analyze.Series{
					Component: name,
					Samples:   components[key],
				})
				report.Trends = append(report.Trends, analyze.HostTrend{Host: host, Trend: trend})
				finding = f

			case keyThermalMargin:
				trend, f := analyze.AnalyzeThermal(host, analyze.Series{
					Component: name,
					Samples:   components[key],
				})
				report.ThermalTrends = append(report.ThermalTrends,
					analyze.HostThermalTrend{Host: host, Trend: trend})
				finding = f

			case keyDriveLife:
				trend, f := analyze.AnalyzeDriveWear(host, analyze.Series{
					Component: name,
					Samples:   components[key],
				}, flagged[host][name])
				report.DriveTrends = append(report.DriveTrends,
					analyze.HostDriveTrend{Host: host, Trend: trend})
				finding = f
				// Covered by the series walk, so the sweep below must not report
				// this bay a second time.
				delete(flagged[host], name)

			default:
				continue
			}

			if finding != nil {
				report.Findings = append(report.Findings, *finding)
			}
		}
	}

	// The drives the firmware flagged that have no series to walk. This is the
	// common SAS and SATA case — the vendor predicts a failure and never
	// populates a media-life estimate — and without this sweep the only signal
	// anybody has on those drives would be dropped for having nothing to trend.
	for _, host := range sortedKeys(flagged) {
		for _, drive := range sortedKeys(flagged[host]) {
			trend, f := analyze.AnalyzeDriveWear(host, analyze.Series{Component: drive}, true)
			report.DriveTrends = append(report.DriveTrends,
				analyze.HostDriveTrend{Host: host, Trend: trend})
			if f != nil {
				report.Findings = append(report.Findings, *f)
			}
		}
	}

	return report
}

// flaggedDrives indexes this poll's vendor failure predictions by host and
// drive.
//
// Copied rather than read through to the telemetry because analyze deletes from
// it as the series walk covers each bay, and mutating the caller's collected
// telemetry to track bookkeeping would be a surprising thing for a function
// named analyze to do.
func flaggedDrives(telemetry []collect.HostTelemetry) map[string]map[string]bool {
	out := map[string]map[string]bool{}
	for _, t := range telemetry {
		for drive, predicted := range t.FailurePredicted {
			if !predicted {
				continue
			}
			if out[t.Host] == nil {
				out[t.Host] = map[string]bool{}
			}
			out[t.Host][drive] = true
		}
	}
	return out
}

// sortedKeys returns m's keys in order, so that repeated runs over identical
// state produce identical output.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// trimKey splits a series key into the component name and the prefix that says
// what kind of measurement it is. An unrecognised key returns an empty prefix.
func trimKey(key string) (name, prefix string) {
	for _, p := range []string{keyECC, keyThermalMargin, keyDriveLife} {
		if name, ok := strings.CutPrefix(key, p); ok {
			return name, p
		}
	}
	return "", ""
}

// save writes the state file atomically.
//
// A truncated state file loses every trend the fleet has accumulated, which is
// why this goes through atomicfile rather than os.WriteFile.
func (h *history) save(path string) error {
	data, err := json.Marshal(h)
	if err != nil {
		return fmt.Errorf("encode history: %w", err)
	}
	// 0600: no credentials in here, but a fleet's hardware inventory is nobody
	// else's business either.
	if err := atomicfile.Write(path, data, 0o600); err != nil {
		return fmt.Errorf("save history: %w", err)
	}
	return nil
}
