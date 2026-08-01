// Package collect pulls hardware telemetry from BMCs over Redfish.
//
// Transport, auth and schema decoding are delegated to gofish
// (github.com/stmcginnis/gofish), which is the established Go Redfish client.
// What lives here is the part that library deliberately does not do: fleet-scale
// concurrency with per-host isolation, and normalisation of the vendor-specific
// ways memory error counters are exposed.
package collect

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/stmcginnis/gofish"
	"github.com/stmcginnis/gofish/schemas"
)

// ErrNoTargets is returned when a fleet poll is asked for with no targets.
var ErrNoTargets = errors.New("no targets configured")

// Target is one BMC to poll.
type Target struct {
	Host     string
	Endpoint string
	Username string
	Password string
	Insecure bool
}

// HostTelemetry is everything read from one BMC in one pass.
//
// Every map holds only values the BMC actually reported. A DIMM absent from
// CorrectableErrors was not read as zero — it was not read at all. Conflating
// the two would make a BMC that exposes no counters look like healthy hardware,
// which is the most dangerous possible failure mode for this tool.
type HostTelemetry struct {
	Host        string
	CollectedAt time.Time

	// CorrectableErrors maps a DIMM identifier to its correctable ECC count.
	CorrectableErrors map[string]float64

	// UncorrectableErrors maps a DIMM identifier to its uncorrectable ECC count.
	// Any non-zero value here is a fault that already escaped correction.
	UncorrectableErrors map[string]float64

	// Temperatures maps a sensor name to its reading in Celsius.
	Temperatures map[string]float64

	// ThermalMargins maps a sensor name to the degrees remaining before its
	// upper critical threshold. Negative means the threshold is exceeded.
	ThermalMargins map[string]float64

	// Err is set when this host could not be polled. One unreachable BMC must
	// not fail the fleet run, so the error travels with the result.
	Err error
}

func newHostTelemetry(host string) HostTelemetry {
	return HostTelemetry{
		Host:                host,
		CollectedAt:         time.Now(),
		CorrectableErrors:   map[string]float64{},
		UncorrectableErrors: map[string]float64{},
		Temperatures:        map[string]float64{},
		ThermalMargins:      map[string]float64{},
	}
}

// Options tunes a fleet poll.
type Options struct {
	// Concurrency bounds simultaneous BMC connections. BMCs are small embedded
	// controllers with shallow connection queues; hitting a fleet with unbounded
	// parallelism produces timeouts that look like hardware faults.
	Concurrency int

	// PerHostTimeout caps one host. An unresponsive BMC is common and must not
	// stall the run.
	PerHostTimeout time.Duration
}

// DefaultOptions are conservative values suitable for a production fleet.
func DefaultOptions() Options {
	return Options{Concurrency: 16, PerHostTimeout: 30 * time.Second}
}

// Fleet polls every target, returning one result per target in input order.
//
// Every target always yields a result. A failure is reported in the result's Err
// field rather than aborting the run, because "this BMC is unreachable" is
// itself an operational finding, and because one bad host must not hide the
// other nine hundred.
func Fleet(ctx context.Context, targets []Target, opts Options) ([]HostTelemetry, error) {
	if len(targets) == 0 {
		return nil, ErrNoTargets
	}
	if opts.Concurrency <= 0 {
		opts.Concurrency = DefaultOptions().Concurrency
	}
	if opts.PerHostTimeout <= 0 {
		opts.PerHostTimeout = DefaultOptions().PerHostTimeout
	}

	results := make([]HostTelemetry, len(targets))
	sem := make(chan struct{}, opts.Concurrency)
	var wg sync.WaitGroup

	for i, t := range targets {
		wg.Add(1)
		go func(i int, t Target) {
			defer wg.Done()

			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				r := newHostTelemetry(t.Host)
				r.Err = ctx.Err()
				results[i] = r
				return
			}

			hostCtx, cancel := context.WithTimeout(ctx, opts.PerHostTimeout)
			defer cancel()
			results[i] = Host(hostCtx, t)
		}(i, t)
	}

	wg.Wait()
	return results, nil
}

// httpClientFor builds an HTTP client owned exclusively by one BMC connection.
//
// This is not merely tidy — it is required for correctness under concurrency.
// When gofish is left to build its own client it copies TLSClientConfig *by
// pointer* from http.DefaultTransport and sets ForceAttemptHTTP2, so every
// client it creates ends up sharing one *tls.Config. Go's HTTP/2 setup then
// mutates that shared config (appending to NextProtos) from each transport,
// which races when clients are created in parallel — precisely what fleet
// polling does. Supplying a client with its own tls.Config removes the sharing.
//
// It also gives each host an isolated connection pool, so one slow BMC cannot
// occupy connections another host is waiting on.
func httpClientFor(t Target) *http.Client {
	base := http.DefaultTransport.(*http.Transport)
	return &http.Client{
		Transport: &http.Transport{
			Proxy:                 base.Proxy,
			DialContext:           base.DialContext,
			MaxIdleConns:          4,
			IdleConnTimeout:       30 * time.Second,
			ExpectContinueTimeout: base.ExpectContinueTimeout,
			TLSHandshakeTimeout:   10 * time.Second,
			ForceAttemptHTTP2:     true,
			// Owned by this transport alone; never shared.
			TLSClientConfig: &tls.Config{
				MinVersion:         tls.VersionTLS12,
				InsecureSkipVerify: t.Insecure, //nolint:gosec // operator-controlled; BMCs ship self-signed certs
			},
		},
	}
}

// Host polls a single BMC.
func Host(ctx context.Context, t Target) HostTelemetry {
	out := newHostTelemetry(t.Host)

	client, err := gofish.ConnectContext(ctx, gofish.ClientConfig{
		Endpoint:   t.Endpoint,
		Username:   t.Username,
		Password:   t.Password,
		Insecure:   t.Insecure,
		HTTPClient: httpClientFor(t),
	})
	if err != nil {
		out.Err = fmt.Errorf("connect %s: %w", t.Endpoint, err)
		return out
	}
	defer client.Logout()

	systems, err := client.Service.Systems()
	if err != nil {
		out.Err = fmt.Errorf("list systems: %w", err)
		return out
	}
	for _, sys := range systems {
		collectMemory(sys, &out)
	}

	chassis, err := client.Service.Chassis()
	if err != nil {
		// Thermal data is useful but not essential. Memory data already
		// collected is still worth returning, so this is not a host failure.
		return out
	}
	for _, ch := range chassis {
		collectThermal(ch, &out)
	}

	return out
}

// collectMemory records ECC error counts per DIMM.
//
// Redfish splits these across CurrentPeriod and LifeTime, and every field is a
// pointer because vendors populate them inconsistently — several expose only a
// health status and omit the counters entirely. A nil pointer therefore means
// "not reported" and is skipped, never coerced to zero.
func collectMemory(sys *schemas.ComputerSystem, out *HostTelemetry) {
	modules, err := sys.Memory()
	if err != nil {
		return
	}
	for _, m := range modules {
		metrics, err := m.Metrics()
		if err != nil || metrics == nil {
			continue
		}

		name := m.Name
		if name == "" {
			name = m.ID
		}

		if v, ok := sumReported(
			metrics.LifeTime.CorrectableECCErrorCount,
			metrics.CurrentPeriod.CorrectableECCErrorCount,
		); ok {
			out.CorrectableErrors[name] = v
		}
		if v, ok := sumReported(
			metrics.LifeTime.UncorrectableECCErrorCount,
			metrics.CurrentPeriod.UncorrectableECCErrorCount,
		); ok {
			out.UncorrectableErrors[name] = v
		}
	}
}

// sumReported adds the non-nil values, reporting whether any were present.
//
// LifeTime is preferred conceptually but CurrentPeriod is what some vendors
// populate, so both are summed. ok is false only when neither was reported,
// which keeps "BMC is silent" distinguishable from "counter is zero".
func sumReported(values ...*int) (float64, bool) {
	var total float64
	var any bool
	for _, v := range values {
		if v != nil {
			total += float64(*v)
			any = true
		}
	}
	return total, any
}

// collectThermal records sensor readings and their margin to the upper critical
// threshold.
//
// Margin matters more than the raw reading: 70°C is unremarkable on one sensor
// and an imminent shutdown on another, and only the threshold says which.
func collectThermal(ch *schemas.Chassis, out *HostTelemetry) {
	thermal, err := ch.Thermal()
	if err != nil || thermal == nil {
		return
	}
	for _, temp := range thermal.Temperatures {
		if temp.ReadingCelsius == nil {
			continue
		}
		name := temp.MemberID
		if name == "" {
			name = string(temp.PhysicalContext)
		}
		if name == "" {
			continue
		}

		reading := *temp.ReadingCelsius
		out.Temperatures[name] = reading

		if temp.UpperThresholdCritical != nil {
			out.ThermalMargins[name] = *temp.UpperThresholdCritical - reading
		}
	}
}
