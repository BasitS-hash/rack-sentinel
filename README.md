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

[PLAN  ] node-114  Bay 1 spent in ~12w at 1.00%/day — schedule a swap
           81% of media life left, wearing 1.00%/day
           14 steps in the estimate over 335h0m0s (336 readings)
           fit R²=0.99
           recent half wearing 1.0x the earlier half
           spent in ~12w if the rate holds — extrapolated from the vendor's own life estimate, not measured
           vendor FailurePredicted=false — the firmware disagrees, though Redfish cannot tell a false from an unimplemented field
           confidence 75%

[WATCH ] node-207  BMC unreachable — no hardware visibility
           connect https://10.0.2.7: context deadline exceeded
           confidence 100%
```

Note what is **absent** from that report: the node with 11,000 correctable errors
that has been flat for a year, the inlet sensor sitting at 5°C of margin where its
fan curve has held it since the node was racked, and the SSD that has reported 11%
of media life left for a fortnight without moving. ECC caught every one of those
errors, that inlet is a design point, and that SSD will outlive the node. None of
them is failing. Two of the three are what a threshold alerter pages for.

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

One measurement cannot tell you whether that is a method or a coincidence, and two
can still be two coincidences, so there are three. The second is a thermal margin —
the degrees a sensor has left before its upper critical threshold — which is a
*gauge* rather than a counter, and inverts in exactly the same way:

| sensor | margin | trend | threshold alerting says | reality |
|---|---|---|---|---|
| CPU1_Inlet | 5°C | flat for a month | 🔴 page someone | fine, that is the fan curve |
| CPU0_Die | 38°C → 12°C | lost 26°C in four days | 🟢 silent | **fan, dust or pump; crosses critical on a schedule** |

A sensor that has held 5°C of headroom since the node was racked is a thermal design
point. One that had 38°C and now has 12°C is losing cooling, and dividing the
remaining margin by the slope says roughly when it stops being a margin. Pinned by
`TestAnalyzeThermal_StableLowMarginIsNotAFinding` and
`TestAnalyzeThermal_ShrinkingHighMarginIsUrgent`.

## And a third, where somebody else has already published an answer

The third is a drive's `PredictedMediaLifeLeftPercent` — the vendor's estimate of
media life remaining, 100 down to 0:

| drive | life left | trend | threshold alerting says | reality |
|---|---|---|---|---|
| Bay 4 | 11% | flat for a fortnight | 🔴 page someone | fine, it will outlive the node |
| Bay 1 | 95% → 81% | 14% gone in a fortnight | 🟢 silent | **spent in about twelve weeks** |

Same inversion, pinned by `TestAnalyzeDriveWear_StableLowLifeIsNotAFinding` and
`TestAnalyzeDriveWear_FastDecliningHighLifeAlerts`. What makes it worth having
rather than a third copy of the same code:

**The vendor ships its own answer on the same part.** Redfish exposes
`FailurePredicted`, a boolean the firmware sets from its own absolute thresholds.
That is the naive signal this project argues against, available on the same drive
at the same moment, so the two can be published side by side and the disagreement
named. Both are reported and neither suppresses the other: a cleared flag over a
drive visibly burning 1% of its media a day is the false negative this tool exists
to catch, and a flagged drive with no estimate to trend — the common SAS case — is
reported as the vendor's opinion under its own `kind`. One drive still produces one
finding; where both fire, the agreement is a line in the evidence rather than a
second row.

**The signal is a staircase, and that needed a different floor.** The estimate
steps by whole percent and sits still between steps, so a line fitted to it is
mostly a statement about *when the steps happened*. Of the thermal analyser's three
defences, only one survives the move, and it is not the obvious split:

- **Bucket medians buy nothing.** They exist to outvote amplitude noise, and there
  is none here — the value is integral and never wrong, only stale.
- **R² is no defence at all.** A straight line explains about **72% of a single
  step**, past both thermal fit floors, so that analyser would have published a rate
  *and* a replacement date off one edge.
- **The magnitude floor would, honestly, have worked on the common case.** A single
  one-percent step fits to about 1.4 points across a fortnight, under the three
  degrees the thermal analyser demands. It is still the wrong instrument: three
  degrees is a statement about sensor rounding and means the same on every sensor,
  whereas three percentage points means almost nothing on a drive at 95% and most of
  what is left of a drive at 4% — and it is not invariant to the quantum. Redfish
  types this field as a float, and a firmware reporting in fives clears a
  three-point floor on its very first step.

So what this analyser counts is the steps themselves — how many independent
observations the slope rests on, whatever size they come in: three before it will
claim a rate, five before it will put a date on anything, and that same count is
what feeds the confidence score. `TestAnalyzeDriveWear_OneStepIsNotATrend` pins all
of it, including the R² that would have broken it and the magnitude floor that
would not have.

**It declines where a counter rises**, so the series is inverted into *wear* once,
in `wearOf`, and every shared helper downstream — slope, acceleration, and the
epoch split — then sees the same "bigger is worse" shape it sees for a DIMM.

**A drive swap is this signal's counter reset**, and it is not as clean as one. A
BMC counter only ever resets to zero, so any decrease is unambiguously a new epoch;
a vendor estimate can tick back *up* by a point when the firmware recalculates, and
treating that as a new drive would discard a fortnight of history and silence a
drive that is genuinely wearing. So the epoch split is shared with the ECC
analyser but given a minimum jump
(`TestAnalyzeDriveWear_RecalibrationIsNotAReplacement`).

**It is an estimate, not a measurement.** Every other number in this tool is
something a BMC observed. This one is a model output, computed differently by every
vendor and not comparable between two drives in the same chassis. Its *derivative*
is still a real statement about how hard the drive is being written, which is why
trending it is worth doing — but a replacement date extrapolated from somebody
else's model has one more layer of someone else's assumptions in it than a date
extrapolated from a counter. So the horizon is rounded to weeks rather than quoted
to the hour, it says in the evidence line that it came off the vendor's estimate,
and every projection off it takes a flat 25% confidence discount that is visible
rather than folded into the fit.

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

Two more change on a quantised estimate, which is why there is a third analyser
rather than a second call to this one:

- **The evidence is steps, not readings.** A drive polled every fifteen minutes for
  a fortnight returns 1,344 readings and perhaps fourteen distinct values. The
  fourteen are the measurements; the rest is the same measurement repeated, so the
  floors, the confidence and the projection gate all count steps. Bucket medians buy
  nothing here — there is no amplitude noise to outvote, only uncertainty about when
  each step happened — and R² buys less than nothing, because a single step scores
  about 0.7.
- **A week as the minimum, and a jump as the epoch.** The estimate moves about once
  a day on a drive that will be spent inside the year, so a shorter window cannot
  hold enough steps to fit anything to. And unlike a counter, where any decrease is
  a reset, a new epoch here needs a jump of twenty points: a vendor estimate can
  tick back up by one when the firmware recalculates, and treating that as a new
  drive would throw away the history behind it.

And one thing a gauge gets that a counter does not: a crossing date.
`crosses upper critical in ~44h` is the number worth acting on, so it is published
only when the fit supports extrapolation — a sensor genuinely losing ground while
swinging hard with load gets the rate and no date, because a date is what somebody
schedules around.

A thin history is treated differently by the counter than by the other two,
deliberately. A counter with a non-zero value and too little history produces a
low-confidence `insufficient-history` note, because a non-zero error count is
abnormal on its own. A small margin is not abnormal — it is the fan curve — and an
estimate below 100% is not abnormal either, it is every drive that has ever been
written to, so there is nothing to surface until the trend is claimable and both
stay silent (`TestAnalyzeThermal_ThinHistoryDoesNotGuess`,
`TestAnalyzeDriveWear_ThinHistoryDoesNotGuess`). The exceptions are a margin
already at or past zero and a drive already reporting 0% of media life left, which
are reported immediately at full confidence: those are observations, not
projections.

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

On a drive media-life estimate it is sharper still, because there the fabricated
zero is not merely optimistic, it is **inverted**: 0% of media life left is the
vendor's verdict that a drive is finished. Most firmware populates
`PredictedMediaLifeLeftPercent` for NVMe and omits it entirely for SAS and SATA, so
coercing absent to zero would not make a blind BMC look healthy — it would condemn
a chassis of working disks. Pinned by
`TestHost_DriveWithoutALifeEstimateIsAbsentNotZero` and
`TestWriteProm_UnreportedDriveIsAbsentNotZero`.

And `FailurePredicted` is absent for a third reason, which is the only one of the
three that is not about what the BMC said. Redfish types it as a plain boolean with
no null, so a firmware that never implemented it decodes identically to one
reporting a clear drive. There is no way to tell those apart, so only the flagged
drives get an entry and only they get a metric series: a `0` here would turn "we
cannot tell" into "the vendor says this drive is fine"
(`TestHost_VendorFailurePredictionIsRecordedOnlyWhenSet`,
`TestWriteProm_VendorPredictionIsOneOrAbsentNeverZero`).

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

The three series shapes land in one file and are pruned by three different rules,
which is not symmetry for its own sake. The counter rule protects enough history
for a rate at four samples over thirty minutes — far below what a margin trend
needs — so a sensor held to it is trimmed down to this afternoon's readings and
falls silent over data retention chose to delete. The reset clamp is the quieter
half: a gauge has no resets to protect, and where that clamp does fire is a sensor
whose BMC went dark and came back, refusing to drop the stale block from before the
gap so the fit then spans it. Pinned by
`TestPruneGauge_KeepsEnoughToStillSpeakAboutTheSensor`.

A drive series needs both halves of both other rules, which is why it has its own.
It needs the epoch clamp the gauge rule does not have, because a swap in the bay is
where the current drive's history begins and dropping the reading on the worn side
of it makes the jump — and with it the epoch split — disappear, leaving a fit that
spans two drives and reports the new one as recovering. And it needs a far higher
floor than the counter rule protects, because the estimate steps about once a day:
held to four samples over thirty minutes, or even to the gauge's day, a drive is
pruned down to one step and goes quiet over data retention chose to delete. Pinned
by `TestPruneWear_KeepsEnoughToStillSpeakAboutTheDrive` and
`TestPruneWear_KeepsTheReplacementBoundary`.

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
| `AnalyzeDriveWear` | two weeks of 15-minute readings (1,344 points) | 49 µs | 25 |
| `LeastSquaresPerHour` | 10,000 points | 147 µs | **0** |

`AnalyzeThermal` is the cheaper of the first two on identical input, which is worth
saying because the smoothing reads like added work: the least-squares fit runs over
at most 24 bucket medians rather than over every reading, so collapsing the series
pays for itself several times over.

`AnalyzeDriveWear` is the one row in that table with a real allocation cost, and it
is the price of doing the sign inversion once: it copies the series to invert it,
where the other two fit in place or over 24 buckets. 50 kB per drive per poll is a
handful of copies on a compute node and something to look at again on a
twenty-four-bay storage node — the alternative is a second definition of which
direction is dangerous, which is what the inversion exists to avoid.

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
window, and drops whole series for hosts, DIMMs, sensors and drive bays nobody has
polled inside it;
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
rack_sentinel_drive_media_life_left_percent{host="node-114",drive="Bay 1"} 81
rack_sentinel_drive_media_wear_percent_per_day{host="node-114",drive="Bay 1"} 1.0
rack_sentinel_drive_days_to_spent{host="node-114",drive="Bay 1"} 81.3
rack_sentinel_drive_vendor_failure_predicted{host="node-209",drive="Bay 3"} 1
rack_sentinel_finding_severity{host="node-114",component="DIMM_B2",kind="ecc.degrading"} 3
rack_sentinel_finding_confidence_ratio{host="node-114",component="DIMM_B2",kind="ecc.degrading",severity="URGENT"} 0.94
rack_sentinel_bmc_unreachable{host="node-207"} 1
```

The metrics file ignores `-min-severity`: deciding what deserves a page is
Prometheus's job once the numbers are in it. `rack_sentinel_poll_timestamp_seconds`
is exported so a cron that stopped running alerts on itself. Hosts that answered get
no `rack_sentinel_bmc_unreachable` series at all — the fleet totals carry the
denominator.

`rack_sentinel_drive_days_to_spent` and `rack_sentinel_drive_vendor_failure_predicted`
are the pair worth graphing together: the first is this tool's answer and the second
is the firmware's, and the drives where exactly one of them fires are the interesting
ones. The wear rate is published per **day**, unlike the other two rate families,
because a drive on course to be spent inside the year wears about 0.04%/hour and
four leading zeros is not a number anybody reads off a dashboard — the same
readability argument that put the other two in per-hour rather than the per-second
the naming conventions would prefer.

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

- **Three signals are analysed: correctable ECC errors, thermal margins and drive
  media life.** Uncorrectable counts and raw temperatures are collected and
  persisted but not yet trended. Uncorrectable errors in particular need no trend —
  any non-zero value is a fault that already escaped correction — and reporting them
  is the obvious next analyser.
- **At the default retention, a drive has to be wearing fast to be seen at all.**
  Three steps in the estimate are needed for a rate and five for a replacement
  date, and a fortnight of history holds at most fourteen steps on a drive burning
  1% a day. So the method speaks about drives with roughly fifteen months of life
  left or less, and puts a date only on drives inside about nine months. A drive
  wearing more slowly than that is reported as nothing at all — which is the right
  answer operationally, and worth knowing is not the same answer as "healthy".
  Raising `-retain` is the lever. The same floor is what keeps a fleet of
  write-heavy but healthy SSDs silent: a drive wearing a few percent a year steps
  at most once a fortnight, so there is nothing to fit and nothing is said.
- **A media-life estimate is the vendor's model, not a measurement.** Every other
  number here is something a BMC observed. Two drives from different vendors
  reporting 40% are not making the same claim, and nothing in this tool normalises
  them; only the derivative of one drive's own series is used, and projections off
  it are discounted and rounded. There is also no published large-scale validation
  of media-life estimates as a failure predictor comparable to the DRAM literature
  behind the ECC half.
- **A used spare fitted into a bay is invisible as a replacement.** The epoch split
  fires on a jump of twenty points or more, so a drive swapped for one at similar
  wear leaves a small upward step the fit reads as a drive that is not wearing, and
  the bay stays quiet until the new drive passes the old one's last reading.
- **`FailurePredicted=false` is not a clean bill of health** and is never treated as
  one. Redfish gives it no null, so a firmware that does not implement it is
  indistinguishable from one reporting a clear drive.
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

Coverage: `internal/analyze` 96.8%, `internal/report` 95.6%, `internal/collect`
88.1%, `internal/atomicfile` 66.7%, `cmd` 61.7%. Both low figures are honest rather
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

The thermal and drive halves have no comparable literature behind them and are not
claimed to. It
The thermal one rests on a mechanical argument — a fan, a heatsink or a pump
degrades gradually and monotonically, so the headroom it buys shrinks before it runs
out — which is plausible and untested at scale. The drive one rests on something
weaker again: that a vendor's own endurance model, whatever it is, is at least
monotone in writes, so its rate of change says something about how hard the drive is
being used even when its level says nothing comparable. Both report the slope and
the fit they measured; what they predict is an extrapolation, which is why the
crossing date is gated and labelled as extrapolated wherever it appears, and why the
drive horizon is rounded to weeks and discounted on top.

## Licence

MIT — see [LICENSE](LICENSE).
