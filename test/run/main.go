// Command run is the e2e benchmark harness. Flags drive it end to end so a CI
// workflow_dispatch can sweep the matrix without prompts.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"git.ruekov.eu/ruakij/nzbStreamer/test/harness"
)

// composeFiles resolves test/compose.yaml whether the binary runs from the repo
// root or from test/.
func composeFiles() string {
	if _, err := os.Stat(filepath.Join("test", "compose.yaml")); err == nil {
		return "test/compose.yaml"
	}
	return "compose.yaml"
}

// storageOverride resolves the storage-flavor override compose adds next to
// the base file: disk (default) or ram when -ram swaps memory for the news
// spool and streamer cache.
func storageOverride(ram bool) string {
	flavor := "disk"
	if ram {
		flavor = "ram"
	}
	dir := filepath.Dir(composeFiles())
	return filepath.Join(dir, "compose."+flavor+".yaml")
}

func main() {
	var (
		testsFlag     = flag.String("tests", "", "comma list of read-test names, '' = all enabled read tests")
		matrixFlag    = flag.String("matrix", "", "matrix spec (rig knobs only: readtype, latency, jitter, linespeed, linejitter, cachewritespeed, cachereadspeed, seed), '' = harness defaults")
		appEnvFlag    = flag.String("app-env", "", "app env var sweep axes, 'NAME=v1,v2' space separated; '' = app config left to the product")
		setsFlag      = flag.String("sets", "all-at-once", "all-at-once | sequential")
		sizeMB        = flag.Int("size-mb", 0, "payload size in MiB, 0 = default")
		outFlag       = flag.String("out", "", "raw CSV path, default test/build/results.csv")
		summaryFlag   = flag.String("summary", "", "summary CSV path, default test/build/summary.csv; 'disable' writes only the raw readings")
		repeats       = flag.Int("repeats", 3, "times to run each (test, cell); summary stats average over them")
		probeOnly     = flag.Bool("probe-only", false, "only print what this host can do, then exit")
		phasesFlag    = flag.String("phases", "cold,warm", "read phases per cell: cold and/or warm; '' = cold,warm")
		baselinesFlag = flag.String("baselines", "news,cache-write,cache-read", "baseline tests to run (news, cache-write, cache-read); '' = the default set, 'none' = no baselines")
		baselinesOut  = flag.String("baselines-out", "", "baseline CSV path, default test/build/baselines.csv")
		baselinesSumm = flag.String("baselines-summary", "", "baseline summary CSV path, default test/build/baselines-summary.csv")
		keep          = flag.Bool("keep", false, "leave the stack up (skip down -v)")
		doBuild       = flag.Bool("build", false, "docker compose build before running")
		ram           = flag.Bool("ram", true, "memory-backed rig: news spool and streamer cache in tmpfs, several GiB of host RAM (cache alone is CACHE_MAX_SIZE, 2 GiB by default); -ram=false for disk-backed named volumes")
		fuse          = flag.Bool("fuse", true, "grant the streamer /dev/fuse + SYS_ADMIN and mount /app/mnt; -fuse=false skips the rig and the fuse read tests with it")
		combine       = flag.String("combine", "cartesian", "matrix combination: cartesian | pairwise (every 2-wise tuple covered)")
		pprofDir      = flag.String("pprof-folder", filepath.Join("test", "build", "profiles"), "collect a CPU+heap pprof per reading into this dir, '' = off")
		keepProf      = flag.Bool("pprof-keep", false, "keep the raw .pprof binaries after the post-run analysis")
		pprofTop      = flag.Int("pprof-top", 5, "functions per profile summary row (go tool pprof -top -nodecount)")
	)
	flag.Parse()

	if *sizeMB > 0 {
		// Regenerate the payloads at this size; payload.Size() reads it back.
		os.Setenv("SIZE_MB", strconv.Itoa(*sizeMB))
	}

	// Baselines measure the rig where their names point (news server, cache
	// device), never via -tests. The CLI accepts the short names (news,
	// cache-write, cache-read); an empty flag selects the default set and "none"
	// selects nothing, as does a sequential lifecycle, which cannot run them.
	baselineNames := map[string]string{
		"news":        harness.BaselineNews,
		"cache-write": harness.BaselineCacheWrite,
		"cache-read":  harness.BaselineCacheRead,
	}
	baselines := split(*baselinesFlag)
	switch {
	case len(baselines) == 0:
		baselines = []string{harness.BaselineNews, harness.BaselineCacheWrite, harness.BaselineCacheRead}
	case len(baselines) == 1 && baselines[0] == "none":
		baselines = nil
	default:
		for i, b := range baselines {
			if full, ok := baselineNames[b]; ok {
				baselines[i] = full
			}
		}
	}
	phases := split(*phasesFlag)
	if len(phases) == 0 {
		phases = []string{"cold", "warm"}
	}
	if *setsFlag == "sequential" {
		baselines = nil
	}
	var tests []harness.Test
	// Default selection (no -tests) is what runs on the current rig: fuse tests
	// need the FUSE mount, so a -fuse=false rig leaves them out, while an
	// explicit -tests always binds what it names.
	tests = harness.Select(split(*testsFlag))
	if *testsFlag == "" && !*fuse {
		var plain []harness.Test
		for _, t := range tests {
			if !t.Fuse() {
				plain = append(plain, t)
			}
		}
		tests = plain
	}

	m := harness.Matrix{}
	if *matrixFlag != "" {
		var err error
		m, err = harness.ParseMatrix(*matrixFlag)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
	}
	if *appEnvFlag != "" {
		app, err := harness.ParseAppEnv(*appEnvFlag)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		m.App = append(m.App, app...)
	}

	runner := harness.NewRunner(composeFiles(), filepath.Join("test", "build", "run.env"))
	runner.Override = []string{storageOverride(*ram)}
	if *fuse {
		dir := filepath.Dir(composeFiles())
		runner.Override = append(runner.Override, filepath.Join(dir, "compose.fuse.yaml"))
	}
	runner.Ram = *ram

	// An interrupt cancels the run rather than killing it: Run returns what it
	// has measured so far and the CSVs below are still written.
	// Not stopped again: every path out of here ends the process, which
	// releases the handler anyway.
	ctx, _ := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	// compose parses the streamer's env_file for every command, build included.
	if err := runner.EnsureEnvFile(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if *doBuild {
		if err := runner.Compose(ctx, "build"); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}

	if *probeOnly {
		// The probe needs news up (netem) and the streamer up (device-bps,
		// fuse); the read tests are irrelevant to it.
		if err := runner.PostSets(ctx, nil); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		if err := runner.Up(ctx, "streamer"); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		if err := runner.StreamerHealthy(ctx); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		runner.Probe(ctx).PrintCapabilities()
		if !*keep {
			if err := runner.DropFixtureVolumes(ctx); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
		}
		return
	}

	results, err := runner.Run(ctx, tests, harness.RunOptions{
		Matrix:       m,
		Sets:         *setsFlag,
		Phases:       phases,
		Repeats:      *repeats,
		Baselines:    baselines,
		Combine:      *combine,
		ProfileDir:   *pprofDir,
		KeepProfiles: *keepProf,
		PprofTop:     *pprofTop,
	})
	// A failed or interrupted run still wrote readings; report the error after
	// they are on disk.
	if err != nil {
		fmt.Fprintf(os.Stderr, "run: %v\n", err)
	}

	// Baseline rows measure the rig, not the app, so they never dilute the read
	// results file (or its summary): each family gets its own pair of outputs.
	var reads, bls []harness.Result
	for _, r := range results {
		if harness.IsBaseline(r.Test) {
			bls = append(bls, r)
		} else {
			reads = append(reads, r)
		}
	}

	outPath := *outFlag
	if outPath == "" {
		outPath = filepath.Join("test", "build", "results.csv")
	}
	sumPath := *summaryFlag
	if sumPath == "" {
		sumPath = filepath.Join("test", "build", "summary.csv")
	}
	// The read summary joins each group against the baselines measured at the
	// same rig settings, which is what its verdict column compares to.
	if err := writeCSVFiles(outPath, sumPath, m, reads, bls); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if len(bls) > 0 {
		bOutPath := *baselinesOut
		if bOutPath == "" {
			bOutPath = filepath.Join("test", "build", "baselines.csv")
		}
		bSumPath := *baselinesSumm
		if bSumPath == "" {
			bSumPath = filepath.Join("test", "build", "baselines-summary.csv")
		}
		if err := writeCSVFiles(bOutPath, bSumPath, m, bls, nil); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}

	// Compact human table on stdout, one line per reading.
	fmt.Printf("%-16s %-4s %-6s %-30s %9s %9s %7s %7s %7s %9s %5s  %s\n",
		"test", "rep", "phase", fmt.Sprintf("cell(%s)", m.Header()), "MiB/s", "bytesMi", "wallms", "ttfbms", "cpums", "cpuwaitms", "ok", "note")
	for _, r := range results {
		fmt.Printf("%-16s %-4d %-6s %-30s %9.1f %9.0f %7d %7d %7d %9d %5v  %s\n",
			r.Test, r.Rep, r.Phase, m.Row(r.Cell), r.MiBs, float64(r.Bytes)/(1<<20), r.Wall.Milliseconds(),
			r.TTFB.Milliseconds(), r.CPU.Milliseconds(), r.CPUWait.Milliseconds(), r.OK, r.Note)
	}

	if !*keep {
		// The run's own context may already be cancelled by the interrupt that
		// ended it; teardown gets a live one of its own.
		if derr := runner.DropFixtureVolumes(context.Background()); derr != nil {
			fmt.Fprintln(os.Stderr, derr)
			os.Exit(1)
		}
	}
	if err != nil {
		os.Exit(1)
	}
}

// writeCSVFiles writes the raw readings CSV and, unless summaryPath is
// "disable", the per-group summary CSV next to it, judged against baselines.
func writeCSVFiles(outPath, summaryPath string, m harness.Matrix, results, baselines []harness.Result) error {
	if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
		return err
	}
	f, err := os.Create(outPath)
	if err != nil {
		return fmt.Errorf("create %s: %w", outPath, err)
	}
	err = harness.WriteCSV(f, m, results)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("write %s: %w", outPath, err)
	}
	fmt.Printf("wrote %d readings to %s\n", len(results), outPath)
	if summaryPath == "disable" {
		return nil
	}
	sf, err := os.Create(summaryPath)
	if err != nil {
		return fmt.Errorf("create %s: %w", summaryPath, err)
	}
	err = harness.WriteSummaryCSV(sf, m, results, baselines)
	if cerr := sf.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("write %s: %w", summaryPath, err)
	}
	fmt.Printf("wrote summary to %s\n", summaryPath)
	return nil
}

func split(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	return strings.Split(s, ",")
}
