package harness

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// clkTck is Linux's default USER_HZ, the unit /proc/pid/stat ticks are in.
const clkTck = 100

// sample is one container-side resource snapshot: the summed process counters
// of the processes that matter there, plus the VM's aggregate iowait.
type sample struct {
	ticks    int64  // utime+stime summed over the matched processes
	rss, hwm int64  // summed VmRSS, largest VmHWM
	iowait   uint64 // aggregate /proc/stat "cpu " iowait ticks, all cores
}

// The compose entrypoint execs nzbstreamer, making it PID 1; the news
// entrypoint instead backgrounds innd and forks nnrpd per reader connection,
// so the news measurement targets innd|nnrpd by comm. CPU and RSS are summed
// over all matching processes; the peak is the largest per-process HWM. A
// process that exits between two snapshots loses its delta, which is exact for
// reads that hold their connections across both.
func (r *Runner) sampleStreamer(ctx context.Context) sample {
	return r.sampleIn(ctx, r.streamerCmd, "nzbstreamer")
}

func (r *Runner) sampleNews(ctx context.Context) sample {
	return r.sampleIn(ctx, r.newsCmd, "innd|nnrpd")
}

// sampleIn collects a container's counters in one docker exec: it runs around a
// read, never inside it, and one exec rather than two keeps that overhead off
// the read's own window. Everything is 0 when the container cannot answer.
//
// /proc/pid/stat carries the ticks (14 utime, 15 stime, after the comm in
// parens), /proc/pid/status the memory, and /proc/stat's "cpu " line the
// aggregate iowait, its fifth field after the label.
func (r *Runner) sampleIn(ctx context.Context, cmd func(context.Context, string) (string, error), names string) sample {
	sh := fmt.Sprintf(`awk -v re='^(%s)$' '
FILENAME == "/proc/stat" { if ($1 == "cpu") w = $6; next }
FILENAME ~ /\/stat$/ { c = substr($2, 2, length($2) - 2); if (c ~ re) t += $14 + $15; next }
$1 == "Name:" { m = ($2 ~ re) }
m && $1 == "VmRSS:" { r += $2 * 1024 }
m && $1 == "VmHWM:" && $2 * 1024 > h { h = $2 * 1024 }
END { print t+0, r+0, h+0, w+0 }
' /proc/[0-9]*/stat /proc/[0-9]*/status /proc/stat 2>/dev/null`, names)
	out, err := cmd(ctx, sh)
	if err != nil {
		return sample{}
	}
	var s sample
	if _, err := fmt.Sscanf(strings.TrimSpace(out), "%d %d %d %d", &s.ticks, &s.rss, &s.hwm, &s.iowait); err != nil {
		return sample{}
	}
	return s
}

// ticksCPU converts a tick delta (process time) to a duration.
func ticksCPU(delta int64) time.Duration {
	if delta < 0 {
		delta = 0
	}
	return time.Duration(delta) * (time.Second / clkTck)
}

// usage fills the resource columns of a Result from a before/after pair of
// snapshots of both containers, which is the same arithmetic for every kind of
// reading.
func (res *Result) usage(before, after, newsBefore, newsAfter sample) {
	res.CPU = ticksCPU(after.ticks - before.ticks)
	res.CPUWait = ticksCPU(int64(after.iowait) - int64(before.iowait))
	res.MemBytes = after.rss
	res.PeakBytes = after.hwm
	res.NewsCPU = ticksCPU(newsAfter.ticks - newsBefore.ticks)
	res.NewsMemBytes = newsAfter.rss
	res.NewsPeakBytes = newsAfter.hwm
}
