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
			key := "ecc.correctable/" + dimm
			byComponent[key] = appendCapped(byComponent[key], analyze.Sample{
				At: t.CollectedAt, Value: count,
			})
		}
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
			components[key] = analyze.Prune(samples, cutoff)
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

	// One trend per counter, including the flat ones: the metrics exporter needs
	// the numbers behind every series, not only the ones that produced a
	// finding. Preallocated because at fleet scale this is one per DIMM.
	report.Trends = make([]analyze.HostTrend, 0, series)

	for _, host := range hosts {
		components := h.Series[host]
		keys := make([]string, 0, len(components))
		for k := range components {
			keys = append(keys, k)
		}
		sort.Strings(keys)

		for _, key := range keys {
			name, isECC := strings.CutPrefix(key, "ecc.correctable/")
			if !isECC {
				continue
			}
			trend, finding := analyze.AnalyzeECC(host, analyze.Series{
				Component: name,
				Samples:   components[key],
			})
			report.Trends = append(report.Trends, analyze.HostTrend{Host: host, Trend: trend})
			if finding != nil {
				report.Findings = append(report.Findings, *finding)
			}
		}
	}

	return report
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
