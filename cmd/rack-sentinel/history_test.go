package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/BasitS-hash/rack-sentinel/internal/analyze"
	"github.com/BasitS-hash/rack-sentinel/internal/collect"
)

// poll builds one telemetry result for a single DIMM at a given time.
func poll(host string, at time.Time, correctable float64) collect.HostTelemetry {
	return collect.HostTelemetry{
		Host:              host,
		CollectedAt:       at,
		CorrectableErrors: map[string]float64{"DIMM_A1": correctable},
	}
}

func TestHistory_DetectsDegradationAcrossPolls(t *testing.T) {
	// The end-to-end claim of the tool: no single poll can tell you a DIMM is
	// failing. Only the accumulated series can. This walks the same path the
	// binary takes — append, persist, reload, analyse.
	start := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "state.json")

	// Flat for eight polls, then climbing hard.
	counts := []float64{0, 0, 0, 0, 0, 0, 0, 0, 10, 30, 70, 150, 310, 630}

	var report analyze.Report
	for i, c := range counts {
		h, err := loadHistory(path)
		if err != nil {
			t.Fatalf("poll %d: load: %v", i, err)
		}
		tele := []collect.HostTelemetry{poll("node-1", start.Add(time.Duration(i)*time.Hour), c)}
		h.append(tele)
		report = h.analyze(tele)
		if err := h.save(path); err != nil {
			t.Fatalf("poll %d: save: %v", i, err)
		}
	}

	var found *analyze.Finding
	for i := range report.Findings {
		if report.Findings[i].Kind == "ecc.degrading" {
			found = &report.Findings[i]
		}
	}
	if found == nil {
		t.Fatalf("no degradation finding after %d polls: %+v", len(counts), report.Findings)
	}
	if found.Severity != analyze.SeverityUrgent {
		t.Errorf("severity = %s, want URGENT", found.Severity)
	}
	if found.Host != "node-1" {
		t.Errorf("host = %q, want node-1", found.Host)
	}
}

func TestHistory_SinglePollCannotTrend(t *testing.T) {
	// Stated honestly in the CLI help, so it needs to be true: one poll has no
	// trend and must not claim one.
	h := newHistory()
	tele := []collect.HostTelemetry{poll("node-1", time.Now(), 5000)}
	h.append(tele)

	report := h.analyze(tele)

	for _, f := range report.Findings {
		if f.Severity >= analyze.SeverityPlan {
			t.Errorf("single poll produced %s finding: %s", f.Severity, f.Summary)
		}
	}
}

func TestHistory_FailedPollAddsNoSample(t *testing.T) {
	// Recording a zero for an unreachable host would look like a counter reset
	// and would corrupt the trend for a DIMM that is genuinely degrading.
	h := newHistory()
	start := time.Now()

	h.append([]collect.HostTelemetry{poll("node-1", start, 100)})
	h.append([]collect.HostTelemetry{{Host: "node-1", CollectedAt: start.Add(time.Hour), Err: os.ErrDeadlineExceeded}})
	h.append([]collect.HostTelemetry{poll("node-1", start.Add(2*time.Hour), 120)})

	samples := h.Series["node-1"]["ecc.correctable/DIMM_A1"]
	if len(samples) != 2 {
		t.Fatalf("got %d samples, want 2 — the failed poll must not contribute", len(samples))
	}
	for _, s := range samples {
		if s.Value == 0 {
			t.Error("a failed poll was recorded as a zero sample")
		}
	}
	if h.failed != 1 {
		t.Errorf("failed = %d, want 1", h.failed)
	}
}

func TestHistory_UnreachableBMCIsAFinding(t *testing.T) {
	// A node nobody can see is not a node that is fine.
	h := newHistory()
	tele := []collect.HostTelemetry{{Host: "node-dark", Err: os.ErrDeadlineExceeded}}
	h.append(tele)

	report := h.analyze(tele)

	var seen bool
	for _, f := range report.Findings {
		if f.Kind == "bmc.unreachable" && f.Host == "node-dark" {
			seen = true
		}
	}
	if !seen {
		t.Fatalf("unreachable BMC produced no finding: %+v", report.Findings)
	}
}

func TestHistory_RoundTripsThroughDisk(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	start := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)

	h := newHistory()
	h.append([]collect.HostTelemetry{poll("node-1", start, 42)})
	if err := h.save(path); err != nil {
		t.Fatalf("save: %v", err)
	}

	reloaded, err := loadHistory(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	samples := reloaded.Series["node-1"]["ecc.correctable/DIMM_A1"]
	if len(samples) != 1 || samples[0].Value != 42 {
		t.Fatalf("round-trip lost data: %+v", samples)
	}
	if !samples[0].At.Equal(start) {
		t.Errorf("timestamp = %v, want %v", samples[0].At, start)
	}
}

func TestLoadHistory_MissingFileIsNotAnError(t *testing.T) {
	// The first run legitimately has no state.
	h, err := loadHistory(filepath.Join(t.TempDir(), "absent.json"))
	if err != nil {
		t.Fatalf("missing history should not error: %v", err)
	}
	if h == nil || h.Series == nil {
		t.Fatal("expected an initialised empty history")
	}
}

func TestLoadHistory_CorruptFileIsReported(t *testing.T) {
	// Silently discarding corrupt state would throw away every accumulated trend
	// without telling anyone.
	path := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadHistory(path); err == nil {
		t.Fatal("corrupt history should be reported, not ignored")
	}
}

func TestHistory_SaveIsAtomic(t *testing.T) {
	// A partial write would destroy the fleet's accumulated trends.
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	h := newHistory()
	h.append([]collect.HostTelemetry{poll("node-1", time.Now(), 1)})
	if err := h.save(path); err != nil {
		t.Fatalf("save: %v", err)
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
}

func TestHistory_CapsRetainedSamples(t *testing.T) {
	// Unbounded growth would make the state file unusable on a real fleet.
	h := newHistory()
	start := time.Now()
	for i := 0; i < maxSamplesPerSeries+250; i++ {
		h.append([]collect.HostTelemetry{
			poll("node-1", start.Add(time.Duration(i)*time.Minute), float64(i)),
		})
	}

	got := len(h.Series["node-1"]["ecc.correctable/DIMM_A1"])
	if got != maxSamplesPerSeries {
		t.Errorf("retained %d samples, want cap of %d", got, maxSamplesPerSeries)
	}
	// The cap must drop the OLDEST, not the newest.
	samples := h.Series["node-1"]["ecc.correctable/DIMM_A1"]
	if samples[len(samples)-1].Value != float64(maxSamplesPerSeries+249) {
		t.Errorf("newest sample = %v, want the most recent value", samples[len(samples)-1].Value)
	}
}

func TestParseSeverity(t *testing.T) {
	for in, want := range map[string]analyze.Severity{
		"info": analyze.SeverityInfo, "watch": analyze.SeverityWatch,
		"PLAN": analyze.SeverityPlan, "Urgent": analyze.SeverityUrgent,
	} {
		got, err := parseSeverity(in)
		if err != nil || got != want {
			t.Errorf("parseSeverity(%q) = (%v, %v), want (%v, nil)", in, got, err, want)
		}
	}
	if _, err := parseSeverity("catastrophic"); err == nil {
		t.Error("unknown severity should error")
	}
}

func TestLoadTargets(t *testing.T) {
	path := filepath.Join(t.TempDir(), "targets.csv")
	content := `# fleet
node-01,https://10.0.0.11,admin,secret
node-02,https://10.0.0.12,admin,hunter2
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	targets, err := loadTargets(path, true)
	if err != nil {
		t.Fatalf("loadTargets: %v", err)
	}
	if len(targets) != 2 {
		t.Fatalf("got %d targets, want 2 (comment line must be skipped)", len(targets))
	}
	if targets[0].Host != "node-01" || targets[0].Endpoint != "https://10.0.0.11" {
		t.Errorf("unexpected first target: %+v", targets[0])
	}
	if !targets[0].Insecure {
		t.Error("insecure flag should propagate to targets")
	}
}

func TestLoadTargets_ShortRowIsRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.csv")
	if err := os.WriteFile(path, []byte("node-01,https://10.0.0.11\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadTargets(path, false); err == nil {
		t.Fatal("a row missing credentials should be rejected, not silently skipped")
	}
}

func TestHistory_PruneDropsStaleSeriesAndEmptyHosts(t *testing.T) {
	// The growth nobody notices is not sample count, it is series count: every
	// host and every DIMM identifier ever polled keeps its series forever, long
	// after the hardware left the rack.
	h := newHistory()
	old := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 6; i++ {
		h.append([]collect.HostTelemetry{poll("node-gone", old.Add(time.Duration(i)*time.Hour), float64(100+10*i))})
	}
	now := old.Add(90 * 24 * time.Hour)
	for i := 0; i < 6; i++ {
		h.append([]collect.HostTelemetry{poll("node-live", now.Add(time.Duration(i)*time.Hour), float64(10*i))})
	}

	h.prune(now.Add(6*time.Hour), defaultRetention)

	if _, ok := h.Series["node-gone"]; ok {
		t.Error("a host nobody has polled inside the window kept its series")
	}
	if n := len(h.Series["node-live"]["ecc.correctable/DIMM_A1"]); n != 6 {
		t.Errorf("live host kept %d samples, want all 6", n)
	}
}

func TestHistory_PruneTrimsToTheWindow(t *testing.T) {
	// 5-minute polls: without retention the file grows with uptime, and every run
	// pays to read and rewrite all of it.
	h := newHistory()
	start := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	const polls = 1000 // a little over three days
	for i := 0; i < polls; i++ {
		h.append([]collect.HostTelemetry{
			poll("node-1", start.Add(time.Duration(i)*5*time.Minute), float64(i)),
		})
	}
	last := start.Add(time.Duration(polls-1) * 5 * time.Minute)

	h.prune(last, 24*time.Hour)

	samples := h.Series["node-1"]["ecc.correctable/DIMM_A1"]
	if len(samples) > 24*12+1 {
		t.Errorf("kept %d samples, want no more than a day of 5-minute polls", len(samples))
	}
	if samples[len(samples)-1].Value != float64(polls-1) {
		t.Error("pruning dropped the newest sample")
	}
}

func TestHistory_PruneKeepsTheDegradationVerdict(t *testing.T) {
	// Retention runs between the append and the analysis on every poll, so a
	// window shorter than the accumulated history must not change the verdict a
	// run would otherwise have reported.
	h := newHistory()
	start := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	counts := []float64{0, 0, 0, 0, 0, 0, 0, 0, 10, 30, 70, 150, 310, 630}

	var report analyze.Report
	for i, c := range counts {
		at := start.Add(time.Duration(i) * time.Hour)
		tele := []collect.HostTelemetry{poll("node-1", at, c)}
		h.append(tele)
		h.prune(at, 6*time.Hour)
		report = h.analyze(tele)
	}

	var found *analyze.Finding
	for i := range report.Findings {
		if report.Findings[i].Kind == "ecc.degrading" {
			found = &report.Findings[i]
		}
	}
	if found == nil {
		t.Fatalf("retention lost the degradation finding: %+v", report.Findings)
	}
	if found.Severity != analyze.SeverityUrgent {
		t.Errorf("severity = %s, want URGENT", found.Severity)
	}
}

func TestHistory_PruneWithoutRetentionKeepsEverything(t *testing.T) {
	// -retain 0 is an explicit opt-out, not a default to fall into.
	h := newHistory()
	start := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 10; i++ {
		h.append([]collect.HostTelemetry{poll("node-1", start.Add(time.Duration(i)*time.Hour), float64(i))})
	}

	h.prune(time.Now(), 0)

	if n := len(h.Series["node-1"]["ecc.correctable/DIMM_A1"]); n != 10 {
		t.Errorf("kept %d samples, want all 10", n)
	}
}

func BenchmarkHistoryAnalyze(b *testing.B) {
	// A realistic fleet shape: 500 hosts x 16 DIMMs, each with a week of
	// 15-minute samples.
	h := newHistory()
	start := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	for host := 0; host < 500; host++ {
		hostName := "node-" + string(rune('a'+host%26)) + string(rune('0'+host/26%10))
		byComp := map[string][]analyze.Sample{}
		for dimm := 0; dimm < 16; dimm++ {
			key := "ecc.correctable/DIMM_" + string(rune('A'+dimm))
			samples := make([]analyze.Sample, 0, 96)
			for i := 0; i < 96; i++ {
				samples = append(samples, analyze.Sample{
					At:    start.Add(time.Duration(i) * 15 * time.Minute),
					Value: float64(i),
				})
			}
			byComp[key] = samples
		}
		h.Series[hostName] = byComp
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		h.analyze(nil)
	}
}

// thermalPoll builds one telemetry result carrying a single sensor's margin.
func thermalPoll(host string, at time.Time, margin float64) collect.HostTelemetry {
	return collect.HostTelemetry{
		Host:           host,
		CollectedAt:    at,
		ThermalMargins: map[string]float64{"CPU0_Die": margin},
	}
}

func TestHistory_DetectsShrinkingMarginAcrossPolls(t *testing.T) {
	// The same end-to-end claim as the ECC walk above, on the second signal: no
	// single poll can tell you a node is losing cooling, because every poll sees
	// a margin that is merely smaller than the threshold, which is normal.
	start := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "state.json")

	var report analyze.Report
	for i := 0; i < 48; i++ {
		h, err := loadHistory(path)
		if err != nil {
			t.Fatalf("poll %d: load: %v", i, err)
		}
		tele := []collect.HostTelemetry{
			thermalPoll("node-1", start.Add(time.Duration(i)*time.Hour), 40-0.5*float64(i)),
		}
		h.append(tele)
		report = h.analyze(tele)
		if err := h.save(path); err != nil {
			t.Fatalf("poll %d: save: %v", i, err)
		}
	}

	var found *analyze.Finding
	for i := range report.Findings {
		if report.Findings[i].Kind == "thermal.margin-shrinking" {
			found = &report.Findings[i]
		}
	}
	if found == nil {
		t.Fatalf("no thermal finding after 48 polls: %+v", report.Findings)
	}
	if found.Severity != analyze.SeverityUrgent {
		t.Errorf("severity = %s, want URGENT", found.Severity)
	}
	if found.Component != "CPU0_Die" {
		t.Errorf("component = %q, want CPU0_Die", found.Component)
	}

	// The trend has to reach the exporter too, not only the prose.
	if len(report.ThermalTrends) != 1 {
		t.Fatalf("got %d thermal trends, want 1", len(report.ThermalTrends))
	}
	if report.ThermalTrends[0].Trend.ShrinkPerHour <= 0 {
		t.Errorf("exported shrink rate = %.3f, want positive", report.ThermalTrends[0].Trend.ShrinkPerHour)
	}
}

func TestHistory_SensorTheBMCStoppedReportingAddsNoSample(t *testing.T) {
	// The absent-not-zero invariant where the series is built. A sensor that drops
	// out of the BMC's response must not acquire a 0°C reading, which would read
	// as a sensor sitting exactly on its critical threshold.
	h := newHistory()
	start := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)

	h.append([]collect.HostTelemetry{thermalPoll("node-1", start, 30)})
	h.append([]collect.HostTelemetry{{
		Host: "node-1", CollectedAt: start.Add(time.Hour),
		ThermalMargins: map[string]float64{}, // BMC answered, sensor gone
	}})
	h.append([]collect.HostTelemetry{thermalPoll("node-1", start.Add(2*time.Hour), 28)})

	samples := h.Series["node-1"]["thermal.margin/CPU0_Die"]
	if len(samples) != 2 {
		t.Fatalf("got %d readings, want 2 — the poll that omitted the sensor must not contribute", len(samples))
	}
	for _, s := range samples {
		if s.Value == 0 {
			t.Error("an unreported sensor was recorded as 0°C of margin")
		}
	}
}

func TestHistory_ThermalSeriesGetGaugeRetention(t *testing.T) {
	// The two series shapes land in the same file and must not be pruned by the
	// same rule. A margin held to the ECC floor is trimmed below what the thermal
	// analyser will speak about, and the sensor goes quiet.
	h := newHistory()
	start := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 96; i++ {
		at := start.Add(time.Duration(i) * time.Hour)
		h.append([]collect.HostTelemetry{{
			Host:              "node-1",
			CollectedAt:       at,
			CorrectableErrors: map[string]float64{"DIMM_A1": float64(10 * i)},
			ThermalMargins:    map[string]float64{"CPU0_Die": 40 - 0.3*float64(i)},
		}})
	}
	last := start.Add(95 * time.Hour)

	// A window far shorter than what a thermal trend needs.
	h.prune(last, 2*time.Hour)

	margins := h.Series["node-1"]["thermal.margin/CPU0_Die"]
	trend, f := analyze.AnalyzeThermal("node-1", analyze.Series{Component: "CPU0_Die", Samples: margins})
	if !trend.Trendable() {
		t.Errorf("retention left %d readings over %s — the sensor can no longer be trended",
			trend.Samples, trend.Span)
	}
	if f == nil {
		t.Error("retention silenced a sensor that is losing headroom")
	}

	// And the counter beside it still keeps its own floor, not the thermal one.
	ecc := h.Series["node-1"]["ecc.correctable/DIMM_A1"]
	if len(ecc) == 0 {
		t.Fatal("the ECC series was deleted outright")
	}
	if len(ecc) >= len(margins) {
		t.Errorf("the counter kept %d samples and the gauge %d; the counter's floor is the lower of the two",
			len(ecc), len(margins))
	}
}

// drivePoll builds one telemetry result for a single drive bay at a given time.
func drivePoll(host string, at time.Time, lifeLeft float64) collect.HostTelemetry {
	return collect.HostTelemetry{
		Host:          host,
		CollectedAt:   at,
		MediaLifeLeft: map[string]float64{"Bay 1": lifeLeft},
	}
}

func TestHistory_DetectsWearAcrossPolls(t *testing.T) {
	// The same end-to-end claim as the other two walks, on the third signal. No
	// single poll can tell you a drive is being written to death: every poll sees
	// an estimate below 100, which is every drive in service. Only the series
	// says how fast it is falling.
	start := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "state.json")

	var report analyze.Report
	for i := 0; i < 336; i++ {
		h, err := loadHistory(path)
		if err != nil {
			t.Fatalf("poll %d: load: %v", i, err)
		}
		// Whole percent, the way firmware reports it: a staircase, not a ramp.
		life := float64(int(95 - float64(i)/24))
		tele := []collect.HostTelemetry{
			drivePoll("node-1", start.Add(time.Duration(i)*time.Hour), life),
		}
		h.append(tele)
		report = h.analyze(tele)
		if err := h.save(path); err != nil {
			t.Fatalf("poll %d: save: %v", i, err)
		}
	}

	var found *analyze.Finding
	for i := range report.Findings {
		if report.Findings[i].Kind == "drive.wearing" {
			found = &report.Findings[i]
		}
	}
	if found == nil {
		t.Fatalf("no drive finding after 336 polls: %+v", report.Findings)
	}
	if found.Component != "Bay 1" {
		t.Errorf("component = %q, want Bay 1", found.Component)
	}
	if found.Severity < analyze.SeverityPlan {
		t.Errorf("severity = %s, want at least PLAN", found.Severity)
	}

	// The trend has to reach the exporter too, not only the prose.
	if len(report.DriveTrends) != 1 {
		t.Fatalf("got %d drive trends, want 1", len(report.DriveTrends))
	}
	if report.DriveTrends[0].Trend.WearPerDay <= 0 {
		t.Errorf("exported wear rate = %.4f, want positive", report.DriveTrends[0].Trend.WearPerDay)
	}
}

func TestHistory_DriveTheBMCStoppedReportingAddsNoSample(t *testing.T) {
	// The absent-not-zero invariant where the series is built, on the signal
	// where coercion is most destructive: a 0 here is a drive with no media life
	// left, so a BMC that drops the field for one poll would otherwise record a
	// spent disk.
	h := newHistory()
	start := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)

	h.append([]collect.HostTelemetry{drivePoll("node-1", start, 80)})
	h.append([]collect.HostTelemetry{{
		Host: "node-1", CollectedAt: start.Add(time.Hour),
		MediaLifeLeft: map[string]float64{}, // BMC answered, drive reported no estimate
	}})
	h.append([]collect.HostTelemetry{drivePoll("node-1", start.Add(2*time.Hour), 79)})

	samples := h.Series["node-1"]["drive.life/Bay 1"]
	if len(samples) != 2 {
		t.Fatalf("got %d readings, want 2 — the poll that omitted the estimate must not contribute", len(samples))
	}
	for _, s := range samples {
		if s.Value == 0 {
			t.Error("a drive with no reported estimate was recorded as 0% media life left")
		}
	}
}

func TestHistory_DriveSeriesGetWearRetention(t *testing.T) {
	// Three series shapes now land in the same file and all three are pruned by
	// different rules. A drive held to the thermal floor keeps a day of readings,
	// which contains at most one step, and the bay goes quiet.
	h := newHistory()
	start := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 336; i++ {
		at := start.Add(time.Duration(i) * time.Hour)
		h.append([]collect.HostTelemetry{{
			Host:              "node-1",
			CollectedAt:       at,
			CorrectableErrors: map[string]float64{"DIMM_A1": float64(10 * i)},
			ThermalMargins:    map[string]float64{"CPU0_Die": 40 - 0.1*float64(i)},
			MediaLifeLeft:     map[string]float64{"Bay 1": float64(int(95 - float64(i)/24))},
		}})
	}
	last := start.Add(335 * time.Hour)

	// A window far shorter than what any of the three trends needs.
	h.prune(last, 2*time.Hour)

	drive := h.Series["node-1"]["drive.life/Bay 1"]
	trend, f := analyze.AnalyzeDriveWear("node-1", analyze.Series{Component: "Bay 1", Samples: drive}, false)
	if !trend.Trendable() {
		t.Errorf("retention left %d readings over %s with %d steps — the bay can no longer be trended",
			trend.Samples, trend.Span, trend.Steps)
	}
	if f == nil {
		t.Error("retention silenced a drive that is burning media life")
	}

	// And the two series beside it keep their own floors, not the drive's.
	if len(h.Series["node-1"]["ecc.correctable/DIMM_A1"]) == 0 {
		t.Error("the ECC series was deleted outright")
	}
	if len(h.Series["node-1"]["thermal.margin/CPU0_Die"]) == 0 {
		t.Error("the thermal series was deleted outright")
	}
}

func TestHistory_VendorFlaggedDriveWithNoEstimateIsStillReported(t *testing.T) {
	// The common SAS case, and the one place the vendor's boolean has to be able
	// to produce a finding on its own: the firmware says the drive is failing and
	// never populated a life estimate, so there is no series to walk and nothing
	// of ours to disagree with. A drive nobody can trend is not a drive that is
	// fine.
	h := newHistory()
	start := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	tele := []collect.HostTelemetry{{
		Host:              "node-1",
		CollectedAt:       start,
		CorrectableErrors: map[string]float64{"DIMM_A1": 0},
		FailurePredicted:  map[string]bool{"Bay 7": true},
	}}
	h.append(tele)

	report := h.analyze(tele)

	var found *analyze.Finding
	for i := range report.Findings {
		if report.Findings[i].Kind == "drive.vendor-failure-predicted" {
			found = &report.Findings[i]
		}
	}
	if found == nil {
		t.Fatalf("a flagged drive with no life estimate produced nothing: %+v", report.Findings)
	}
	if found.Component != "Bay 7" {
		t.Errorf("component = %q, want Bay 7", found.Component)
	}

	// And it reaches the exporter, so the flag can be alerted on directly.
	if len(report.DriveTrends) != 1 {
		t.Fatalf("got %d drive trends, want 1", len(report.DriveTrends))
	}
	if !report.DriveTrends[0].Trend.VendorFailurePredicted {
		t.Error("the flag did not reach the exported trend")
	}
	if report.DriveTrends[0].Trend.Samples != 0 {
		t.Errorf("samples = %d, want 0 — there was no estimate to trend", report.DriveTrends[0].Trend.Samples)
	}
}

func TestHistory_VendorFlagDoesNotDuplicateATrendedDrive(t *testing.T) {
	// One drive, one row, even though the flag arrives by a different route from
	// the series. A bay that is both trended and flagged must produce exactly one
	// finding, with the agreement inside it.
	h := newHistory()
	start := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 336; i++ {
		h.append([]collect.HostTelemetry{{
			Host:             "node-1",
			CollectedAt:      start.Add(time.Duration(i) * time.Hour),
			MediaLifeLeft:    map[string]float64{"Bay 1": float64(int(95 - float64(i)/24))},
			FailurePredicted: map[string]bool{"Bay 1": true},
		}})
	}
	tele := []collect.HostTelemetry{{
		Host:             "node-1",
		CollectedAt:      start.Add(335 * time.Hour),
		MediaLifeLeft:    map[string]float64{"Bay 1": 81},
		FailurePredicted: map[string]bool{"Bay 1": true},
	}}

	report := h.analyze(tele)

	var drive []analyze.Finding
	for _, f := range report.Findings {
		if f.Component == "Bay 1" {
			drive = append(drive, f)
		}
	}
	if len(drive) != 1 {
		t.Fatalf("got %d findings for one bay, want 1: %+v", len(drive), drive)
	}
	if drive[0].Kind != "drive.wearing" {
		t.Errorf("kind = %q, want drive.wearing", drive[0].Kind)
	}
	if len(report.DriveTrends) != 1 {
		t.Errorf("got %d drive trends for one bay, want 1", len(report.DriveTrends))
	}
}
