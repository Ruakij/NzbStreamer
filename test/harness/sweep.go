package harness

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// RunOptions controls a whole Run: the sweep matrix, the posting lifecycle,
// which phases each cell reads, how many times to repeat each cell, and which
// baseline tests to run.
type RunOptions struct {
	Matrix  Matrix
	Sets    string   // "all-at-once" (default) or "sequential"
	Phases  []string // readings per cell: "cold" and/or "warm"; cold drops the cache first
	Repeats int      // times to run each (test, cell); 0 = 1
	// Baselines are the environment tests to run (see IsBaseline), e.g.
	// baseline-news; they measure the rig rather than the app and write their
	// own output file.
	Baselines []string
	// Combine selects how the matrix sweeps: ""/"cartesian" = every cross
	// product (Matrix.Cells), "pairwise" = Matrix.CellsPairwise.
	Combine string
	// ProfileDir is the directory for per-reading pprof files; empty turns
	// profiling off. KeepProfiles keeps the raw *.pprof binaries after the
	// post-run analysis (otherwise only .top + summary.csv survive). PprofTop
	// is how many functions each summary row carries (<=0 = 5).
	ProfileDir   string
	KeepProfiles bool
	PprofTop     int
}

// phases is the readings each cell takes, cold then warm by default.
func (o RunOptions) phases() []string {
	if len(o.Phases) == 0 {
		return []string{"cold", "warm"}
	}
	return o.Phases
}

// repeats is how many times each (test, cell) runs, at least once.
func (o RunOptions) repeats() int { return max(o.Repeats, 1) }

// Run plays every selected test against the full matrix. Behavior differs by
// lifecycle: all-at-once posts every set to one server and sweeps each cell
// under it; sequential posts one fixture group at a time to a fresh server and
// drops the whole stack between groups so leaks never bleed across sets.
//
// Results are returned even on error, so an interrupted run still writes what
// it measured.
func (r *Runner) Run(ctx context.Context, tests []Test, opts RunOptions) ([]Result, error) {
	if err := r.EnsurePayloads(r.payloadDir()); err != nil {
		return nil, err
	}
	if err := r.EnsureEnvFile(); err != nil {
		return nil, err
	}

	// One client per run: long reads need no overall timeout.
	client := &http.Client{}
	cells := opts.Matrix.Cells()
	if opts.Combine == "pairwise" {
		cells = opts.Matrix.CellsPairwise()
	}

	if opts.ProfileDir != "" {
		if err := os.MkdirAll(opts.ProfileDir, 0o755); err != nil {
			return nil, fmt.Errorf("mkdir profile dir: %w", err)
		}
		r.prof = newProfiler(opts.ProfileDir, opts.KeepProfiles, opts.PprofTop, client, r.BaseURL)
	}

	var results []Result
	var err error
	if opts.Sets == "sequential" {
		results, err = r.runSequential(ctx, client, tests, cells, opts)
	} else {
		results, err = r.runAllAtOnce(ctx, client, tests, cells, opts)
	}
	// Post-run analysis once, whatever the lifecycle: the profiles are all on
	// disk by now, and go tool pprof must not run during the timed reads.
	_, _ = r.prof.analyze()
	// Clear the profiler so a later Run on the same Runner starts clean.
	r.prof = nil
	return results, err
}

// runAllAtOnce posts everything, brings the streamer up once, probes what
// works on this host, makes sure every selected path resolves, then sweeps the
// cells and finally runs any requested baseline tests.
func (r *Runner) runAllAtOnce(ctx context.Context, client *http.Client, tests []Test, cells []Cell, opts RunOptions) ([]Result, error) {
	if err := r.PostSets(ctx, nil); err != nil {
		return nil, fmt.Errorf("post sets: %w", err)
	}
	if err := r.Up(ctx, "streamer"); err != nil {
		return nil, fmt.Errorf("up streamer: %w", err)
	}
	fmt.Println("waiting for streamer health...")
	if err := r.StreamerHealthy(ctx); err != nil {
		return nil, err
	}
	if err := r.AddNzbs(ctx, nil); err != nil {
		return nil, err
	}
	for _, t := range tests {
		for _, p := range t.Paths() {
			if err := r.WaitPath(ctx, p, !t.ExpectErr()); err != nil {
				return nil, err
			}
		}
	}
	// Capability probe last before the sweep: its result gates cap-unsupported
	// notes and fuse-* skips for the cells that follow, and nothing between the
	// streamer coming up and here needs it. keepServing=false: the first cell
	// recreates the streamer anyway, which lands the cleared probe cap.
	r.Probe(ctx, cells, false).PrintCapabilities()
	results, err := r.runCells(ctx, client, tests, cells, opts)
	if err != nil {
		return results, err
	}
	if len(opts.Baselines) > 0 {
		bls, err := r.runBaselines(ctx, cells, opts)
		results = append(results, bls...)
		if err != nil {
			return results, err
		}
	}
	return results, nil
}

// runSequential posts and tears down one fixture group at a time. Grouping by
// fixture keeps tests that share a set (e.g. the webdav-rar-* family would live
// together) served by the same posting.
//
// The capability probe and baseline tests are all-at-once concerns: the stack
// is only guaranteed up in one place, so a sequential run refuses them.
func (r *Runner) runSequential(ctx context.Context, client *http.Client, tests []Test, cells []Cell, opts RunOptions) ([]Result, error) {
	if len(opts.Baselines) > 0 {
		return nil, fmt.Errorf("baseline tests require -sets all-at-once")
	}
	for _, t := range tests {
		if t.Concurrent() {
			return nil, fmt.Errorf("concurrent test %s requires -sets all-at-once", t.Name())
		}
	}
	var results []Result

	groups := map[string][]Test{}
	var order []string
	for _, t := range tests {
		if _, seen := groups[t.Fixture()]; !seen {
			order = append(order, t.Fixture())
		}
		groups[t.Fixture()] = append(groups[t.Fixture()], t)
	}

	for _, fix := range order {
		group := groups[fix]
		if err := r.PostSets(ctx, []string{fix}); err != nil {
			return results, fmt.Errorf("post set %s: %w", fix, err)
		}
		if err := r.Up(ctx, "streamer"); err != nil {
			return results, err
		}
		if err := r.StreamerHealthy(ctx); err != nil {
			return results, err
		}
		if err := r.AddNzbs(ctx, []string{fix}); err != nil {
			return results, err
		}
		for _, t := range group {
			if err := r.WaitPath(ctx, t.Path(), !t.ExpectErr()); err != nil {
				return results, err
			}
		}
		gr, err := r.runCells(ctx, client, group, cells, opts)
		results = append(results, gr...)
		if err != nil {
			return results, err
		}
		// full teardown: the next group starts from nothing again
		if err := r.DropFixtureVolumes(ctx); err != nil {
			return results, err
		}
	}
	return results, nil
}

// runCells sweeps every cell for every test, repeating each opts.Repeats times,
// reporting one progress line per (test, cell) with a running ETA. The total
// counts every reading: tests * cells * repeats * phases.
func (r *Runner) runCells(ctx context.Context, client *http.Client, tests []Test, cells []Cell, opts RunOptions) ([]Result, error) {
	total := len(tests) * len(cells) * opts.repeats() * len(opts.phases())
	var results []Result
	done, start := 0, time.Now()
	for _, t := range tests {
		for _, cell := range cells {
			res, err := r.runCell(ctx, client, t, cell, opts)
			results = append(results, res...)
			if err != nil {
				return results, err
			}
			done += len(res)
			fmt.Printf("[%3d/%3d] %-16s %-28s %s MiB/s  %s%s\n",
				done, total, t.Name(), opts.Matrix.Row(cell), phaseSpeeds(res),
				time.Since(start).Round(time.Second), eta(start, done, total))
		}
	}
	return results, nil
}

// phaseSpeeds renders the mean speed of each phase of one cell. Both columns
// are always present, with "-" for a phase the run did not request, so the
// progress lines stay a table.
func phaseSpeeds(res []Result) string {
	speeds := map[string][]float64{}
	for _, x := range res {
		if x.OK {
			speeds[x.Phase] = append(speeds[x.Phase], x.MiBs)
		}
	}
	var b strings.Builder
	for _, p := range []string{"cold", "warm"} {
		if _, ok := speeds[p]; ok {
			fmt.Fprintf(&b, " %s=%6.1f", p, meanOf(speeds[p]))
		} else {
			fmt.Fprintf(&b, " %s=     -", p)
		}
	}
	return strings.TrimSpace(b.String())
}

// eta extrapolates the remaining time from what the run has managed so far.
func eta(start time.Time, done, total int) string {
	if done <= 0 {
		return ""
	}
	per := time.Duration(float64(time.Since(start)) / float64(done))
	return fmt.Sprintf(" eta %s", (per * time.Duration(total-done)).Round(time.Second))
}

// runCell imposes one cell and runs it opts.Repeats times: apply the cell's
// envelope and env, restart the streamer (cold or warm per phase), and take a
// reading for each requested phase. Each repetition after the first starts from
// a fresh cold start (fresh cache) when the cold phase is requested, then reads
// warm against it.
func (r *Runner) runCell(ctx context.Context, client *http.Client, t Test, cell Cell, opts RunOptions) ([]Result, error) {
	phases := opts.phases()
	hasCold := slices.Contains(phases, "cold")
	if err := r.applyCellEnv(ctx, cell); err != nil {
		return nil, err
	}
	// The sweep reaches the container through the env file, so it must be
	// rewritten before the streamer restarts.
	if err := r.writeCellEnv(cell); err != nil {
		return nil, err
	}

	var out []Result
	// The profile filename identifies one reading; the cell row string is the
	// stable, Matrix-derived identity for it.
	cellpath := opts.Matrix.Row(cell)
	for rep := 0; rep < opts.repeats(); rep++ {
		switch {
		case hasCold && rep == 0:
			// First repetition: a full recreate, so the cell's env and throttle
			// override land and the cache volume is fresh.
			if err := r.ColdStart(ctx); err != nil {
				return out, err
			}
		case hasCold:
			// Later repetitions: the container config cannot change inside a
			// cell, so an emptied cache plus a restarted process is the same
			// fresh-cache start without a recreate.
			if err := r.FastColdStart(ctx); err != nil {
				return out, err
			}
		case rep == 0:
			if err := r.WarmRestart(ctx); err != nil {
				return out, err
			}
		default:
			// Warm-only repetition: env and cache unchanged, nothing to restart.
		}

		// A restart restores metadata asynchronously; wait for this file to be
		// addressable before timing a read that would otherwise 404.
		if err := r.WaitPath(ctx, t.Path(), !t.ExpectErr()); err != nil {
			return out, err
		}

		// Cold is the first touch on a fresh cache; warm reads the same file
		// again immediately, cache primed, no restart in between.
		if hasCold {
			out = append(out, r.readOnce(ctx, client, t, cell, cellpath, "cold", rep))
		}
		if slices.Contains(phases, "warm") {
			out = append(out, r.readOnce(ctx, client, t, cell, cellpath, "warm", rep))
		}
	}
	return out, nil
}

// applyCellEnv imposes the cell's network envelope and cache-device throttle on
// the rig. A mechanism the probe says this host ignores is skipped - the cell
// still runs unthrottled, and capPrefix flags it on every result. The throttle
// override is always (re)written, so a 0/0 cell clears a stale cap.
func (r *Runner) applyCellEnv(ctx context.Context, cell Cell) error {
	if r.netemUsable() {
		if err := r.ApplyNetemFull(ctx, cell.LatencyMs, cell.JitterMs, cell.LineSpeed, cell.LineJitter); err != nil {
			return err
		}
	}
	if r.deviceBpsUsable() {
		if err := r.ApplyCacheThrottle(ctx, cell.CacheWriteSpeed, cell.CacheReadSpeed); err != nil {
			return err
		}
	}
	return nil
}

// writeCellEnv rewrites the env file with a cell's sweep settings, so the next
// streamer restart actually runs with them.
func (r *Runner) writeCellEnv(cell Cell) error { return r.writeEnv(cell.Env()) }

// writeEnv rewrites the env file, which is both compose's interpolation source
// and the streamer service's env_file. Keys are written in sorted order so a
// diff of two cells is readable.
func (r *Runner) writeEnv(env map[string]string) error {
	if err := os.MkdirAll(filepath.Dir(r.EnvFile), 0o755); err != nil {
		return fmt.Errorf("mkdir env file dir: %w", err)
	}
	var b strings.Builder
	for _, k := range slices.Sorted(maps.Keys(env)) {
		fmt.Fprintf(&b, "%s=%s\n", k, env[k])
	}
	if err := os.WriteFile(r.EnvFile, []byte(b.String()), 0o644); err != nil {
		return fmt.Errorf("write env file: %w", err)
	}
	return nil
}

// EnsureEnvFile writes the rig defaults if no env file is there yet. The
// streamer service declares it as env_file, so compose refuses to parse the
// stack at all while it is missing - including for a plain build.
func (r *Runner) EnsureEnvFile() error {
	if _, err := os.Stat(r.EnvFile); err == nil {
		return nil
	}
	return r.writeEnv(RigDefaults)
}

// payloadDir is where the generated source files live: the host side of the
// bind mount the archive builder reads and the digest check compares against.
func (r *Runner) payloadDir() string {
	return filepath.Join(filepath.Dir(r.ComposeFile), "build", "payload")
}
