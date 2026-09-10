package harness

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Probe reports which stack-level mechanisms actually work on this Docker host.
// Each field is an error: nil means the mechanism works, or carries the reason
// it does not. The run never aborts on an unsupported mechanism — each affected
// cell just gets a "cap=axis:unsupported" note on its results, and fuse-* tests
// are skipped when the mount cannot be established.
type Probe struct {
	Netem     error // applying tc netem to the news link works
	DeviceBps error // docker --device-*-bps actually throttles the cache device
	Fuse      error // a FUSE mount can be established in the streamer container
	Tmpfs     bool  // informational: -ram override loaded (a config fact, not a failure)
	// NotSwept marks a mechanism no cell of the run imposes, whose probe was
	// skipped. A cell asking for it anyway is attempted normally.
	NetemNotSwept     bool
	DeviceBpsNotSwept bool
}

// Probe figures out what works on this machine. news must be running for netem
// and the streamer container up for device-bps/fuse. The underlying errors are
// kept whole so callers can wrap and pass them up.
//
// cells is the run's sweep: a mechanism no cell imposes is reported as not
// swept and its probe skipped, since its cost buys nothing (nil cells probes
// everything, which is what -probe-only wants). keepServing says whether the
// streamer should be restarted without the probe cap afterwards; a caller that
// recreates the streamer right after passes false, and that recreate lands the
// cleared cap.
func (r *Runner) Probe(ctx context.Context, cells []Cell, keepServing bool) *Probe {
	p := &Probe{}
	r.ProbeResult = p
	needNetem := cells == nil || sweepsNetem(cells)
	needBps := cells == nil || sweepsBps(cells)
	p.NetemNotSwept = !needNetem
	p.DeviceBpsNotSwept = !needBps

	// netem: actually apply a tiny qdisc to the news link, then take it off.
	if needNetem {
		if err := r.ApplyNetem(ctx, 1, 0); err != nil {
			p.Netem = fmt.Errorf("apply netem: %w", err)
		} else {
			_ = r.ApplyNetem(ctx, 0, 0) // leave the link clean
		}
	}

	// device-bps: discover the cache device, bake a 4MB/s write cap into a
	// compose override, recreate the streamer so the bake lands, then time a
	// direct-I/O write to it and see whether the cap actually stuck. A failed
	// recreate (e.g. the io.max cgroup missing on this kernel) is itself the
	// "unsupported" answer; either way the override is cleared and the streamer
	// brought back up so the stack stays usable.
	// restore clears the cap and recreates the streamer without it; the error
	// paths use it whole, the success path only clears and optionally restarts.
	restore := func() {
		_ = r.ApplyCacheThrottle(ctx, 0, 0) // no cap write needs no device
		_ = r.WarmRestart(ctx)
	}
	if !needBps {
		return r.probeFuse(ctx, p)
	}
	dev, err := r.cacheDevice(ctx)
	if err != nil {
		p.DeviceBps = fmt.Errorf("discover cache device: %w", err)
	} else if err := r.ApplyCacheThrottle(ctx, 4, 0); err != nil {
		restore()
		p.DeviceBps = fmt.Errorf("cap override: %w", err)
	} else if err := r.WarmRestart(ctx); err != nil {
		restore()
		p.DeviceBps = fmt.Errorf("recreate streamer: %w", err)
	} else {
		start := time.Now()
		// oflag=direct + conv=fsync: busybox (this Alpine image) has no
		// fdatasync conv value.
		_, derr := r.streamerCmd(ctx, fmt.Sprintf(
			"dd if=/dev/zero of=/app/.cache/probe.tmp bs=1M count=%d oflag=direct conv=fsync 2>/dev/null", probeWriteMB))
		elapsed := time.Since(start)
		_, _ = r.streamerCmd(ctx, "rm -f /app/.cache/probe.tmp")
		// Clear the cap in the override. The streamer keeps serving with the
		// probe cap until its next recreate lands the cleared file, which is
		// the first cell's recreate on a sweep; keepServing asks for the
		// restart instead.
		_ = r.ApplyCacheThrottle(ctx, 0, 0)
		if keepServing {
			_ = r.WarmRestart(ctx)
		}
		switch {
		case derr != nil:
			p.DeviceBps = fmt.Errorf("direct write: %w", derr)
		case float64(probeWriteMB)/elapsed.Seconds() >= 8:
			// Under the 4MB/s cap the write takes ~8s (8 MiB/s or less); an
			// ignored cap finishes near-native, far above that. Docker Desktop
			// kernels commonly ignore blkio device caps.
			p.DeviceBps = fmt.Errorf("cap ignored: ~%.1f MiB/s on %s under a 4MB/s cap",
				float64(probeWriteMB)/elapsed.Seconds(), dev)
		}
	}
	return r.probeFuse(ctx, p)
}

// probeFuse checks the FUSE mount and finishes the probe. The real capability
// is the mount actually being up, which proves device + SYS_ADMIN + apparmor +
// the app's MOUNT_PATH wiring end to end. Without the -fuse override no
// service sets MOUNT_PATH, so no mount exists and the probe honestly reports
// unsupported.
func (r *Runner) probeFuse(ctx context.Context, p *Probe) *Probe {
	if _, err := r.streamerCmd(ctx, "mount | grep -q ' on /app/mnt '"); err != nil {
		p.Fuse = fmt.Errorf("/app/mnt not mounted: %w", err)
	}
	p.Tmpfs = r.Ram
	return p
}

// sweepsNetem reports whether any cell imposes a netem axis, which is when the
// netem probe is worth its two netshoot container runs.
func sweepsNetem(cells []Cell) bool {
	for _, c := range cells {
		if c.LatencyMs != 0 || c.JitterMs != 0 || c.LineSpeed != 0 || c.LineJitter != 0 {
			return true
		}
	}
	return false
}

// sweepsBps reports whether any cell imposes a cache-device cap, which is when
// the device-bps probe is worth its two streamer recreations and capped write.
func sweepsBps(cells []Cell) bool {
	for _, c := range cells {
		if c.CacheWriteSpeed != 0 || c.CacheReadSpeed != 0 {
			return true
		}
	}
	return false
}

// PrintCapabilities writes the capability table to stdout.
func (p *Probe) PrintCapabilities() {
	fmt.Println("capabilities:")
	fmt.Printf("  %-12s %-13s %s\n", "netem", capWord(p.Netem, p.NetemNotSwept), capNote(p.Netem, p.NetemNotSwept))
	fmt.Printf("  %-12s %-13s %s\n", "device-bps", capWord(p.DeviceBps, p.DeviceBpsNotSwept), capNote(p.DeviceBps, p.DeviceBpsNotSwept))
	fmt.Printf("  %-12s %-13s %s\n", "fuse", capWord(p.Fuse, false), capNote(p.Fuse, false))
	fmt.Printf("  %-12s %-13s\n", "tmpfs", strconv.FormatBool(p.Tmpfs))
}

func capWord(err error, notSwept bool) string {
	switch {
	case notSwept:
		return "not swept"
	case err == nil:
		return "supported"
	}
	return "unsupported"
}

func capNote(err error, notSwept bool) string {
	switch {
	case notSwept:
		return "no cell of this run imposes it"
	case err == nil:
		return "ok"
	}
	return err.Error()
}

// netemUsable reports whether the probe found netem applicable on this host
// (or no probe ran and we should attempt it).
func (r *Runner) netemUsable() bool {
	return r.ProbeResult == nil || r.ProbeResult.Netem == nil
}

// deviceBpsUsable reports whether the probe found device bandwidth caps
// enforceable on this host (or no probe ran and we should attempt them).
func (r *Runner) deviceBpsUsable() bool {
	return r.ProbeResult == nil || r.ProbeResult.DeviceBps == nil
}

// fuseUsable reports whether the probe found a live FUSE mount in the streamer
// (or no probe ran and the mount should be attempted).
func (r *Runner) fuseUsable() bool {
	return r.ProbeResult == nil || r.ProbeResult.Fuse == nil
}

// capPrefix returns a note prefix flagging every axis this cell asks for that
// the probe says the host cannot impose.
// probeWriteMB is how much the device-bps probe writes: enough that a 4MB/s cap
// costs seconds and an ignored one costs none, and no more, since the probe
// runs before every sweep.
const probeWriteMB = 32

func (r *Runner) capPrefix(cell Cell) string {
	if r.ProbeResult == nil {
		return ""
	}
	var notes []string
	if r.ProbeResult.Netem != nil &&
		(cell.LatencyMs != 0 || cell.JitterMs != 0 || cell.LineSpeed != 0 || cell.LineJitter != 0) {
		notes = append(notes, "cap:netem=unsupported")
	}
	if r.ProbeResult.DeviceBps != nil &&
		(cell.CacheWriteSpeed != 0 || cell.CacheReadSpeed != 0) {
		notes = append(notes, "cap:device-bps=unsupported")
	}
	return strings.Join(notes, " ")
}

// prefixNote prepends any cap-unsupported flag for the cell to the given note.
func (r *Runner) prefixNote(cell Cell, note string) string {
	prefix := r.capPrefix(cell)
	if prefix == "" {
		return note
	}
	if note == "" {
		return prefix
	}
	return prefix + "; " + note
}
