package report

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BasitS-hash/rack-sentinel/internal/analyze"
)

var generatedAt = time.Date(2026, 8, 1, 18, 4, 11, 0, time.UTC)

// render writes a report and returns the exposition text.
func render(t *testing.T, r analyze.Report) string {
	t.Helper()
	var buf bytes.Buffer
	if err := WriteProm(&buf, r); err != nil {
		t.Fatalf("WriteProm: %v", err)
	}
	return buf.String()
}

// samplesOf returns the sample lines of one metric family.
func samplesOf(text, metric string) []string {
	var out []string
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, metric+"{") || line == metric || strings.HasPrefix(line, metric+" ") {
			out = append(out, line)
		}
	}
	return out
}

// degradingReport is the fleet from the README: one DIMM turning, one dark BMC.
func degradingReport() analyze.Report {
	return analyze.Report{
		GeneratedAt: generatedAt,
		HostsPolled: 512,
		HostsFailed: 1,
		Findings: []analyze.Finding{
			{
				Host: "node-114", Component: "DIMM_B2", Kind: "ecc.degrading",
				Severity: analyze.SeverityUrgent, Confidence: 0.94,
			},
			{
				Host: "node-207", Component: "bmc", Kind: "bmc.unreachable",
				Severity: analyze.SeverityWatch, Confidence: 1.0,
			},
		},
		Trends: []analyze.HostTrend{{
			Host: "node-114",
			Trend: analyze.ECCTrend{
				Component: "DIMM_B2", RatePerHour: 41.3, Acceleration: 7.9,
				RSquared: 0.97, Total: 2914, Span: 71 * time.Hour, Samples: 284,
			},
		}},
	}
}

func TestWriteProm_OnlyCountersTheBMCReportedAppear(t *testing.T) {
	// The project's central invariant, carried into the metrics layer. A host has
	// sixteen DIMM slots; this BMC reported a counter for one of them. The other
	// fifteen must be absent from the exposition, not exported as 0 — a zero here
	// is indistinguishable from healthy hardware to every alert downstream.
	text := render(t, degradingReport())

	got := samplesOf(text, "rack_sentinel_ecc_correctable_errors_total")
	if len(got) != 1 {
		t.Fatalf("got %d correctable-error samples, want exactly the 1 counter that was reported:\n%s",
			len(got), strings.Join(got, "\n"))
	}
	if !strings.Contains(got[0], `dimm="DIMM_B2"`) || !strings.HasSuffix(got[0], " 2914") {
		t.Errorf("unexpected sample: %s", got[0])
	}
}

func TestWriteProm_UncomputableRateIsAbsentNotZero(t *testing.T) {
	// Same invariant one level up. Two samples over ten minutes cannot support a
	// rate claim, and the analyser refuses to make one. Exporting 0.0 errors/hour
	// would be the metrics layer inventing the claim the analyser declined to.
	r := analyze.Report{
		GeneratedAt: generatedAt,
		HostsPolled: 1,
		Trends: []analyze.HostTrend{{
			Host: "node-9",
			Trend: analyze.ECCTrend{
				Component: "DIMM_A1", Total: 7, Samples: 2, Span: 10 * time.Minute,
			},
		}},
	}

	text := render(t, r)

	if got := samplesOf(text, "rack_sentinel_ecc_correctable_errors_per_hour"); len(got) != 0 {
		t.Errorf("an untrendable series exported a rate: %s", strings.Join(got, "\n"))
	}
	if got := samplesOf(text, "rack_sentinel_ecc_acceleration_ratio"); len(got) != 0 {
		t.Errorf("an untrendable series exported an acceleration: %s", strings.Join(got, "\n"))
	}
	// The counter itself was reported, so it is exported.
	if got := samplesOf(text, "rack_sentinel_ecc_correctable_errors_total"); len(got) != 1 {
		t.Errorf("the reported counter went missing: %s", strings.Join(got, "\n"))
	}
}

func TestWriteProm_ReportedZeroIsExported(t *testing.T) {
	// The other half of the invariant: a BMC that reported zero correctable
	// errors said something, and dropping it would lose a healthy DIMM from the
	// fleet's denominator. Absent and zero are different, in both directions.
	r := analyze.Report{
		GeneratedAt: generatedAt,
		HostsPolled: 1,
		Trends: []analyze.HostTrend{{
			Host:  "node-9",
			Trend: analyze.ECCTrend{Component: "DIMM_A1", Total: 0, Samples: 96, Span: 24 * time.Hour},
		}},
	}

	got := samplesOf(render(t, r), "rack_sentinel_ecc_correctable_errors_total")
	if len(got) != 1 || !strings.HasSuffix(got[0], " 0") {
		t.Errorf("a reported zero was not exported: %q", got)
	}
}

func TestWriteProm_ThesisNumbersAreExported(t *testing.T) {
	text := render(t, degradingReport())

	for metric, want := range map[string]string{
		"rack_sentinel_ecc_correctable_errors_per_hour": `rack_sentinel_ecc_correctable_errors_per_hour{host="node-114",dimm="DIMM_B2"} 41.3`,
		"rack_sentinel_ecc_acceleration_ratio":          `rack_sentinel_ecc_acceleration_ratio{host="node-114",dimm="DIMM_B2"} 7.9`,
		"rack_sentinel_finding_confidence_ratio":        `rack_sentinel_finding_confidence_ratio{host="node-114",component="DIMM_B2",kind="ecc.degrading",severity="URGENT"} 0.94`,
		"rack_sentinel_finding_severity":                `rack_sentinel_finding_severity{host="node-114",component="DIMM_B2",kind="ecc.degrading"} 3`,
		"rack_sentinel_bmc_unreachable":                 `rack_sentinel_bmc_unreachable{host="node-207"} 1`,
		"rack_sentinel_hosts_polled":                    `rack_sentinel_hosts_polled 512`,
		"rack_sentinel_hosts_failed":                    `rack_sentinel_hosts_failed 1`,
		"rack_sentinel_poll_timestamp_seconds":          `rack_sentinel_poll_timestamp_seconds 1785607451`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("%s: missing\n want %s\n got %s", metric, want,
				strings.Join(samplesOf(text, metric), "\n"))
		}
	}
}

func TestWriteProm_EveryFamilyIsDeclaredExactlyOnce(t *testing.T) {
	// node_exporter rejects a file with a duplicated or missing TYPE line, and it
	// rejects the whole file, not the offending family.
	text := render(t, degradingReport())

	declared := map[string]int{}
	typed := map[string]int{}
	for _, line := range strings.Split(text, "\n") {
		fields := strings.Fields(line)
		switch {
		case strings.HasPrefix(line, "# HELP ") && len(fields) >= 4:
			declared[fields[2]]++
		case strings.HasPrefix(line, "# TYPE ") && len(fields) == 4:
			typed[fields[2]]++
		case line == "" || strings.HasPrefix(line, "#"):
		default:
			name, _, _ := strings.Cut(line, "{")
			name, _, _ = strings.Cut(name, " ")
			if declared[name] == 0 || typed[name] == 0 {
				t.Fatalf("sample for undeclared family %q: %s", name, line)
			}
		}
	}
	if len(declared) == 0 {
		t.Fatal("no metric families declared")
	}
	for name, n := range declared {
		if n != 1 || typed[name] != 1 {
			t.Errorf("%s declared %d HELP / %d TYPE lines, want 1 each", name, n, typed[name])
		}
	}
}

func TestWriteProm_EscapesLabelValues(t *testing.T) {
	// Vendors have shipped component identifiers with quotes and backslashes in
	// them. An unescaped one does not corrupt one sample, it corrupts the file.
	r := analyze.Report{
		GeneratedAt: generatedAt,
		Findings: []analyze.Finding{{
			Host: "node\n1", Component: `DIMM_"A\1`, Kind: "ecc.degrading",
			Severity: analyze.SeverityPlan, Confidence: 0.5,
		}},
	}

	text := render(t, r)

	want := `rack_sentinel_finding_severity{host="node\n1",component="DIMM_\"A\\1",kind="ecc.degrading"} 2`
	if !strings.Contains(text, want) {
		t.Errorf("label escaping wrong:\n want %s\n got  %s", want,
			strings.Join(samplesOf(text, "rack_sentinel_finding_severity"), "\n"))
	}
	// An embedded newline must not have split one sample across two lines.
	if got := samplesOf(text, "rack_sentinel_finding_severity"); len(got) != 1 {
		t.Errorf("got %d sample lines, want 1:\n%s", len(got), strings.Join(got, "\n"))
	}
	if strings.Contains(text, "node\n1") {
		t.Error("a raw newline from a label value reached the output")
	}
}

func TestWriteProm_IsOrderedAndRepeatable(t *testing.T) {
	// Same discipline as the text report: identical state produces an identical
	// file, so a diff of two polls shows only what changed.
	r := degradingReport()
	r.Trends = append(r.Trends, analyze.HostTrend{
		Host:  "node-003",
		Trend: analyze.ECCTrend{Component: "DIMM_A1", Total: 4, Samples: 96, Span: 24 * time.Hour, RatePerHour: 0.1},
	})

	first, second := render(t, r), render(t, r)
	if first != second {
		t.Error("two writes of one report differ")
	}

	got := samplesOf(first, "rack_sentinel_ecc_correctable_errors_total")
	if len(got) != 2 || !strings.Contains(got[0], "node-003") {
		t.Errorf("samples are not host-ordered:\n%s", strings.Join(got, "\n"))
	}
}

func TestWritePromFile_IsAtomicAndWorldReadable(t *testing.T) {
	// node_exporter reads this file on its own schedule, as its own user: a
	// half-written file is a parse error that discards every metric in it, and a
	// 0600 file is one it cannot read at all.
	dir := t.TempDir()
	path := filepath.Join(dir, "rack-sentinel.prom")

	if err := WritePromFile(path, degradingReport()); err != nil {
		t.Fatalf("WritePromFile: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != render(t, degradingReport()) {
		t.Error("file contents differ from the writer's output")
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o644 {
		t.Errorf("mode = %o, want 644 so the node_exporter user can read it", perm)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".tmp" {
			t.Errorf("temp file %s left behind", e.Name())
		}
	}

	// Rewriting in place must replace, not append.
	if err := WritePromFile(path, degradingReport()); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	again, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != len(data) {
		t.Errorf("rewrite produced %d bytes, want %d", len(again), len(data))
	}
}

func TestWritePromFile_ReportsAnUnwritablePath(t *testing.T) {
	err := WritePromFile(filepath.Join(t.TempDir(), "no-such-dir", "m.prom"), analyze.Report{})
	if err == nil {
		t.Fatal("writing into a missing directory should be reported, not ignored")
	}
}
