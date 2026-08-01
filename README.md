# rack-sentinel

Predictive hardware failure detection from BMC telemetry, over Redfish.

Answers the question a data-centre operator actually has — *which of these nodes is
about to cost me an outage?* — which is **not** the same as *which nodes have high
counters*.

```
$ rack-sentinel -targets fleet.csv -history state.json
rack-sentinel — 2026-08-01T18:04:11Z
  hosts polled  512
  hosts failed  1

[URGENT] node-114  DIMM_B2 correctable errors accelerating (7.9x) at 41.3/hour — replace before the next uncorrectable
           rate 41.3 errors/hour over 71h0m0s (284 samples)
           total 2914 correctable errors this epoch
           fit R²=0.97
           recent half running 7.9x the earlier half
           confidence 94%

[WATCH ] node-207  BMC unreachable — no hardware visibility
           connect https://10.0.2.7: context deadline exceeded
           confidence 100%
```

Note what is **absent** from that report: the node with 11,000 correctable errors
that has been flat for a year. ECC caught every one of them. It is not failing.

## The premise

Correctable ECC errors are, by definition, errors the memory subsystem *handled*.
The count alone tells you almost nothing. What predicts an uncorrectable error —
the one that halts the host — is the **derivative**.

| DIMM | total | trend | threshold alerting says | reality |
|---|---|---|---|---|
| A | 11,000 | flat for a year | 🔴 page someone | fine, ECC is working |
| B | 200 | doubled this week | 🟢 silent | **about to take the node down** |

`rack-sentinel` ranks by trend and acceleration, so it reports B and stays quiet
about A. That inversion is the whole point, and it is pinned by tests:
`TestAnalyzeECC_FlatHighCountIsNotUrgent` and
`TestAnalyzeECC_AcceleratingLowCountIsUrgent`.

## How it decides

- **Rate** — least-squares slope over the current epoch, in errors/hour.
- **Acceleration** — recent-half rate ÷ earlier-half rate. A part that is getting
  worse has a deadline; a part that is merely bad and stable does not. Acceleration
  outranks absolute rate.
- **Counter resets** — BMC counters restart at zero on host power-cycle. That drop
  is a new epoch, not a negative rate. Only the latest epoch is trended.
- **Confidence** — eroded by thin history, a poor linear fit (bursty counters), and
  frequent resets. A scary extrapolation from three points in five minutes is how a
  tool teaches people to ignore it, so short series produce a low-confidence
  `insufficient-history` note instead of an alarm.

## Design decisions worth arguing with

**A counter the BMC did not report is absent, never zero.** Vendors populate
Redfish memory metrics inconsistently; several expose only a health status. Coercing
a missing counter to `0` would make a blind BMC indistinguishable from healthy
hardware — the most dangerous possible failure mode for this tool. Pinned by
`TestHost_UnreportedCounterIsAbsentNotZero`.

**One dead BMC never hides the fleet.** Every target always yields a result;
failures travel in the result rather than aborting the run. An unreachable BMC is
itself a finding — a node nobody can see is not a node that is fine.

**Concurrency is bounded and per-host isolated.** BMCs are small embedded
controllers with shallow connection queues; unbounded parallelism produces timeouts
that look exactly like hardware faults.

**State writes are atomic.** A truncated state file loses every trend the fleet has
accumulated, so saves go to a temp file and are renamed into place.

## A data race when configuring many transports concurrently

`rack-sentinel` builds its own `*http.Client`, with an explicitly non-nil
`TLSClientConfig`, per BMC. That is not stylistic — without it, `go test -race`
fails.

Creating many `http.Transport`s concurrently with `ForceAttemptHTTP2: true` and a
nil `TLSClientConfig` races inside the standard library, in
`Transport.onceSetNextProtoDefaults` → `http2configureTransports`:

```
Read at  net/http.http2configureTransports   h2_bundle.go:7539
Write at net/http.(*Transport).onceSetNextProtoDefaults  transport.go:461
```

**This is not a bug in gofish.** The first version of this README said it was —
gofish copies `TLSClientConfig` from `http.DefaultTransport` and sets
`ForceAttemptHTTP2`, which looked like the cause. It is not: `DefaultTransport`'s
`TLSClientConfig` is nil, and `http2configureTransports` gives each transport its
own `new(tls.Config)`. Reproducing the same pattern with **plain `net/http` and no
gofish at all** produces an identical race signature, on Go 1.26.5.

Giving each transport an explicitly-constructed `tls.Config` avoids it. Whether the
underlying stdlib behaviour is a Go bug or intended is not something this project
claims to have established.

*Lesson recorded here because it was nearly published as an upstream bug report: a
plausible root cause that fits the stack trace is still a guess until the
counterfactual is tested.*

## Benchmarks

`go test -bench=. -benchmem ./...` on an M-series Mac, Go 1.26:

| benchmark | scale | time | allocs |
|---|---|---|---|
| `HistoryAnalyze` | 500 hosts × 16 DIMMs = **8,000 series**, 96 samples each | **15.5 ms** | 66,839 |
| `AnalyzeECC` | one week of 1-minute samples (10,080 points) | 309 µs | 17 |
| `LeastSquaresPerHour` | 10,000 points | 147 µs | **0** |

A 500-node fleet analyses in about 16 ms, so analysis is never the bottleneck —
polling latency dominates entirely, which is why concurrency and per-host timeouts
are the tunable knobs.

## Usage

```bash
go build ./cmd/rack-sentinel

rack-sentinel -targets fleet.csv -history state.json [-insecure] [-json]
```

`fleet.csv` is `host,endpoint,username,password`; `#` comments are skipped:

```csv
# rack 14
node-01,https://10.0.0.11,admin,secret
node-02,https://10.0.0.12,admin,secret
```

**Trends need history.** Run it on a schedule with `-history` pointing at the same
file. A single run can only report counters, not whether they are getting worse —
the tool says so rather than pretending otherwise.

Exit codes: `0` clean · `1` findings at or above `-min-severity` · `2` usage ·
`3` could not run. Suitable as a cron or CI gate.

`-insecure` skips BMC TLS verification. BMCs almost universally ship self-signed
certificates, so this is usually required — but it is opt-in, not the default.

## Scope and limits

- **ECC correctable errors are the only analyser so far.** Thermal margins and
  uncorrectable counts are collected and persisted but not yet trended.
- **Credentials come from a CSV.** Not ideal, but it is what a provisioning system
  already emits, and the alternative — flags — puts passwords in the process table
  where every user on the box can read them. A secrets-manager backend is the
  obvious next step.
- **History is capped at 2,000 samples per counter** (~3 weeks at a 15-minute poll).
  Trends care about shape, not archaeology.
- **Tested against a Redfish mock, not against real BMC firmware.** Vendor
  implementations diverge; the pointer-vs-zero handling exists precisely because of
  that, but real iDRAC/iLO/XCC validation is not yet done and is the biggest gap.

## Tests

```bash
go test -race ./...
```

Coverage: `internal/analyze` 91.6%, `internal/collect` 85.5%, `cmd` 50.6%. The CLI
figure is honest — `run()` and the output formatting are not yet covered; the
history and target-parsing logic under it is.

Collector tests run against a real `httptest` Redfish server rather than a stubbed
client, because the vendor-inconsistency bugs live in the JSON decoding and pointer
semantics. A stub would only prove the stub matches the test's assumptions.

## Attribution

Redfish transport, auth and schema decoding come from
[gofish](https://github.com/stmcginnis/gofish) (Apache-2.0), the established Go
Redfish client — reimplementing it would have been wasted effort. What is original
here is the analysis layer: trend and acceleration scoring, confidence, epoch
handling, and fleet-scale collection with per-host isolation. Existing Redfish
exporters scrape and display; none of them decide what is *degrading*.

The premise that correctable-error *rate* predicts uncorrectable failure follows the
published large-scale DRAM studies (Schroeder et al., *DRAM Errors in the Wild*,
SIGMETRICS 2009; Meza et al., *Revisiting Memory Errors at Scale*, DSN 2015).

## Licence

MIT — see [LICENSE](LICENSE).
