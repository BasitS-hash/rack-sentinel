// Command rack-sentinel polls a fleet of BMCs over Redfish and reports hardware
// that is degrading, ranked so the urgent items are readable at fleet scale.
package main

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/BasitS-hash/rack-sentinel/internal/analyze"
	"github.com/BasitS-hash/rack-sentinel/internal/collect"
	"github.com/BasitS-hash/rack-sentinel/internal/report"
)

// Exit codes are chosen so this can gate a cron job or a CI step.
const (
	exitClean    = 0
	exitFindings = 1
	exitUsage    = 2
	exitError    = 3
)

func main() {
	os.Exit(run())
}

func run() int {
	var (
		targetsPath = flag.String("targets", "", "CSV of host,endpoint,username,password (required)")
		historyPath = flag.String("history", "", "JSON history file; read for trends, rewritten after the poll")
		retain      = flag.Duration("retain", defaultRetention, "drop history older than this; 0 keeps everything")
		concurrency = flag.Int("concurrency", collect.DefaultOptions().Concurrency, "max simultaneous BMC connections")
		timeout     = flag.Duration("timeout", collect.DefaultOptions().PerHostTimeout, "per-host timeout")
		insecure    = flag.Bool("insecure", false, "skip BMC TLS verification (BMCs usually ship self-signed certs)")
		asJSON      = flag.Bool("json", false, "emit findings as JSON")
		promPath    = flag.String("prom", "", "also write Prometheus textfile metrics to this path")
		minSeverity = flag.String("min-severity", "watch", "report at or above: info|watch|plan|urgent")
	)
	flag.Usage = usage
	flag.Parse()

	if *targetsPath == "" {
		usage()
		return exitUsage
	}
	floor, err := parseSeverity(*minSeverity)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return exitUsage
	}

	targets, err := loadTargets(*targetsPath, *insecure)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return exitError
	}

	history, err := loadHistory(*historyPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return exitError
	}

	// Ctrl-C must produce a partial report rather than nothing; a fleet poll can
	// take minutes and an operator who interrupts it still wants what was found.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	telemetry, err := collect.Fleet(ctx, targets, collect.Options{
		Concurrency:    *concurrency,
		PerHostTimeout: *timeout,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return exitError
	}

	history.append(telemetry)
	history.prune(time.Now(), *retain)
	full := history.analyze(telemetry)

	if *historyPath != "" {
		if err := history.save(*historyPath); err != nil {
			// Losing history degrades future trends but does not invalidate this
			// run's findings, so warn and carry on.
			fmt.Fprintln(os.Stderr, "warning: could not save history:", err)
		}
	}

	if *promPath != "" {
		// Deliberately the unfiltered report: -min-severity shapes what a human
		// reads top-down, and deciding what is worth waking someone for is
		// Prometheus's job once the numbers are in it.
		if err := report.WritePromFile(*promPath, full); err != nil {
			// Same judgement as the history save above: the poll happened and
			// the findings stand. The poll-timestamp metric exists precisely so
			// that a file which stopped being rewritten alerts on itself.
			fmt.Fprintln(os.Stderr, "warning: could not write metrics:", err)
		}
	}

	filtered := full
	filtered.Findings = nil
	for _, f := range full.Findings {
		if f.Severity >= floor {
			filtered.Findings = append(filtered.Findings, f)
		}
	}

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(filtered); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			return exitError
		}
	} else {
		printReport(filtered)
	}

	if len(filtered.Findings) > 0 {
		return exitFindings
	}
	return exitClean
}

func printReport(r analyze.Report) {
	fmt.Printf("rack-sentinel — %s\n", r.GeneratedAt.Format(time.RFC3339))
	fmt.Printf("  hosts polled  %d\n", r.HostsPolled)
	if r.HostsFailed > 0 {
		fmt.Printf("  hosts failed  %d\n", r.HostsFailed)
	}
	fmt.Println()

	if len(r.Findings) == 0 {
		fmt.Println("No degrading hardware found.")
		return
	}

	// Highest severity first, then by confidence — an operator reads top-down and
	// should hit the things that matter first.
	sorted := make([]analyze.Finding, len(r.Findings))
	copy(sorted, r.Findings)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].Severity != sorted[j].Severity {
			return sorted[i].Severity > sorted[j].Severity
		}
		return sorted[i].Confidence > sorted[j].Confidence
	})

	for _, f := range sorted {
		fmt.Printf("[%-6s] %s  %s\n", f.Severity, f.Host, f.Summary)
		for _, e := range f.Evidence {
			fmt.Printf("           %s\n", e)
		}
		fmt.Printf("           confidence %.0f%%\n\n", f.Confidence*100)
	}
}

func parseSeverity(s string) (analyze.Severity, error) {
	switch strings.ToLower(s) {
	case "info":
		return analyze.SeverityInfo, nil
	case "watch":
		return analyze.SeverityWatch, nil
	case "plan":
		return analyze.SeverityPlan, nil
	case "urgent":
		return analyze.SeverityUrgent, nil
	}
	return 0, fmt.Errorf("unknown severity %q (want info|watch|plan|urgent)", s)
}

// loadTargets reads a CSV of host,endpoint,username,password.
//
// Credentials in a file are not ideal, but they are what an operator already has
// from their provisioning system, and the alternative — flags — puts passwords
// in the process table where every user on the box can read them.
func loadTargets(path string, insecure bool) ([]collect.Target, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open targets: %w", err)
	}
	defer f.Close()

	r := csv.NewReader(f)
	r.FieldsPerRecord = -1
	r.TrimLeadingSpace = true

	rows, err := r.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("parse targets: %w", err)
	}

	var targets []collect.Target
	for i, row := range rows {
		if len(row) == 0 || strings.HasPrefix(strings.TrimSpace(row[0]), "#") {
			continue
		}
		if len(row) < 4 {
			return nil, fmt.Errorf("targets line %d: want host,endpoint,username,password", i+1)
		}
		targets = append(targets, collect.Target{
			Host:     row[0],
			Endpoint: row[1],
			Username: row[2],
			Password: row[3],
			Insecure: insecure,
		})
	}
	if len(targets) == 0 {
		return nil, collect.ErrNoTargets
	}
	return targets, nil
}

func usage() {
	fmt.Fprint(os.Stderr, `rack-sentinel — predictive hardware failure detection from BMC telemetry

Polls a fleet over Redfish and reports components that are DEGRADING, which is
not the same as components with high counters. A DIMM at 10,000 correctable
errors that has been flat for a year is ECC doing its job. One at 200 that
doubled this week is on its way to taking the node down.

usage:
  rack-sentinel -targets fleet.csv [-history state.json] [flags]

targets CSV (host,endpoint,username,password):
  node-01,https://10.0.0.11,admin,secret
  node-02,https://10.0.0.12,admin,secret

Trends need history. Run on a schedule with -history pointing at the same file;
a single run can only report counters, not whether they are getting worse.

-prom writes node_exporter textfile metrics, replacing the file in one rename so
a scrape mid-write cannot read half of it. It carries every finding regardless of
-min-severity, because what is worth paging for is Prometheus's decision:

  rack-sentinel -targets fleet.csv -history state.json \
    -prom /var/lib/node_exporter/textfile/rack-sentinel.prom

exit codes:
  0  polled, nothing at or above -min-severity
  1  findings reported
  2  bad usage
  3  could not run

flags:
`)
	flag.PrintDefaults()
}
