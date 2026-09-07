package harness

import (
	"encoding/csv"
	"io"
	"math"
	"slices"
	"strconv"
	"time"
)

// Result is one measured read of one test under one cell, in one phase, for one
// repetition.
type Result struct {
	Test  string
	Rep   int // repetition index, 0-based (see RunOptions.Repeats)
	Cell  Cell
	Phase string // "cold" or "warm", or "measure" for baseline tests
	// Outcome fields. Wall/TTFB are the read's own instrumented timings; CPU and
	// the memory figures are snapshots of the streamer process (PID 1) taken
	// around the read, which never touch the measured HTTP path.
	Bytes int64
	Wall  time.Duration // whole read wall clock
	TTFB  time.Duration // time to first body byte (webdav reads; 0 for fuse)
	// TTFBP50/TTFBP95 are the per-request p50/p95 of the TTFB distribution
	// across a read's ranged requests (a single-request read equals TTFB; 0
	// for fuse reads that report no ttfb).
	TTFBP50, TTFBP95 time.Duration
	// LatencyP50/P95 are the per-request full call->reply round-trip latencies.
	LatencyP50, LatencyP95 time.Duration
	CPU                    time.Duration // streamer process CPU consumed during the read
	// CPUWait is the VM iowait attributable to the read's window: the delta of
	// the aggregate /proc/stat "cpu " iowait field (all cores, USER_HZ) between
	// the same before/after snapshots that CPU uses. 0 when /proc/stat is
	// unreadable in the container.
	CPUWait time.Duration
	// NewsCPU/NewsMemBytes/NewsPeakBytes mirror the streamer fields above but
	// for the news server container: the rig's own resource use is as much a
	// signal as the app's on reads that push it.
	NewsCPU       time.Duration
	NewsMemBytes  int64
	NewsPeakBytes int64
	MemBytes      int64 // streamer VmRSS at the end of the read
	PeakBytes     int64 // streamer VmHWM, peak since process start
	// MiBs is Bytes over Wall for every test, so a fuse row and a webdav row
	// mean the same thing in that column. DDMiBs is what dd reported for itself
	// inside the container on a fuse read: the same transfer without the docker
	// exec around it, and 0 everywhere else.
	MiBs   float64
	DDMiBs float64
	OK     bool
	Note   string
}

// mibs is bytes over a duration, 0 for a zero-length window.
func mibs(bytes int64, d time.Duration) float64 {
	if d <= 0 {
		return 0
	}
	return float64(bytes) / (1 << 20) / d.Seconds()
}

// resultHeader are the outcome columns both CSVs carry, in order. The names
// carry their unit, so the columns are self-describing.
var resultHeader = []string{
	"out_mibs",
	"out_dd_mibs",
	"out_bytes",
	"out_wall_ms",
	"out_ttfb_ms",
	"out_ttfb_p50_ms",
	"out_ttfb_p95_ms",
	"out_latency_p50_ms",
	"out_latency_p95_ms",
	"out_cpu_ms",
	"out_cpuwait_ms",
	"out_rss_mib",
	"out_peak_rss_mib",
	"out_news_cpu_ms",
	"out_news_rss_mib",
	"out_news_peak_rss_mib",
	"ok",
	"note",
}

// WriteCSV writes one row per reading: identity and inputs (test/phase/rep and
// the cell axes it ran under) on the left, outcome fields on the right.
func WriteCSV(w io.Writer, m Matrix, results []Result) error {
	cw := csv.NewWriter(w)
	hdr := append([]string{"test", "rep", "phase"}, m.HeaderFields()...)
	_ = cw.Write(append(hdr, resultHeader...))
	for _, r := range results {
		row := append([]string{r.Test, strconv.Itoa(r.Rep), r.Phase}, m.RowFields(r.Cell)...)
		_ = cw.Write(append(row,
			f2(r.MiBs),
			f2(r.DDMiBs),
			strconv.FormatInt(r.Bytes, 10),
			ms(r.Wall),
			ms(r.TTFB),
			ms(r.TTFBP50),
			ms(r.TTFBP95),
			ms(r.LatencyP50),
			ms(r.LatencyP95),
			ms(r.CPU),
			ms(r.CPUWait),
			strconv.FormatInt(r.MemBytes>>20, 10),
			strconv.FormatInt(r.PeakBytes>>20, 10),
			ms(r.NewsCPU),
			strconv.FormatInt(r.NewsMemBytes>>20, 10),
			strconv.FormatInt(r.NewsPeakBytes>>20, 10),
			strconv.FormatBool(r.OK),
			r.Note,
		))
	}
	cw.Flush()
	return cw.Error()
}

// group collects the ok readings of one (test, phase, cell). readings counts
// every one of them; the sample slices hold only what each statistic is
// meaningful over.
type group struct {
	test, phase               string
	cell                      Cell
	readings, failures        int
	mibs, wall, ttfb, cpuwait []float64 // ms for the three latencies
	// what the reading cost the streamer: cpu ms, cpu ms per MiB moved, and
	// the RSS and peak RSS it was at when it finished
	cpu, cpuPerMiB, rss, peak []float64
	// the same for the news server, which shows what the load costs it
	newsCPU, newsRSS []float64
}

// groupBy buckets readings by (test, phase, cell) in first-seen order. A
// reading that failed only raises the group's failure count, so a cell nothing
// succeeded in still has a row saying so rather than vanishing.
//
// A reading that moved no bytes is counted but kept out of the speed and
// latency samples: the damaged fixtures pass by failing to read, and their
// 0 MiB/s is the absence of a measurement rather than a slow one. Averaging it
// in would drag a group's mean toward zero and put its p5 there outright.
func groupBy(m Matrix, results []Result) []*group {
	type key struct{ test, phase, cell string }
	index := map[key]*group{}
	var order []*group
	for _, r := range results {
		k := key{r.Test, r.Phase, m.Row(r.Cell)}
		g, seen := index[k]
		if !seen {
			g = &group{test: r.Test, phase: r.Phase, cell: r.Cell}
			index[k] = g
			order = append(order, g)
		}
		if !r.OK {
			g.failures++
			continue
		}
		g.readings++
		if r.Bytes <= 0 {
			continue
		}
		g.mibs = append(g.mibs, r.MiBs)
		g.wall = append(g.wall, float64(r.Wall.Milliseconds()))
		g.ttfb = append(g.ttfb, float64(r.TTFB.Milliseconds()))
		g.cpuwait = append(g.cpuwait, float64(r.CPUWait.Milliseconds()))
		cpu := float64(r.CPU.Milliseconds())
		g.cpu = append(g.cpu, cpu)
		g.cpuPerMiB = append(g.cpuPerMiB, cpu/(float64(r.Bytes)/(1<<20)))
		g.rss = append(g.rss, float64(r.MemBytes)/(1<<20))
		g.peak = append(g.peak, float64(r.PeakBytes)/(1<<20))
		g.newsCPU = append(g.newsCPU, float64(r.NewsCPU.Milliseconds()))
		g.newsRSS = append(g.newsRSS, float64(r.NewsMemBytes)/(1<<20))
	}
	return order
}

// serverBoundRatio is the share of the news baseline at or above which a read
// is sitting at what the rig can deliver rather than at what the app can.
const serverBoundRatio = 0.8

// WriteSummaryCSV writes one row per (test, phase, cell) group with the
// statistics the repeats feature is for. Because these are samples of repeated
// sweeps, the standard error of the mean and a 95% confidence interval for the
// true mean (mean +- t*SEM, Student's t with n-1 degrees of freedom) are derived
// from the same data, and p5/p50/p95 summarise the spread of the speed and of
// both latencies (whole-read wall and time to first byte; fuse reads have no
// ttfb, so their ttfb percentiles are 0). count is every reading of the group,
// failures how many of them failed, and measured how many moved bytes and so
// carry the statistics - the three differ on the damaged fixtures, which pass
// by reading nothing.
//
// baselines are the news-baseline readings of the same run, or nil. Where one
// covers a group's cell, the group carries what the rig itself managed and the
// verdict that follows from the ratio: server-bound means the read is at the
// news server's own limit and the app has nothing left to give, streamer-bound
// that the headroom is in the app.
func WriteSummaryCSV(w io.Writer, m Matrix, results, baselines []Result) error {
	ceiling := newsCeilings(m, baselines)

	cw := csv.NewWriter(w)
	hdr := append([]string{"test", "phase"}, m.HeaderFields()...)
	_ = cw.Write(append(hdr,
		"count", "failures", "measured",
		"mean_mibs", "stddev_mibs", "sem_mibs", "ci95_pm", "min_mibs", "max_mibs",
		"p5_mibs", "p50_mibs", "p95_mibs",
		"mean_wall_ms", "p5_wall_ms", "p50_wall_ms", "p95_wall_ms",
		"mean_ttfb_ms", "p5_ttfb_ms", "p50_ttfb_ms", "p95_ttfb_ms",
		"mean_cpuwait_ms", "p5_cpuwait_ms", "p50_cpuwait_ms", "p95_cpuwait_ms",
		"mean_cpu_ms", "mean_cpu_ms_per_mib", "mean_rss_mib", "mean_peak_rss_mib",
		"mean_news_cpu_ms", "mean_news_rss_mib",
		"baseline_mibs", "baseline_ratio", "verdict",
	))
	for _, g := range groupBy(m, results) {
		n := len(g.mibs)
		mean := meanOf(g.mibs)
		stddev := stddevOf(g.mibs, mean)
		sem, ci := 0.0, 0.0
		if n > 1 {
			sem = stddev / math.Sqrt(float64(n))
			ci = tCrit95(n-1) * sem
		}
		sm := sortedFloat(g.mibs)

		base, ratio, verdict := 0.0, 0.0, "no-baseline"
		if b, ok := ceiling[newsProjection(g.cell)]; ok && b > 0 && n > 0 {
			base, ratio = b, mean/b
			verdict = "streamer-bound"
			if ratio >= serverBoundRatio {
				verdict = "server-bound"
			}
		}

		row := append([]string{g.test, g.phase}, m.RowFields(g.cell)...)
		row = append(row,
			strconv.Itoa(g.readings),
			strconv.Itoa(g.failures),
			strconv.Itoa(n),
			f2(mean), f2(stddev), f2(sem), f2(ci),
			f2(percentile(sm, 0)), f2(percentile(sm, 100)),
			f2(percentile(sm, 5)), f2(percentile(sm, 50)), f2(percentile(sm, 95)),
		)
		for _, xs := range [][]float64{g.wall, g.ttfb, g.cpuwait} {
			s := sortedFloat(xs)
			row = append(row, f1(meanOf(xs)),
				f1(percentile(s, 5)), f1(percentile(s, 50)), f1(percentile(s, 95)))
		}
		row = append(row, f1(meanOf(g.cpu)), f2(meanOf(g.cpuPerMiB)),
			f1(meanOf(g.rss)), f1(meanOf(g.peak)),
			f1(meanOf(g.newsCPU)), f1(meanOf(g.newsRSS)))
		_ = cw.Write(append(row, f2(base), f2(ratio), verdict))
	}
	cw.Flush()
	return cw.Error()
}

// newsCeilings is the mean baseline-news speed per news-link projection, which
// is what a read under the same link conditions is judged against.
func newsCeilings(m Matrix, baselines []Result) map[string]float64 {
	out := map[string]float64{}
	for _, g := range groupBy(m, baselines) {
		if g.test != BaselineNews || len(g.mibs) == 0 {
			continue
		}
		out[newsProjection(g.cell)] = meanOf(g.mibs)
	}
	return out
}

func f1(v float64) string { return strconv.FormatFloat(v, 'f', 1, 64) }
func f2(v float64) string { return strconv.FormatFloat(v, 'f', 2, 64) }

func ms(d time.Duration) string { return strconv.FormatInt(d.Milliseconds(), 10) }

// meanOf is the arithmetic mean of a sample, 0 for an empty one.
func meanOf(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	var s float64
	for _, x := range xs {
		s += x
	}
	return s / float64(len(xs))
}

// stddevOf is the sample standard deviation, 0 for fewer than two samples.
func stddevOf(xs []float64, mean float64) float64 {
	if len(xs) < 2 {
		return 0
	}
	var s float64
	for _, x := range xs {
		d := x - mean
		s += d * d
	}
	// guard against tiny negative residues of the float computation
	if v := s / float64(len(xs)-1); v > 0 {
		return math.Sqrt(v)
	}
	return 0
}

// sortedFloat returns a sorted copy of xs.
func sortedFloat(xs []float64) []float64 {
	out := slices.Clone(xs)
	slices.Sort(out)
	return out
}

// percentile returns the pth percentile (0..100) of an ascending slice,
// linearly interpolated between the two neighbouring ranks and clamped at the
// ends, and 0 for an empty one.
func percentile(sorted []float64, p float64) float64 {
	n := len(sorted)
	if n == 0 {
		return 0
	}
	if n == 1 {
		return sorted[0]
	}
	idx := min(max(p/100*float64(n-1), 0), float64(n-1))
	lo, hi := int(math.Floor(idx)), int(math.Ceil(idx))
	if lo == hi {
		return sorted[lo]
	}
	return sorted[lo] + (sorted[hi]-sorted[lo])*(idx-float64(lo))
}

// tCrit95 is the two-tailed 95% critical value of Student's t for the given
// degrees of freedom, table for df up to 30 and the normal approximation
// beyond. A full t CDF is not worth a stats dependency for a benchmark harness.
func tCrit95(df int) float64 {
	// [df] = t_{0.975,df}: df=1..30
	tab := [...]float64{
		12.706, 4.303, 3.182, 2.776, 2.571, 2.447, 2.365, 2.306, 2.262, 2.228,
		2.201, 2.179, 2.160, 2.145, 2.131, 2.120, 2.110, 2.101, 2.093, 2.086,
		2.080, 2.074, 2.069, 2.064, 2.060, 2.056, 2.052, 2.048, 2.045, 2.042,
	}
	if df >= 1 && df <= len(tab) {
		return tab[df-1]
	}
	return 1.960 // asymptotic z for df > 30
}
