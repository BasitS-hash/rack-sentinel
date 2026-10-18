package collect

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// redfishMock serves the subset of the Redfish tree this collector walks.
//
// Talking to a real HTTP server rather than stubbing gofish is deliberate: it
// exercises the actual JSON decoding and pointer semantics, which is where the
// vendor-inconsistency bugs live. A stub would only prove the stub matches the
// test's assumptions.
type redfishMock struct {
	// correctable is emitted for DIMM_A1; nil omits the field entirely, which is
	// how several vendors behave.
	correctable *int
	// omitMetrics drops the MemoryMetrics link, another real vendor behaviour.
	omitMetrics bool
	// reading and threshold drive the thermal sensor.
	reading   *float64
	threshold *float64

	// lifeLeft is the drive's PredictedMediaLifeLeftPercent; nil omits the field
	// entirely, which is how most firmware behaves on SAS and SATA.
	lifeLeft *float64
	// failurePredicted is the drive's vendor-defined failure prediction. Redfish
	// types it as a plain boolean with no null, so there is no unreported state
	// to model here — which is itself the thing worth pinning.
	failurePredicted bool
	// omitStorage drops the Storage link from the system, which is every BMC
	// whose drives hang off a controller it does not expose over Redfish.
	omitStorage bool
	// breakStorage makes the Storage collection fail.
	breakStorage bool
	// breakDrive makes the drive resource itself fail while the subsystem listing
	// it succeeds.
	breakDrive bool
	// anonymousDrive omits both Name and Id, which leaves nothing to key the
	// series on.
	anonymousDrive bool

	requests atomic.Int64
	delay    time.Duration
}

func (m *redfishMock) handler() http.Handler {
	mux := http.NewServeMux()

	write := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}

	mux.HandleFunc("/redfish/v1/", func(w http.ResponseWriter, r *http.Request) {
		m.requests.Add(1)
		if m.delay > 0 {
			time.Sleep(m.delay)
		}
		write(w, map[string]any{
			"@odata.id":   "/redfish/v1/",
			"@odata.type": "#ServiceRoot.v1_5_0.ServiceRoot",
			"Id":          "RootService",
			"Name":        "Root Service",
			"Systems":     map[string]string{"@odata.id": "/redfish/v1/Systems"},
			"Chassis":     map[string]string{"@odata.id": "/redfish/v1/Chassis"},
		})
	})

	mux.HandleFunc("/redfish/v1/Systems", func(w http.ResponseWriter, r *http.Request) {
		write(w, map[string]any{
			"@odata.id": "/redfish/v1/Systems",
			"Name":      "Systems Collection",
			"Members": []map[string]string{
				{"@odata.id": "/redfish/v1/Systems/1"},
			},
			"Members@odata.count": 1,
		})
	})

	mux.HandleFunc("/redfish/v1/Systems/1", func(w http.ResponseWriter, r *http.Request) {
		body := map[string]any{
			"@odata.id": "/redfish/v1/Systems/1",
			"Id":        "1",
			"Name":      "System",
			"Memory":    map[string]string{"@odata.id": "/redfish/v1/Systems/1/Memory"},
		}
		if !m.omitStorage {
			body["Storage"] = map[string]string{"@odata.id": "/redfish/v1/Systems/1/Storage"}
		}
		write(w, body)
	})

	mux.HandleFunc("/redfish/v1/Systems/1/Storage", func(w http.ResponseWriter, r *http.Request) {
		if m.breakStorage {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		write(w, map[string]any{
			"@odata.id":           "/redfish/v1/Systems/1/Storage",
			"Name":                "Storage Collection",
			"Members":             []map[string]string{{"@odata.id": "/redfish/v1/Systems/1/Storage/NVMe"}},
			"Members@odata.count": 1,
		})
	})

	mux.HandleFunc("/redfish/v1/Systems/1/Storage/NVMe", func(w http.ResponseWriter, r *http.Request) {
		// Drives hang off Storage as a link array rather than a collection, which
		// is why the collector walks them differently from Memory.
		write(w, map[string]any{
			"@odata.id": "/redfish/v1/Systems/1/Storage/NVMe",
			"Id":        "NVMe",
			"Name":      "NVMe Subsystem",
			"Drives": []map[string]string{
				{"@odata.id": "/redfish/v1/Systems/1/Storage/NVMe/Drives/Bay1"},
			},
		})
	})

	mux.HandleFunc("/redfish/v1/Systems/1/Storage/NVMe/Drives/Bay1", func(w http.ResponseWriter, r *http.Request) {
		if m.breakDrive {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		body := map[string]any{
			"@odata.id":        "/redfish/v1/Systems/1/Storage/NVMe/Drives/Bay1",
			"Id":               "Bay1",
			"Name":             "Bay 1",
			"MediaType":        "SSD",
			"Protocol":         "NVMe",
			"CapacityBytes":    1920383410176,
			"FailurePredicted": m.failurePredicted,
			"Status":           map[string]any{"Health": "OK", "State": "Enabled"},
		}
		if m.lifeLeft != nil {
			body["PredictedMediaLifeLeftPercent"] = *m.lifeLeft
		}
		if m.anonymousDrive {
			delete(body, "Id")
			delete(body, "Name")
		}
		write(w, body)
	})

	mux.HandleFunc("/redfish/v1/Systems/1/Memory", func(w http.ResponseWriter, r *http.Request) {
		write(w, map[string]any{
			"@odata.id": "/redfish/v1/Systems/1/Memory",
			"Name":      "Memory Collection",
			"Members": []map[string]string{
				{"@odata.id": "/redfish/v1/Systems/1/Memory/DIMM_A1"},
			},
			"Members@odata.count": 1,
		})
	})

	mux.HandleFunc("/redfish/v1/Systems/1/Memory/DIMM_A1", func(w http.ResponseWriter, r *http.Request) {
		body := map[string]any{
			"@odata.id": "/redfish/v1/Systems/1/Memory/DIMM_A1",
			"Id":        "DIMM_A1",
			"Name":      "DIMM_A1",
		}
		if !m.omitMetrics {
			body["Metrics"] = map[string]string{
				"@odata.id": "/redfish/v1/Systems/1/Memory/DIMM_A1/MemoryMetrics",
			}
		}
		write(w, body)
	})

	mux.HandleFunc("/redfish/v1/Systems/1/Memory/DIMM_A1/MemoryMetrics", func(w http.ResponseWriter, r *http.Request) {
		lifetime := map[string]any{}
		if m.correctable != nil {
			lifetime["CorrectableECCErrorCount"] = *m.correctable
		}
		write(w, map[string]any{
			"@odata.id": "/redfish/v1/Systems/1/Memory/DIMM_A1/MemoryMetrics",
			"Id":        "MemoryMetrics",
			"Name":      "Memory Metrics",
			"LifeTime":  lifetime,
		})
	})

	mux.HandleFunc("/redfish/v1/Chassis", func(w http.ResponseWriter, r *http.Request) {
		write(w, map[string]any{
			"@odata.id":           "/redfish/v1/Chassis",
			"Name":                "Chassis Collection",
			"Members":             []map[string]string{{"@odata.id": "/redfish/v1/Chassis/1"}},
			"Members@odata.count": 1,
		})
	})

	mux.HandleFunc("/redfish/v1/Chassis/1", func(w http.ResponseWriter, r *http.Request) {
		write(w, map[string]any{
			"@odata.id": "/redfish/v1/Chassis/1",
			"Id":        "1",
			"Name":      "Chassis",
			"Thermal":   map[string]string{"@odata.id": "/redfish/v1/Chassis/1/Thermal"},
		})
	})

	mux.HandleFunc("/redfish/v1/Chassis/1/Thermal", func(w http.ResponseWriter, r *http.Request) {
		sensor := map[string]any{"MemberId": "0", "Name": "CPU1 Temp"}
		if m.reading != nil {
			sensor["ReadingCelsius"] = *m.reading
		}
		if m.threshold != nil {
			sensor["UpperThresholdCritical"] = *m.threshold
		}
		write(w, map[string]any{
			"@odata.id":    "/redfish/v1/Chassis/1/Thermal",
			"Id":           "Thermal",
			"Name":         "Thermal",
			"Temperatures": []map[string]any{sensor},
		})
	})

	return mux
}

func intp(v int) *int         { return &v }
func f64p(v float64) *float64 { return &v }

func serve(t *testing.T, m *redfishMock) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(m.handler())
	t.Cleanup(srv.Close)
	return srv
}

func TestHost_ReadsReportedCounters(t *testing.T) {
	m := &redfishMock{correctable: intp(1234), reading: f64p(70), threshold: f64p(95)}
	srv := serve(t, m)

	got := Host(context.Background(), Target{Host: "node-1", Endpoint: srv.URL})

	if got.Err != nil {
		t.Fatalf("unexpected error: %v", got.Err)
	}
	if v, ok := got.CorrectableErrors["DIMM_A1"]; !ok || v != 1234 {
		t.Errorf("CorrectableErrors[DIMM_A1] = %v (present=%v), want 1234", v, ok)
	}
	if v := got.Temperatures["CPU1 Temp"]; v != 70 {
		// MemberId is used when Name is absent; here the sensor has both, and
		// gofish maps MemberId. Accept whichever key carries the reading.
		if v2, ok := got.Temperatures["0"]; !ok || v2 != 70 {
			t.Errorf("temperature not recorded: %+v", got.Temperatures)
		}
	}
}

func TestHost_ThermalMarginIsThresholdMinusReading(t *testing.T) {
	// The number an operator acts on. 70°C means nothing without knowing the
	// sensor trips at 95.
	m := &redfishMock{correctable: intp(0), reading: f64p(70), threshold: f64p(95)}
	srv := serve(t, m)

	got := Host(context.Background(), Target{Host: "node-1", Endpoint: srv.URL})

	var margin float64
	var found bool
	for _, v := range got.ThermalMargins {
		margin, found = v, true
	}
	if !found {
		t.Fatalf("no thermal margin recorded: %+v", got)
	}
	if margin != 25 {
		t.Errorf("margin = %.1f, want 25 (95 - 70)", margin)
	}
}

func TestHost_SensorWithoutACriticalThresholdHasNoMargin(t *testing.T) {
	// A reading with nothing to compare it against is not a margin of zero, and
	// it is not a margin of the reading either. Vendors leave
	// UpperThresholdCritical unset on plenty of sensors — ambient inlets, board
	// probes — and the margin analysis has to not see them at all, because 0°C of
	// margin is a sensor on the edge of tripping.
	m := &redfishMock{correctable: intp(0), reading: f64p(50), threshold: nil}
	srv := serve(t, m)

	got := Host(context.Background(), Target{Host: "node-1", Endpoint: srv.URL})

	if len(got.ThermalMargins) != 0 {
		t.Errorf("a sensor with no critical threshold produced margins %+v", got.ThermalMargins)
	}
	// The reading itself was reported and is still worth having.
	if len(got.Temperatures) == 0 {
		t.Error("the temperature reading was dropped along with the missing threshold")
	}
}

func TestHost_UnreportedCounterIsAbsentNotZero(t *testing.T) {
	// The most important behaviour in this package. A BMC that omits the counter
	// must not produce a zero, because zero reads as "healthy DIMM" and would
	// make a blind BMC indistinguishable from good hardware.
	m := &redfishMock{correctable: nil, reading: f64p(50), threshold: f64p(95)}
	srv := serve(t, m)

	got := Host(context.Background(), Target{Host: "node-1", Endpoint: srv.URL})

	if v, ok := got.CorrectableErrors["DIMM_A1"]; ok {
		t.Errorf("absent counter recorded as %v; it must be omitted entirely", v)
	}
}

func TestHost_ZeroCounterIsRecordedAsZero(t *testing.T) {
	// The converse: a BMC that explicitly reports zero is genuinely healthy and
	// that fact should be captured.
	m := &redfishMock{correctable: intp(0), reading: f64p(50), threshold: f64p(95)}
	srv := serve(t, m)

	got := Host(context.Background(), Target{Host: "node-1", Endpoint: srv.URL})

	v, ok := got.CorrectableErrors["DIMM_A1"]
	if !ok {
		t.Fatal("explicit zero should be recorded")
	}
	if v != 0 {
		t.Errorf("value = %v, want 0", v)
	}
}

func TestHost_MissingMetricsLinkIsSurvivable(t *testing.T) {
	m := &redfishMock{omitMetrics: true, reading: f64p(50), threshold: f64p(95)}
	srv := serve(t, m)

	got := Host(context.Background(), Target{Host: "node-1", Endpoint: srv.URL})

	if got.Err != nil {
		t.Fatalf("a DIMM without a Metrics link must not fail the host: %v", got.Err)
	}
	if len(got.CorrectableErrors) != 0 {
		t.Errorf("expected no counters, got %+v", got.CorrectableErrors)
	}
	// Thermal collection must still have happened.
	if len(got.Temperatures) == 0 {
		t.Error("thermal data should still be collected when memory metrics are absent")
	}
}

func TestHost_UnreachableBMCReturnsErrorNotPanic(t *testing.T) {
	got := Host(context.Background(), Target{
		Host:     "node-dead",
		Endpoint: "http://127.0.0.1:1", // nothing listening
	})

	if got.Err == nil {
		t.Fatal("unreachable BMC must report an error")
	}
	if got.Host != "node-dead" {
		t.Errorf("host = %q, want node-dead — identity must survive failure", got.Host)
	}
}

func TestFleet_OneBadHostDoesNotHideTheRest(t *testing.T) {
	// The operational requirement: a dead BMC in a rack must not cost visibility
	// into every other node.
	m := &redfishMock{correctable: intp(42), reading: f64p(60), threshold: f64p(95)}
	srv := serve(t, m)

	targets := []Target{
		{Host: "good-1", Endpoint: srv.URL},
		{Host: "dead", Endpoint: "http://127.0.0.1:1"},
		{Host: "good-2", Endpoint: srv.URL},
	}

	results, err := Fleet(context.Background(), targets, Options{Concurrency: 3, PerHostTimeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("Fleet returned error: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("got %d results, want 3 — every target must yield one", len(results))
	}

	// Order must match input so callers can correlate.
	if results[0].Host != "good-1" || results[1].Host != "dead" || results[2].Host != "good-2" {
		t.Errorf("results out of order: %s, %s, %s", results[0].Host, results[1].Host, results[2].Host)
	}
	if results[1].Err == nil {
		t.Error("dead host should carry an error")
	}
	for _, i := range []int{0, 2} {
		if results[i].Err != nil {
			t.Errorf("%s failed: %v", results[i].Host, results[i].Err)
		}
		if results[i].CorrectableErrors["DIMM_A1"] != 42 {
			t.Errorf("%s lost its data: %+v", results[i].Host, results[i].CorrectableErrors)
		}
	}
}

func TestFleet_RespectsConcurrencyLimit(t *testing.T) {
	// BMCs have shallow connection queues. Exceeding the cap produces timeouts
	// that look exactly like hardware faults, so the bound has to actually hold.
	var inFlight, peak atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cur := inFlight.Add(1)
		for {
			old := peak.Load()
			if cur <= old || peak.CompareAndSwap(old, cur) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
		inFlight.Add(-1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	targets := make([]Target, 24)
	for i := range targets {
		targets[i] = Target{Host: fmt.Sprintf("node-%d", i), Endpoint: srv.URL}
	}

	const limit = 4
	if _, err := Fleet(context.Background(), targets, Options{
		Concurrency: limit, PerHostTimeout: 5 * time.Second,
	}); err != nil {
		t.Fatalf("Fleet error: %v", err)
	}

	if got := peak.Load(); got > limit {
		t.Errorf("peak concurrent requests = %d, exceeds limit %d", got, limit)
	}
}

func TestFleet_NoTargets(t *testing.T) {
	if _, err := Fleet(context.Background(), nil, DefaultOptions()); err != ErrNoTargets {
		t.Errorf("err = %v, want ErrNoTargets", err)
	}
}

func TestFleet_CancelledContextStopsWork(t *testing.T) {
	m := &redfishMock{correctable: intp(1), delay: 500 * time.Millisecond}
	srv := serve(t, m)

	targets := make([]Target, 40)
	for i := range targets {
		targets[i] = Target{Host: fmt.Sprintf("n%d", i), Endpoint: srv.URL}
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled

	start := time.Now()
	results, err := Fleet(ctx, targets, Options{Concurrency: 2, PerHostTimeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("Fleet error: %v", err)
	}
	if len(results) != len(targets) {
		t.Fatalf("got %d results, want %d", len(results), len(targets))
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("cancellation took %s — context is not being honoured", elapsed)
	}
}

func TestSumReported(t *testing.T) {
	tests := map[string]struct {
		values  []*int
		want    float64
		wantAny bool
	}{
		"both present":  {[]*int{intp(10), intp(5)}, 15, true},
		"first only":    {[]*int{intp(7), nil}, 7, true},
		"second only":   {[]*int{nil, intp(3)}, 3, true},
		"neither":       {[]*int{nil, nil}, 0, false},
		"explicit zero": {[]*int{intp(0), nil}, 0, true},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got, any := sumReported(tt.values...)
			if got != tt.want || any != tt.wantAny {
				t.Errorf("sumReported = (%v, %v), want (%v, %v)", got, any, tt.want, tt.wantAny)
			}
		})
	}
}

func TestHost_ReadsReportedDriveLife(t *testing.T) {
	m := &redfishMock{correctable: intp(0), reading: f64p(50), threshold: f64p(95), lifeLeft: f64p(62)}
	srv := serve(t, m)

	got := Host(context.Background(), Target{Host: "node-1", Endpoint: srv.URL})

	if got.Err != nil {
		t.Fatalf("unexpected error: %v", got.Err)
	}
	if v, ok := got.MediaLifeLeft["Bay 1"]; !ok || v != 62 {
		t.Errorf("MediaLifeLeft[Bay 1] = %v (present=%v), want 62", v, ok)
	}
}

func TestHost_DriveWithoutALifeEstimateIsAbsentNotZero(t *testing.T) {
	// The invariant at its sharpest. Most firmware populates
	// PredictedMediaLifeLeftPercent on NVMe and omits it on SAS and SATA, so a
	// zero here would not merely look like a healthy drive the way a zeroed error
	// counter does — it would read as the opposite, a drive with no media life
	// left at all, and condemn a chassis of working disks.
	m := &redfishMock{correctable: intp(0), reading: f64p(50), threshold: f64p(95), lifeLeft: nil}
	srv := serve(t, m)

	got := Host(context.Background(), Target{Host: "node-1", Endpoint: srv.URL})

	if v, ok := got.MediaLifeLeft["Bay 1"]; ok {
		t.Errorf("absent life estimate recorded as %v; it must be omitted entirely", v)
	}
}

func TestHost_ZeroLifeLeftIsRecordedAsSpent(t *testing.T) {
	// The converse: a drive the firmware says is out of media life is reporting a
	// real measurement and that fact has to survive collection.
	m := &redfishMock{correctable: intp(0), reading: f64p(50), threshold: f64p(95), lifeLeft: f64p(0)}
	srv := serve(t, m)

	got := Host(context.Background(), Target{Host: "node-1", Endpoint: srv.URL})

	v, ok := got.MediaLifeLeft["Bay 1"]
	if !ok {
		t.Fatal("an explicit zero should be recorded")
	}
	if v != 0 {
		t.Errorf("value = %v, want 0", v)
	}
}

func TestHost_VendorFailurePredictionIsRecordedOnlyWhenSet(t *testing.T) {
	// The vendor's own opinion, collected so the analysis can be shown next to
	// it. Only the true case gets an entry, and that is not tidiness: Redfish
	// types FailurePredicted as a plain boolean, so a firmware that does not
	// implement it decodes identically to one reporting a clear drive. Recording
	// false would be asserting something the BMC never said.
	flagged := &redfishMock{correctable: intp(0), reading: f64p(50), threshold: f64p(95), lifeLeft: f64p(80), failurePredicted: true}
	clear := &redfishMock{correctable: intp(0), reading: f64p(50), threshold: f64p(95), lifeLeft: f64p(80)}

	got := Host(context.Background(), Target{Host: "node-1", Endpoint: serve(t, flagged).URL})
	if !got.FailurePredicted["Bay 1"] {
		t.Error("a flagged drive was not recorded")
	}

	got = Host(context.Background(), Target{Host: "node-1", Endpoint: serve(t, clear).URL})
	if _, ok := got.FailurePredicted["Bay 1"]; ok {
		t.Error("an unflagged drive got an entry; absence is the only honest encoding of a boolean with no null")
	}
	// The life estimate is still collected either way.
	if _, ok := got.MediaLifeLeft["Bay 1"]; !ok {
		t.Error("the life estimate was dropped along with the clear flag")
	}
}

func TestHost_MissingStorageIsSurvivable(t *testing.T) {
	// Plenty of BMCs expose no Storage subsystem at all. That is not a host
	// failure and must not cost the memory and thermal data already collected.
	m := &redfishMock{omitStorage: true, correctable: intp(7), reading: f64p(50), threshold: f64p(95)}
	srv := serve(t, m)

	got := Host(context.Background(), Target{Host: "node-1", Endpoint: srv.URL})

	if got.Err != nil {
		t.Fatalf("a system without a Storage link must not fail the host: %v", got.Err)
	}
	if len(got.MediaLifeLeft) != 0 {
		t.Errorf("expected no drive estimates, got %+v", got.MediaLifeLeft)
	}
	if got.CorrectableErrors["DIMM_A1"] != 7 || len(got.Temperatures) == 0 {
		t.Error("memory and thermal data should still be collected when storage is absent")
	}
}

func TestHost_UnreadableStorageIsSurvivable(t *testing.T) {
	// A storage subsystem that answers with an error, and a drive resource that
	// does, are both routine on firmware mid-reboot. Neither is a host failure
	// and neither may cost the memory and thermal data already collected.
	for name, m := range map[string]*redfishMock{
		"collection fails": {breakStorage: true, correctable: intp(3), reading: f64p(50), threshold: f64p(95)},
		"drive fails":      {breakDrive: true, correctable: intp(3), reading: f64p(50), threshold: f64p(95)},
	} {
		t.Run(name, func(t *testing.T) {
			got := Host(context.Background(), Target{Host: "node-1", Endpoint: serve(t, m).URL})

			if got.Err != nil {
				t.Fatalf("unreadable storage failed the host: %v", got.Err)
			}
			if len(got.MediaLifeLeft) != 0 {
				t.Errorf("expected no drive estimates, got %+v", got.MediaLifeLeft)
			}
			if got.CorrectableErrors["DIMM_A1"] != 3 || len(got.Temperatures) == 0 {
				t.Error("memory and thermal data were lost with the storage error")
			}
		})
	}
}

func TestHost_DriveWithNoIdentifierIsSkipped(t *testing.T) {
	// A drive with neither Name nor Id has nothing to key a series on, and a
	// series keyed on an empty string would merge every such drive in the chassis
	// into one. Dropping it loses a drive; keeping it invents a trend across
	// several, so it is dropped.
	m := &redfishMock{anonymousDrive: true, lifeLeft: f64p(40), failurePredicted: true,
		correctable: intp(0), reading: f64p(50), threshold: f64p(95)}
	srv := serve(t, m)

	got := Host(context.Background(), Target{Host: "node-1", Endpoint: srv.URL})

	if len(got.MediaLifeLeft) != 0 || len(got.FailurePredicted) != 0 {
		t.Errorf("an unidentifiable drive was recorded: %+v / %+v", got.MediaLifeLeft, got.FailurePredicted)
	}
}
