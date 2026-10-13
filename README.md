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

[URGENT] node-114  CPU0_Die reaches upper critical in ~44h at 0.28°C/hour — find the airflow fault now
           margin 12.0°C smoothed, 12.9°C latest reading
           26.4°C of headroom lost over 95h45m0s (384 readings in 24 buckets)
           fit R²=0.93
           recent half closing 1.1x the earlier half
           crosses upper critical in ~44h if the rate holds — extrapolated past the last reading, not measured
           confidence 93%

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
that has been flat for a year, and the inlet sensor sitting at 5°C of margin where
its fan curve has held it since the node was racked. ECC caught every one of those
errors, and that inlet is a design point. Neither is failing.

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

## The same premise on a second signal

One measurement cannot tell you whether that is a method or a coincidence, so it is
applied to a thermal margin — the degrees a sensor has left before its upper
critical threshold — which is a *gauge* rather than a counter, and inverts in
exactly the same way:

| sensor | margin | trend | threshold alerting says | reality |
|---|---|---|---|---|
| CPU1_Inlet | 5°C | flat for a month | 🔴 page someone | fine, that is the fan curve |
| CPU0_Die | 38°C → 12°C | lost 26°C in four days | 🟢 silent | **fan, dust or pump; crosses critical on a schedule** |

A sensor that has held 5°C of headroom since the node was racked is a thermal design
point. One that had 38°C and now has 12°C is losing cooling, and dividing the
remaining margin by the slope says roughly when it stops being a margin. Pinned by
`TestAnalyzeThermal_StableLowMarginIsNotAFinding` and
`TestAnalyzeThermal_ShrinkingHighMarginIsUrgent`.

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

Three of those change on a gauge, and the differences are the reason the second
analyser is not a copy of the first:

- **No epochs.** A counter's only discontinuity is a power-cycle reset, and only the
  latest epoch is trended. A margin falls every time load rises; a dip is Tuesday
  afternoon, not a new epoch, so the whole retained window is fitted.
- **Noise has to be resisted, not just discounted.** The readings are collapsed into
  bucket medians before anything is fitted, so a spike shorter than half a bucket is
  outvoted rather than averaged in. A robust regression over every reading resists
  the same noise, but costs the pairwise slopes of up to 2,000 points per sensor and
  this tool fits thousands of series per poll. Two floors then sit under the verdict:
  the fit must account for at least 3°C across the window — BMCs commonly report
  whole degrees — and a line must have explained at least a quarter of the movement.
  A sensor oscillating 8°C either side of a flat mean clears the first floor and is
  caught by the second, which `TestAnalyzeThermal_LoadOscillationIsNotShrinkage`
  says out loud, because it is not the floor you would guess.
- **A minimum of a day.** Load and hall ambient both cycle daily, so a window
  shorter than a day can show half a cycle, which looks exactly like a trend. What
  that does not fix: a cycle *longer* than the window still looks like one.

And one thing a gauge gets that a counter does not: a crossing date.
`crosses upper critical in ~44h` is the number worth acting on, so it is published
only when the fit supports extrapolation — a sensor genuinely losing ground while
swinging hard with load gets the rate and no date, because a date is what somebody
schedules around.

A thin history is treated differently in the two analysers, deliberately. A counter
with a non-zero value and too little history produces a low-confidence
`insufficient-history` note, because a non-zero error count is abnormal on its own.
A small margin is not abnormal — it is the fan curve — so there is nothing to
surface until the trend is claimable, and the thermal analyser stays silent
(`TestAnalyzeThermal_ThinHistoryDoesNotGuess`). The exception is a margin that is
already at or past zero, which is reported immediately at full confidence: that is
an observation, not a projection.

## Design decisions worth arguing with

**A measurement the BMC did not report is absent, never zero.** Vendors populate
Redfish memory metrics inconsistently; several expose only a health status. Coercing
a missing counter to `0` would make a blind BMC indistinguishable from healthy
hardware — the most dangerous possible failure mode for this tool. The same rule
survives into the metrics output: a counter the BMC never returned has no series in
the `.prom` file, and a rate the analysis refused to claim is not exported as
`0.0`. Pinned by `TestHost_UnreportedCounterIsAbsentNotZero`,
`TestWriteProm_OnlyCountersTheBMCReportedAppear` and
`TestWriteProm_UncomputableRateIsAbsentNotZero`.

On a thermal margin the same rule bites harder, because zero is not a neutral value
there: 0°C of margin is a sensor sitting exactly on the threshold the vendor said
not to cross. "No reading" and "on the edge of tripping" are opposite conditions
that would render identically. A sensor whose BMC reports a temperature but no upper
critical threshold has no margin at all and is not synthesised one
(`TestHost_SensorWithoutACriticalThresholdHasNoMargin`,
`TestWriteProm_UnreportedSensorIsAbsentNotZero`).

**One dead BMC never hides the fleet.** Every target always yields a result;
failures travel in the result rather than aborting the run. An unreachable BMC is
itself a finding — a node nobody can see is not a node that is fine.

**Concurrency is bounded and per-host isolated.** BMCs are small embedded
controllers with shallow connection queues; unbounded parallelism produces timeouts
that look exactly like hardware faults.

**Retention is a window on the analysis, not a cut on the file.** `-retain` drops
samples outside the window, but two positions survive regardless of age: the sample
on the high side of the latest counter reset, without which the epoch boundary and
the reset count that discounts confidence both vanish, and enough of the epoch to
stay trendable, without which a long history collapses into a 20%-confidence
`insufficient-history` note. On the pinned fixture, a naive age-only cut moves the
rate from 66.7 to 78.5 errors/hour, the acceleration from 16.6x to 13.6x and the
confidence from 56% to 52% — same DIMM, different report. Pinned by
`TestPrune_KeepsTheResetBoundaryAndTheVerdict` and
`TestPrune_WillNotStarveATrendableSeries`.

The two series shapes land in one file and are pruned by different rules, which is
not symmetry for its own sake. The counter rule protects enough history for a rate
at four samples over thirty minutes — far below what a margin trend needs — so a
sensor held to it is trimmed down to this afternoon's readings and falls silent over
data retention chose to delete. The reset clamp is the quieter half: a gauge has no
resets to protect, and where that clamp does fire is a sensor whose BMC went dark
and came back, refusing to drop the stale block from before the gap so the fit then
spans it. Pinned by `TestPruneGauge_KeepsEnoughToStillSpeakAboutTheSensor`.

**State writes are atomic.** A truncated state file loses every trend the fleet has
accumulated, so saves go to a temp file and are renamed into place. The metrics file
gets the same treatment for a different reason: node_exporter reads it on its own
schedule, and a half-written file is a parse error that discards the whole scrape.

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
| `HistoryAnalyze` | 500 hosts × 16 DIMMs = **8,000 series**, 96 samples each | **16.6 ms** | 66,841 |
| `AnalyzeECC` | one week of 1-minute samples (10,080 points) | 309 µs | 17 |
| `AnalyzeThermal` | one week of 1-minute readings (10,080 points) | **47 µs** | 25 |
| `LeastSquaresPerHour` | 10,000 points | 147 µs | **0** |

`AnalyzeThermal` is the cheaper of the two on identical input, which is worth saying
because the smoothing reads like added work: the least-squares fit runs over at most
24 bucket medians rather than over every reading, so collapsing the series pays for
itself several times over.

A 500-node fleet analyses in about 16 ms, so analysis is never the bottleneck —
polling latency dominates entirely, which is why concurrency and per-host timeouts
are the tunable knobs.

## Usage

```bash
go build ./cmd/rack-sentinel

rack-sentinel -targets fleet.csv -history state.json [-retain 336h] [-prom FILE]
              [-insecure] [-json]
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

`-retain` bounds that file. It defaults to two weeks, drops samples outside the
window, and drops whole series for hosts, DIMMs and sensors nobody has polled
inside it;
without that, every host ever polled keeps its series forever and the cost of a poll
grows with uptime. `-retain 0` keeps everything.

### Prometheus

`-prom` writes a file for node_exporter's textfile collector, replaced in a single
rename so a scrape landing mid-write cannot read half of it:

```bash
rack-sentinel -targets fleet.csv -history state.json \
  -prom /var/lib/node_exporter/textfile/rack-sentinel.prom
```

```
rack_sentinel_ecc_correctable_errors_total{host="node-114",dimm="DIMM_B2"} 2914
rack_sentinel_ecc_correctable_errors_per_hour{host="node-114",dimm="DIMM_B2"} 41.3
rack_sentinel_ecc_acceleration_ratio{host="node-114",dimm="DIMM_B2"} 7.9
rack_sentinel_thermal_margin_celsius{host="node-114",sensor="CPU0_Die"} 12.9
rack_sentinel_thermal_margin_shrink_celsius_per_hour{host="node-114",sensor="CPU0_Die"} 0.28
rack_sentinel_thermal_shrink_acceleration_ratio{host="node-114",sensor="CPU0_Die"} 1.1
rack_sentinel_thermal_hours_to_critical{host="node-114",sensor="CPU0_Die"} 43.6
rack_sentinel_finding_severity{host="node-114",component="DIMM_B2",kind="ecc.degrading"} 3
rack_sentinel_finding_confidence_ratio{host="node-114",component="DIMM_B2",kind="ecc.degrading",severity="URGENT"} 0.94
rack_sentinel_bmc_unreachable{host="node-207"} 1
```

The metrics file ignores `-min-severity`: deciding what deserves a page is
Prometheus's job once the numbers are in it. `rack_sentinel_poll_timestamp_seconds`
is exported so a cron that stopped running alerts on itself. Hosts that answered get
no `rack_sentinel_bmc_unreachable` series at all — the fleet totals carry the
denominator.

`rack_sentinel_thermal_hours_to_critical` is the one to alert on rather than
`rack_sentinel_thermal_margin_celsius`, and it is absent for every sensor the
analysis would not put a date on — a margin that is not closing, or closing on a fit
no straight line described. The acceleration ratio is held to the same gate, because
on a sensor that is merely swinging with load the earlier half is flat and the recent
half happens to be rising, which the ratio reports as its 999 cap: a dramatic number
that flips back next poll. A flat sensor does export a shrink rate of `0`, which is a
measurement rather than a guess — the fit genuinely says zero.

One limit worth knowing before you alert on it: every sample is stamped at scrape
time, because node_exporter rejects a textfile whose samples carry their own
timestamps. So a host whose BMC did not answer this poll still exports its last
known counters, looking as current as everything else. That is what
`rack_sentinel_bmc_unreachable` is for — join on it before trusting a host's other
series.

Exit codes: `0` clean · `1` findings at or above `-min-severity` · `2` usage ·
`3` could not run. Suitable as a cron or CI gate.

`-insecure` skips BMC TLS verification. BMCs almost universally ship self-signed
certificates, so this is usually required — but it is opt-in, not the default.

## Scope and limits

- **Two signals are analysed: correctable ECC errors and thermal margins.**
  Uncorrectable counts and raw temperatures are collected and persisted but not yet
  trended. Uncorrectable errors in particular need no trend — any non-zero value is
  a fault that already escaped correction — and reporting them is the obvious next
  analyser.
- **The thermal analyser cannot see a cycle longer than its window.** Bucket
  medians and a one-day minimum span handle load spikes and the daily ambient
  swing. A weekly batch job, or a hall whose cooling drifts with the season, can
  still present half a cycle as a trend. The fit floor catches the worst of it and
  the confidence score reflects the rest, but nothing here distinguishes a slow
  cycle from slow cooling loss.
- **A margin needs an upper critical threshold to exist at all.** Sensors where the
  BMC leaves `UpperThresholdCritical` unset are collected as temperatures and never
  trended, because there is nothing to measure headroom against. On some firmware
  that is most of the sensors on the board.
- **Credentials come from a CSV.** Not ideal, but it is what a provisioning system
  already emits, and the alternative — flags — puts passwords in the process table
  where every user on the box can read them. A secrets-manager backend is the
  obvious next step.
- **History is bounded two ways:** `-retain`, two weeks by default, and a hard cap
  of 2,000 samples per counter. Trends care about shape, not archaeology. What
  retention does not fix is width — 8,000 series is a large JSON file whichever
  window it covers, and every run still reads and rewrites all of it. A fleet that
  size wants the series in Prometheus rather than in this file, which is what
  `-prom` is for.
- **Tested against a Redfish mock, not against real BMC firmware.** Vendor
  implementations diverge; the pointer-vs-zero handling exists precisely because of
  that, but real iDRAC/iLO/XCC validation is not yet done and is the biggest gap.

## Tests

```bash
go test -race ./...
```

Coverage: `internal/analyze` 96.0%, `internal/report` 94.3%, `internal/collect`
85.5%, `internal/atomicfile` 66.7%, `cmd` 56.0%. Both low figures are honest rather
than accidental: `run()` and the output formatting in `cmd` are not covered, though
the history, retention and target-parsing logic under them is, and `atomicfile`'s
write, sync, close and chmod failure branches need an injected filesystem to reach,
which is not worth the indirection for forty lines.

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

The thermal half has no comparable literature behind it and is not claimed to. It
rests on a mechanical argument — a fan, a heatsink or a pump degrades gradually and
monotonically, so the headroom it buys shrinks before it runs out — which is
plausible and untested at scale. The numbers it reports are the slope and the fit it
measured; what they predict is an extrapolation, which is why the crossing date is
gated on the fit and labelled as extrapolated wherever it appears.

## Licence

MIT — see [LICENSE](LICENSE).
