package harness

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"sync"
	"time"
)

// segmentRe pulls the message-id out of each segment; the nzb wraps it in the
// angle brackets the NNTP protocol itself requires.
var segmentRe = regexp.MustCompile(`<segment[^>]*>([^<]+)</segment>`)

// Baseline tests measure the test rig rather than the app (see -baselines): a
// direct news fetch for the news link, dd to and from the cache device for its
// caps. Each runs against the axes it exercises, projected from the same cell
// sweep the read tests use, and each measurement is repeated -repeats times.
const (
	BaselineNews       = "baseline-news"
	BaselineCacheWrite = "baseline-cache-write"
	BaselineCacheRead  = "baseline-cache-read"
)

// IsBaseline reports whether name is a baseline test.
func IsBaseline(name string) bool {
	switch name {
	case BaselineNews, BaselineCacheWrite, BaselineCacheRead:
		return true
	}
	return false
}

// NewsBaselineOptions is one news baseline measurement: which server, which
// nzb's articles, and how hard to ask for them.
type NewsBaselineOptions struct {
	Host         string
	Port         int
	User, Pass   string
	NzbPath      string
	MaxConn      int // parallel sockets; <1 is one
	PipelineSize int // outstanding requests per socket; <1 is DefaultBaselinePipelineSize
	Limit        int // max articles to fetch, 0 = every one in the nzb
}

// BaselineResult is one news-server baseline measurement.
type BaselineResult struct {
	MiBs     float64
	Articles int64
	Bytes    int64
	Failures int
}

// NewsBaseline measures the raw news server article-download baseline: MaxConn
// pipelined NNTP sockets fetch the nzb's articles and discard them, each socket
// keeping PipelineSize requests in flight so latency is hidden rather than paid
// per article. Every socket is dialed, authenticated and warmed first, so the
// timer only sees steady state. A raw protocol read and a regex over the nzb,
// so it cannot share a decoder bug with the app.
func NewsBaseline(ctx context.Context, o NewsBaselineOptions) (BaselineResult, error) {
	var out BaselineResult
	data, err := os.ReadFile(o.NzbPath)
	if err != nil {
		return out, fmt.Errorf("read nzb: %w", err)
	}
	ids := collectIDs(string(data), o.Limit)
	if len(ids) == 0 {
		return out, fmt.Errorf("no message-ids found in %s", o.NzbPath)
	}
	conns, window := max(o.MaxConn, 1), o.PipelineSize
	if window < 1 {
		window = DefaultBaselinePipelineSize
	}

	// Dial, authenticate and warm every socket before the timer.
	pipes, err := warmSockets(conns, window, ids, o)
	defer func() {
		for _, n := range pipes {
			n.close()
		}
	}()
	if err != nil {
		return out, err
	}

	articles, bytes, elapsed := pumpAll(ctx, pipes, ids, window)
	if err := ctx.Err(); err != nil {
		return out, err
	}
	out.Articles, out.Bytes = int64(articles), bytes
	out.Failures = len(ids) - articles
	if out.Failures > 0 {
		return out, fmt.Errorf("%d/%d article fetches failed", out.Failures, len(ids))
	}
	out.MiBs = mibs(bytes, elapsed)
	return out, nil
}

// warmSockets brings conns sockets up in parallel and hands back everything it
// managed to dial, whether or not one of them failed, so the caller closes them
// either way.
func warmSockets(conns, window int, ids []string, o NewsBaselineOptions) ([]*nntConn, error) {
	pipes := make([]*nntConn, conns)
	errs := make([]error, conns)
	var wg sync.WaitGroup
	for i := range conns {
		wg.Add(1)
		go func() {
			defer wg.Done()
			n, err := dialConn(o.Host, o.Port)
			if err != nil {
				errs[i] = fmt.Errorf("dial: %w", err)
				return
			}
			pipes[i] = n
			if err := n.auth(o.User, o.Pass); err != nil {
				errs[i] = fmt.Errorf("auth: %w", err)
				return
			}
			if err := n.warm(ids, window); err != nil {
				errs[i] = fmt.Errorf("warm: %w", err)
			}
		}()
	}
	wg.Wait()
	live := make([]*nntConn, 0, conns)
	for _, n := range pipes {
		if n != nil {
			live = append(live, n)
		}
	}
	return live, errors.Join(errs...)
}

// baselineNews measures the fixture news server at a given connection count.
func (r *Runner) baselineNews(ctx context.Context, conns int) (BaselineResult, error) {
	return NewsBaseline(ctx, NewsBaselineOptions{
		Host: r.Host, Port: r.NewsPort, User: "mock", Pass: "mock",
		NzbPath: r.nzbPath("plain.nzb"), MaxConn: conns, PipelineSize: DefaultBaselinePipelineSize,
	})
}

// collectIDs returns up to limit message-ids from the nzb, all by default.
func collectIDs(data string, limit int) []string {
	ms := segmentRe.FindAllStringSubmatch(data, -1)
	ids := make([]string, 0, len(ms))
	for _, m := range ms {
		ids = append(ids, m[1])
		if limit > 0 && len(ids) >= limit {
			break
		}
	}
	return ids
}

// baselineCacheWrite measures cache device write speed by writing a temp file
// straight through to the cache. The rate comes from dd's own report on stderr
// (busybox prints "N bytes copied, <t> s, <X>"): dd times only the transfer
// inside the container, so the docker-compose-exec setup is never part of the
// measurement. conv=fsync (not fdatasync): busybox has no fdatasync.
func (r *Runner) baselineCacheWrite(ctx context.Context, sizeMB int) (float64, error) {
	out, err := r.streamerCmd(ctx, ddWrite(sizeMB))
	_, _ = r.streamerCmd(ctx, "rm -f "+baselineFile)
	if err != nil {
		return 0, err
	}
	return parseDDRate(out)
}

// baselineCacheRead measures cache device read speed from the temp file,
// writing it first if it is not already on the cache: a read has to hit
// something, and a warm page-cache read may bypass the device entirely,
// mirroring the cap.
func (r *Runner) baselineCacheRead(ctx context.Context, sizeMB int) (float64, error) {
	if _, err := r.streamerCmd(ctx, "test -f "+baselineFile); err != nil {
		if _, err := r.streamerCmd(ctx, ddWrite(sizeMB)); err != nil {
			return 0, err
		}
	}
	out, err := r.streamerCmd(ctx, "dd if="+baselineFile+" of=/dev/null bs=1M 2>&1")
	if err != nil {
		return 0, err
	}
	return parseDDRate(out)
}

// baselineFile is the cache-device measurement target both cache baselines use.
const baselineFile = "/app/.cache/baseline.tmp"

func ddWrite(sizeMB int) string {
	return fmt.Sprintf("dd if=/dev/zero of=%s bs=1M count=%d conv=fsync 2>&1", baselineFile, sizeMB)
}

// ddSummaryRe matches dd's stderr throughput summary, both busybox
// ("N bytes (64.0MB) copied, 0.010149 seconds, 6.2GB/s") and GNU
// ("N bytes (1.1 GB) copied, 0.291891 s, 3.68 GB/s").
var ddSummaryRe = regexp.MustCompile(`([0-9]+) bytes .*copied, ([0-9.]+) (s|seconds),`)

// parseDDRate extracts the MiB/s dd reports for itself.
func parseDDRate(out string) (float64, error) {
	m := ddSummaryRe.FindStringSubmatch(out)
	if m == nil {
		return 0, fmt.Errorf("no dd summary in output: %q", out)
	}
	bytes, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		return 0, err
	}
	sec, err := strconv.ParseFloat(m[2], 64)
	if err != nil {
		return 0, err
	}
	if sec <= 0 {
		return 0, fmt.Errorf("dd elapsed %v", sec)
	}
	return float64(bytes) / (1 << 20) / sec, nil
}

// appConns is the sweep's USENET_MAX_CONN option, 0 when the run left the var
// to the application default. The harness knows this one name because
// baseline-news mirrors the app's main concurrency knob; the generic sweep
// model does not.
func appConns(cell Cell) int {
	if n, err := strconv.Atoi(cell.AppEnv["USENET_MAX_CONN"]); err == nil {
		return n
	}
	return 0
}

// newsProjection identifies a cell by everything that reaches the news link,
// which is what one baseline-news measurement stands for: every read cell with
// this projection is judged against it.
func newsProjection(cell Cell) string {
	return fmt.Sprintf("%d/%d/%d/%d/%d", appConns(cell), cell.LatencyMs, cell.JitterMs,
		cell.LineSpeed, cell.LineJitter)
}

// cacheProjection identifies a cell by the cache-device caps, the only axes a
// cache baseline exercises.
func cacheProjection(cell Cell) string {
	return fmt.Sprintf("%d/%d", cell.CacheWriteSpeed, cell.CacheReadSpeed)
}

// projection is the identity a baseline of this kind measures at.
func projection(cell Cell, kind string) string {
	switch kind {
	case BaselineNews:
		return newsProjection(cell)
	case BaselineCacheWrite, BaselineCacheRead:
		return cacheProjection(cell)
	}
	return ""
}

// runBaselines executes the requested baseline tests against the test rig (the
// news server, the cache device) rather than the app, sweeping the same
// combined cell set the read tests use so -combine applies to them too. Each
// baseline kind only exercises the axes that reach its target, so a kind runs
// once per distinct projection of the cells onto those axes, repeated
// opts.Repeats times. Container metrics ride the same outcome columns as reads:
// news-server CPU/memory for baseline-news, streamer container for the
// dd-driven cache baselines. No pprof: the streamer is not on a baseline's path.
func (r *Runner) runBaselines(ctx context.Context, cells []Cell, opts RunOptions) ([]Result, error) {
	// Distinct projections of the cells onto each kind's relevant axes; the
	// first cell of a projection carries the representative envelope.
	projected := map[string][]Cell{}
	for _, kind := range opts.Baselines {
		seen := map[string]bool{}
		for _, cell := range cells {
			key := projection(cell, kind)
			if key != "" && !seen[key] {
				seen[key] = true
				projected[kind] = append(projected[kind], cell)
			}
		}
	}

	total := 0
	for _, kind := range opts.Baselines {
		total += len(projected[kind]) * opts.repeats()
	}
	var results []Result
	done, start := 0, time.Now()
	for _, kind := range opts.Baselines {
		for _, cell := range projected[kind] {
			// The cell's envelope also gates cap-unsupported notes on its rows.
			if err := r.applyCellEnv(ctx, cell); err != nil {
				return results, err
			}
			for rep := range opts.repeats() {
				done++
				res := r.runBaseline(ctx, kind, cell, rep)
				fmt.Printf("[%3d/%3d] %-18s %-28s %9.1f MiB/s  %s%s\n",
					done, total, kind, opts.Matrix.Row(cell), res.MiBs,
					time.Since(start).Round(time.Second), eta(start, done, total))
				results = append(results, res)
			}
		}
	}
	// The baselines run after the cell sweep; clear the leftover envelope so
	// the next phase (if any) starts clean.
	if r.netemUsable() {
		_ = r.ApplyNetemFull(ctx, 0, 0, 0, 0)
	}
	if r.deviceBpsUsable() {
		_ = r.ApplyCacheThrottle(ctx, 0, 0)
	}
	return results, nil
}

// runBaseline takes one baseline measurement, with the same before/after
// container snapshots a read takes: for baseline-news the news container is the
// interesting one, for the cache baselines the streamer container dd runs in.
func (r *Runner) runBaseline(ctx context.Context, kind string, cell Cell, rep int) Result {
	res := Result{Test: kind, Phase: "measure", Rep: rep, Cell: cell}
	before := r.sampleStreamer(ctx)
	newsBefore := r.sampleNews(ctx)

	var err error
	switch kind {
	case BaselineNews:
		conns := appConns(cell)
		if conns == 0 {
			conns = defaultAppConns
		}
		var b BaselineResult
		b, err = r.baselineNews(ctx, conns)
		res.MiBs, res.Bytes = b.MiBs, b.Bytes
	case BaselineCacheWrite:
		res.MiBs, err = r.baselineCacheWrite(ctx, baselineSizeMB)
	case BaselineCacheRead:
		res.MiBs, err = r.baselineCacheRead(ctx, baselineSizeMB)
	}

	res.usage(before, r.sampleStreamer(ctx), newsBefore, r.sampleNews(ctx))
	res.OK = err == nil
	if err != nil {
		res.Note = r.prefixNote(cell, err.Error())
	}
	return res
}

// defaultAppConns mirrors USENET_MAX_CONN's own default, which is what the
// news baseline measures at when the sweep leaves the var alone.
const defaultAppConns = 20

// baselineSizeMB is how much each cache baseline moves: enough for a device
// cap to show, short enough to repeat.
const baselineSizeMB = 64
