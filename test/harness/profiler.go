package harness

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// defaultProfileSeconds is the width of the CPU sampling window asked for,
// roughly a typical single read. The window is cut short by the early
// disconnect in cpuProfile.stop, so it tracks a shorter read too. ponytail: a
// per-cell estimate (bytes / expected throughput) could replace this fixed
// value if short reads want more samples - add when the overhead for long reads
// proves too coarse.
const defaultProfileSeconds = 5

// stopWait bounds how long cpuProfile.stop() waits for the profile handler to
// wind down after the early disconnect, so a wedged /debug/pprof request can
// never stall the measured read past the read itself plus this margin.
const stopWait = 3 * time.Second

// Profiler collects a raw CPU and heap pprof per reading into a directory for
// later analysis. It is the "collect raw data during the run, analyze after"
// half of the harness: reads record their profiles here without the analysis
// slowing the timing path, and AnalyzeProfiles does the go tool pprof pass at
// the end.
type Profiler struct {
	dir     string
	keep    bool // false: delete raw *.pprof after analysis (keep .top + summary.csv)
	topN    int  // functions per summary row & -top -nodecount
	client  *http.Client
	baseURL string
}

// newProfiler builds a Profiler that will write into dir (created by Run).
// client is the run's single http.Client — CPU/heap profile GETs go through
// the same connection pool, but never inside the instrumented Wall/TTFB
// window, so they are not counted in either.
func newProfiler(dir string, keep bool, topN int, client *http.Client, baseURL string) *Profiler {
	return &Profiler{dir: dir, keep: keep, topN: topN, client: client, baseURL: baseURL}
}

// tagName turns dirty sweep/file identifiers into a single filename-safe tag
// "<test>.<cellpath>.<phase>.<rep>".
func profileTag(test, cellpath, phase string, rep int) string {
	san := func(s string) string {
		var b []byte
		for i := 0; i < len(s); i++ {
			c := s[i]
			if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') ||
				c == '.' || c == '_' || c == '-' || c == ',' {
				b = append(b, c)
			} else {
				b = append(b, '_')
			}
		}
		return string(b)
	}
	return san(test) + "." + san(cellpath) + "." + san(phase) + "." + strconv.Itoa(rep)
}

// cpuProfile is one in-flight CPU profile. The GET runs in a goroutine so it
// never blocks the measured read; stop() disconnects the body once the read is
// done, which makes net/http/pprof wind down early and the handler returns the
// partial profile for exactly the elapsed window.
type cpuProfile struct {
	p     *Profiler
	tag   string // filename base (no extension)
	ready chan struct{}
	done  chan struct{}
	resp  *http.Response
}

// startCPU begins sampling /debug/pprof/profile for the expected window. The
// request is issued in a background goroutine, so it starts alongside the read
// (right after the "before" snapshot) without blocking it.
func (p *Profiler) startCPU(ctx context.Context, tag string) *cpuProfile {
	if p == nil {
		return nil
	}
	url := p.baseURL + fmt.Sprintf("/debug/pprof/profile?seconds=%d", defaultProfileSeconds)
	cp := &cpuProfile{p: p, tag: tag, ready: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(cp.done)
		resp, err := p.client.Get(url)
		if err != nil || resp.StatusCode != http.StatusOK {
			if resp != nil {
				resp.Body.Close()
			}
			close(cp.ready)
			return
		}
		cp.resp = resp
		close(cp.ready)
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err == nil && len(body) > 0 {
			p.write(tag+".cpu.pprof", body)
		}
	}()
	return cp
}

// stop ends the sampling window: it disconnects the response body (pprof
// honors the early disconnect and returns whatever it samples up to now), then
// waits for the goroutine to finish writing the file.
func (cp *cpuProfile) stop() {
	if cp == nil {
		return
	}
	select {
	case <-cp.ready:
		cp.resp.Body.Close() // force pprof to stop sampling and return
	case <-cp.done:
	case <-time.After(stopWait):
	}
	select {
	case <-cp.done:
	case <-time.After(stopWait):
	}
}

// heapProfile fetches the RUNNING process heap snapshot (outside the measured
// window) and writes it to dir.
func (p *Profiler) heapProfile(ctx context.Context, tag string) {
	if p == nil {
		return
	}
	url := p.baseURL + "/debug/pprof/heap"
	resp, err := p.client.Get(url)
	if err != nil || resp.StatusCode != http.StatusOK {
		if resp != nil {
			resp.Body.Close()
		}
		return
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err == nil && len(body) > 0 {
		p.write(tag+".heap.pprof", body)
	}
}

// write saves one raw pprof file best-effort.
func (p *Profiler) write(name string, body []byte) {
	_ = os.WriteFile(filepath.Join(p.dir, name), body, 0o644)
}

// startProf is the Runner-facing glue: it builds the tag from the test and the
// cell row string and starts the CPU profile, or returns nil when profiling is
// off.
func (r *Runner) startProf(ctx context.Context, t Test, cellpath string, phase string, rep int) *cpuProfile {
	if r.prof == nil {
		return nil
	}
	return r.prof.startCPU(ctx, profileTag(t.Name(), cellpath, phase, rep))
}

// stopProf stops a CPU profile and, when profiling is on, fetches the heap
// snapshot for the same reading.
func (r *Runner) stopProf(ctx context.Context, t Test, cellpath string, phase string, rep int, cp *cpuProfile) {
	if cp == nil {
		return
	}
	cp.stop()
	if r.prof != nil {
		r.prof.heapProfile(ctx, profileTag(t.Name(), cellpath, phase, rep))
	}
}

// topLine parses one go tool pprof -top data row - "flat flat% sum% cum cum%
// name" - into the function name and its flat percent, and reports false for
// anything else (the header, the "Showing nodes..." preamble).
func topLine(ln string) (name, flatPct string, ok bool) {
	f := strings.Fields(ln)
	if len(f) != 6 || !strings.HasSuffix(f[1], "%") {
		return "", "", false
	}
	return f[5], strings.TrimSuffix(f[1], "%"), true
}

// analyze runs go tool pprof -top over every *.cpu.pprof, writes the raw text
// beside it as <base>.top, appends one summary.csv line per profile (tag plus
// the top-N functions as "fn 12.3%"), and, when keep is false, deletes the raw
// binaries. It prints a one-line result and a per-profile warning if the
// toolchain is unavailable, never failing the bench.
func (p *Profiler) analyze() (int, error) {
	if p == nil {
		return 0, nil
	}
	files, _ := filepath.Glob(filepath.Join(p.dir, "*.cpu.pprof"))
	topN := p.topN
	if topN <= 0 {
		topN = 5
	}
	var summary []string
	n := 0
	for _, f := range files {
		n++
		base := strings.TrimSuffix(f, ".cpu.pprof")
		cmd := exec.Command("go", "tool", "pprof", "-top", "-nodecount="+strconv.Itoa(topN), f)
		out, err := cmd.Output()
		tag := filepath.Base(base)
		if err != nil {
			fmt.Printf("profiler: warning: go tool pprof %s: %v (skipping; raw kept)\n", tag, err)
			continue
		}
		_ = os.WriteFile(base+".top", out, 0o644)
		summary = append(summary, topRow(tag, string(out), topN))
		if !p.keep {
			_ = os.Remove(f)
		}
	}
	if len(summary) > 0 {
		_ = os.WriteFile(filepath.Join(p.dir, "summary.csv"), []byte(strings.Join(summary, "\n")+"\n"), 0o644)
	}
	if p.keep {
		fmt.Printf("profiles: %d analyzed into %s (raw kept)\n", n, p.dir)
	} else {
		fmt.Printf("profiles: %d analyzed into %s (raw deleted, .top + summary.csv kept)\n", n, p.dir)
	}
	return n, nil
}

// topRow turns go tool pprof -top output into one summary.csv row: the tag
// followed by the top topN functions as "<fn> <pct>%", comma-joined.
func topRow(tag, topText string, topN int) string {
	var parts []string
	parts = append(parts, tag)
	for _, ln := range strings.Split(topText, "\n") {
		name, flat, ok := topLine(ln)
		if !ok {
			continue
		}
		parts = append(parts, fmt.Sprintf("%s %s%%", name, flat))
		if len(parts)-1 >= topN { // parts[0] is the tag
			break
		}
	}
	return strings.Join(parts, ",")
}
