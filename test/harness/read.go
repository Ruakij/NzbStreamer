package harness

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"sync"
	"time"

	"git.ruekov.eu/ruakij/nzbStreamer/test/readclient"
)

// sha256SumRe matches busybox sha256sum's output line ("<digest>  -").
var sha256SumRe = regexp.MustCompile(`([0-9a-f]{64})  -`)

// readOnce performs one measured read of the test's path under the cell and
// marks the result with its repetition index. A FUSE test reads through the
// mount namespace instead of HTTP; a two-path test reads both at once.
func (r *Runner) readOnce(ctx context.Context, client *http.Client, t Test, cell Cell, cellpath string, phase string, rep int) Result {
	switch {
	case t.Fuse():
		return r.fuseRead(ctx, t, cell, cellpath, phase, rep)
	case t.Concurrent():
		return r.concurrentRead(ctx, client, t, cell, cellpath, phase, rep)
	default:
		return r.httpRead(ctx, client, t, cell, cellpath, phase, rep)
	}
}

// httpRead reads the test's path over webdav following the cell's read pattern.
//
// Validation only happens where the read covers the whole file: the bytes are
// hashed as they stream and compared with the payload digest for the same plan.
// The expected digest is the payload slices concatenated in plan order - a tail
// or random plan is a permutation of the file, not the file itself.
func (r *Runner) httpRead(ctx context.Context, client *http.Client, t Test, cell Cell, cellpath string, phase string, rep int) Result {
	plan := readclient.Plan(cell.ReadType, t.ExpectedSize(), MatrixChunk, cell.Seed)

	w := io.Discard
	var hasher hash.Hash
	expected := ""
	if t.CanValidate(cell.ReadType) && !t.ExpectErr() {
		hasher = sha256.New()
		w = hasher
		exp, err := r.expectedDigest(t, plan)
		if err != nil {
			return r.resErr(t, cell, phase, rep, err)
		}
		expected = exp
	}

	before := r.sampleStreamer(ctx)
	newsBefore := r.sampleNews(ctx)
	prof := r.startProf(ctx, t, cellpath, phase, rep)
	start := time.Now()
	n, ttfb, perRange, perRTT, err := readclient.ReadHTTP(ctx, client, r.BaseURL, t.Path(), plan, w)
	elapsed := time.Since(start)
	after := r.sampleStreamer(ctx)
	newsAfter := r.sampleNews(ctx)
	r.stopProf(ctx, t, cellpath, phase, rep, prof)

	res := Result{
		Test:       t.Name(),
		Rep:        rep,
		Cell:       cell,
		Phase:      phase,
		Bytes:      n,
		Wall:       elapsed,
		TTFB:       ttfb,
		TTFBP50:    readclient.PercentileDur(perRange, 50),
		TTFBP95:    readclient.PercentileDur(perRange, 95),
		LatencyP50: readclient.PercentileDur(perRTT, 50),
		LatencyP95: readclient.PercentileDur(perRTT, 95),
		MiBs:       mibs(n, elapsed),
	}
	res.usage(before, after, newsBefore, newsAfter)

	// Cap-unsupported flags ride along on every result of the cell, whatever the
	// outcome, so the rows that matter keep saying what the host could not do.
	note := func(s string) string { return r.prefixNote(cell, s) }
	switch {
	case t.ExpectErr():
		// The damaged set must fail to read; a read that succeeds is the bug.
		if err != nil {
			res.OK, res.Note = true, note("expected error")
		} else {
			res.OK, res.Note = false, note("damaged set read without error")
		}
	case err != nil:
		res.OK, res.Note = false, note(err.Error())
	case expected != "":
		res.OK = hex.EncodeToString(hasher.Sum(nil)) == expected
		res.Note = note("")
		if !res.OK {
			res.Note = note("digest mismatch")
		}
	default:
		res.OK, res.Note = true, note("")
	}
	return res
}

// fuseRead measures a read through the FUSE mount. The mount lives only in the
// streamer's namespace, so the read happens in-container: dd streams the file
// to sha256sum, dd self-reports its own rate on stderr and the digest lands on
// stdout. Wall (and with it MiBs) is the host's clock around the whole docker
// exec, the same thing every other test reports; DDMiBs is the transfer alone.
// Payload-backed tests read the whole file sequentially and validate the digest;
// archive-container tests (7z, zip) only gate on non-empty bytes; the damaged
// set is expected to be absent.
func (r *Runner) fuseRead(ctx context.Context, t Test, cell Cell, cellpath string, phase string, rep int) Result {
	if !r.fuseUsable() {
		return r.resErr(t, cell, phase, rep, fmt.Errorf("cap:fuse=unsupported"))
	}
	expected := ""
	if t.validate && t.ExpectedSize() > 0 {
		exp, err := r.expectedDigest(t, readclient.Plan("sequential", t.ExpectedSize(), MatrixChunk, 0))
		if err != nil {
			return r.resErr(t, cell, phase, rep, err)
		}
		expected = exp
	}

	before := r.sampleStreamer(ctx)
	newsBefore := r.sampleNews(ctx)
	prof := r.startProf(ctx, t, cellpath, phase, rep)
	start := time.Now()
	out, err := r.streamerCmd(ctx, fmt.Sprintf("dd if=%s bs=1M | sha256sum", t.FusePath()))
	elapsed := time.Since(start)
	after := r.sampleStreamer(ctx)
	newsAfter := r.sampleNews(ctx)
	r.stopProf(ctx, t, cellpath, phase, rep, prof)

	ddMiBs, _ := parseDDRate(out) // 0 when the read failed before dd reported
	bytes := int64(0)
	if m := ddSummaryRe.FindStringSubmatch(out); m != nil {
		bytes, _ = strconv.ParseInt(m[1], 10, 64)
	}

	res := Result{
		Test:   t.Name(),
		Rep:    rep,
		Cell:   cell,
		Phase:  phase,
		Bytes:  bytes,
		Wall:   elapsed,
		MiBs:   mibs(bytes, elapsed),
		DDMiBs: ddMiBs,
	}
	res.usage(before, after, newsBefore, newsAfter)

	note := ""
	switch {
	case t.ExpectErr():
		// The damaged set is health-checked away and must not be in the mount;
		// a read that succeeds is the bug.
		if err != nil {
			res.OK, note = true, "expected error"
		} else {
			res.OK, note = false, "damaged set read without error"
		}
	case err != nil:
		res.OK, note = false, err.Error()
	default:
		if expected != "" {
			got := ""
			if s := sha256SumRe.FindStringSubmatch(out); s != nil {
				got = s[1]
			}
			switch {
			case got == "":
				note = "no sha256 sum in output"
			case got != expected:
				note = fmt.Sprintf("digest mismatch got %s... want %s...", got[:12], expected[:12])
			case bytes != t.ExpectedSize():
				note = fmt.Sprintf("short read: %d/%d bytes", bytes, t.ExpectedSize())
			}
		} else if bytes <= 0 {
			note = "empty read"
		}
		res.OK = note == ""
	}
	res.Note = r.prefixNote(cell, note)
	return res
}

// concurrentRead reads two files in parallel, each as a sequence of chunk-sized
// ranged requests over the shared client, to measure per-request latency under
// contention. The shared http.Client is safe for concurrent use; nothing else
// mutable is shared between the goroutines.
func (r *Runner) concurrentRead(ctx context.Context, client *http.Client, t Test, cell Cell, cellpath string, phase string, rep int) Result {
	size := t.ExpectedSize()
	plan := seqChunks(size, MatrixChunk)

	before := r.sampleStreamer(ctx)
	newsBefore := r.sampleNews(ctx)
	prof := r.startProf(ctx, t, cellpath, phase, rep)

	type reader struct {
		n   int64
		d   time.Duration
		per []time.Duration // per-request TTFB
		rtt []time.Duration // per-request call->reply RTT
		err error
	}
	paths := []string{t.Path(), t.Path2()}
	readers := make([]reader, len(paths))
	var wg sync.WaitGroup
	for i, p := range paths {
		wg.Add(1)
		go func() {
			defer wg.Done()
			t0 := time.Now()
			n, _, per, rtt, err := readclient.ReadHTTP(ctx, client, r.BaseURL, p, plan, nil)
			readers[i] = reader{n, time.Since(t0), per, rtt, err}
		}()
	}
	wg.Wait()

	after := r.sampleStreamer(ctx)
	newsAfter := r.sampleNews(ctx)
	r.stopProf(ctx, t, cellpath, phase, rep, prof)

	// One reader owns each file, so the per-request union is both readers'
	// ranges; p50/p95 of that union is the latency under contention.
	var wall time.Duration
	var totalBytes int64
	var perRange, perRTT []time.Duration
	for _, rd := range readers {
		wall = max(wall, rd.d)
		totalBytes += rd.n
		perRange = append(perRange, rd.per...)
		perRTT = append(perRTT, rd.rtt...)
	}

	res := Result{
		Test:       t.Name(),
		Rep:        rep,
		Cell:       cell,
		Phase:      phase,
		Bytes:      totalBytes,
		Wall:       wall,
		TTFB:       readclient.PercentileDur(perRange[:min(1, len(perRange))], 50), // first sample, 0 if the read never started
		TTFBP50:    readclient.PercentileDur(perRange, 50),
		TTFBP95:    readclient.PercentileDur(perRange, 95),
		LatencyP50: readclient.PercentileDur(perRTT, 50),
		LatencyP95: readclient.PercentileDur(perRTT, 95),
		MiBs:       mibs(totalBytes, wall),
	}
	res.usage(before, after, newsBefore, newsAfter)

	note := func(s string) string { return r.prefixNote(cell, s) }
	res.OK = true
	for _, rd := range readers {
		if rd.err != nil {
			res.OK, res.Note = false, note(rd.err.Error())
			break
		}
		if rd.n != size {
			res.OK, res.Note = false, note(fmt.Sprintf("short read: %d/%d bytes", rd.n, size))
			break
		}
	}
	if res.OK {
		res.Note = note("")
	}
	return res
}

// resErr builds a Result that failed before a read could run (e.g. the
// expected-digest computation), marked not-OK with the reason.
func (r *Runner) resErr(t Test, cell Cell, phase string, rep int, err error) Result {
	return Result{
		Test:  t.Name(),
		Rep:   rep,
		Cell:  cell,
		Phase: phase,
		Note:  r.prefixNote(cell, err.Error()),
	}
}

// seqChunks splits [0, size) into chunk-aligned ranges covering the whole file
// in order. readclient.Plan collapses its "sequential" readtype to a single
// whole-file range, which would yield one TTFB sample; a concurrent test needs
// one request (and thus one sample) per chunk.
func seqChunks(size, chunk int64) []readclient.Range {
	if size <= 0 {
		return []readclient.Range{{Offset: 0, Len: -1}}
	}
	var out []readclient.Range
	for off := int64(0); off < size; off += chunk {
		out = append(out, readclient.Range{Offset: off, Len: min(chunk, size-off)})
	}
	return out
}

// expectedDigest is the payload digest a full-covered read of the plan must
// match: the payload slices concatenated in plan order. It reads the generated
// source file the news server was filled from, so an arbitrary offset costs a
// seek rather than a re-run of the generator from zero - a random or tail plan
// over a 256 MiB payload would otherwise regenerate gigabytes per reading.
func (r *Runner) expectedDigest(t Test, plan []readclient.Range) (string, error) {
	name := filepath.Join(r.payloadDir(), t.Content())
	f, err := os.Open(name)
	if err != nil {
		return "", fmt.Errorf("expected digest: %w", err)
	}
	defer f.Close()

	h := sha256.New()
	for _, seg := range plan {
		if _, err := io.Copy(h, io.NewSectionReader(f, seg.Offset, seg.Len)); err != nil {
			return "", fmt.Errorf("expected digest %s at %d: %w", name, seg.Offset, err)
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
