package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"math/rand"
	"mime/multipart"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"git.ruekov.eu/ruakij/nzbStreamer/test/payload"
)

// Runner drives the compose stack from the host: generates the payloads,
// brings the news server up with the right fixtures, drops and recreates the
// streamer to impose each cell's settings, and perturbs the news link with
// netem when a cell asks for latency. Everything shells out to docker; the
// harness deliberately stays a thin executor with no docker SDK dependency.
//
// Debug endpoints: compose.yaml sets HTTP_DEBUG: "true" on the streamer service
// environment directly, which is where a setting the sweep must never touch
// belongs - the service reads the sweep's env file as its env_file, and
// environment wins over it. The service also publishes host
// ${HTTP_PORT:-8090} -> container :8080 (HTTP_ADDRESS), and the config mounts
// /debug/pprof/ on that same :8080 mux, so
// http://127.0.0.1:8090/debug/pprof/... is reachable from the host for the
// profiler (see profiler.go).
type Runner struct {
	ComposeFile string
	EnvFile     string // compose's --env-file and the streamer service's env_file
	BaseURL     string
	NewsPort    int           // published news port on the host
	NewsCid     string        // cached news container id, from NewsID
	Override    []string      // extra compose files merged after ComposeFile
	ProbeResult *Probe        // capability probe, set by Probe()
	Ram         bool          // memory-backed rig override loaded (-ram)
	Host        string        // where the published ports are reachable (E2E_HOST)
	netemStop   chan struct{} // closes the running line-jitter loop, set per cell
	prof        *Profiler     // pprof collector; set when RunOptions.ProfileDir != ""
}

// NewRunner returns a Runner pointed at the compose file, ready to be driven.
// The published ports come from the same environment compose interpolates
// them from, so a host that has to move them only says so once. E2E_HOST is the
// address to reach those ports on: localhost wherever the harness runs alongside
// the daemon.
func NewRunner(composeFile, envFile string) *Runner {
	return &Runner{
		ComposeFile: composeFile,
		EnvFile:     envFile,
		Host:        envOr("E2E_HOST", "127.0.0.1"),
		BaseURL:     "http://" + envOr("E2E_HOST", "127.0.0.1") + ":" + envOr("HTTP_PORT", "8090"),
		NewsPort:    atoiOr(envOr("NNTP_PORT", "1119"), 1119),
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func atoiOr(s string, def int) int {
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	return def
}

// projectName is the compose project every volume of the rig is prefixed with,
// which is how a volume is addressed by name from outside compose.
func (r *Runner) projectName() string {
	return envOr("COMPOSE_PROJECT_NAME", "nzbstreamer-e2e")
}

// nzbPath resolves a generated nzb next to the compose file, the same way
// payloadDir resolves the payloads.
func (r *Runner) nzbPath(name string) string {
	return filepath.Join(filepath.Dir(r.ComposeFile), "build", "nzb", name)
}

// composeCmd is the one place a docker invocation is spelled, so the compose
// file flag is always present (plus any Override files, e.g. the spool-ram
// tmpfs override).
func composeCmd(ctx context.Context, r *Runner, args ...string) *exec.Cmd {
	argv := []string{"compose", "-f", r.ComposeFile}
	for _, o := range r.Override {
		argv = append(argv, "-f", o)
	}
	argv = append(argv, args...)
	return exec.CommandContext(ctx, "docker", argv...)
}

// EnsurePayloads makes sure build/payload has every source file (regenerating
// if any is missing or the wrong size for the current run) and that the nzb
// directory exists for the news and streamer containers.
func (r *Runner) EnsurePayloads(dir string) error {
	if !completePayload(dir) {
		if err := payload.WriteSized(dir, payload.Size()); err != nil {
			return fmt.Errorf("write payloads: %w", err)
		}
	}
	if err := os.MkdirAll(filepath.Join(filepath.Dir(dir), "nzb"), 0o755); err != nil {
		return fmt.Errorf("mkdir nzb: %w", err)
	}
	return nil
}

// completePayload reports whether every source file is present at the size the
// current run wants; a SIZE_MB change regenerates.
func completePayload(dir string) bool {
	st, err := os.Stat(dir)
	if err != nil || !st.IsDir() {
		return false
	}
	for _, s := range payload.Sources() {
		fi, err := os.Stat(filepath.Join(dir, s.Name))
		if err != nil || fi.Size() != s.Size {
			return false
		}
	}
	return true
}

// Compose runs docker compose with the configured compose file, forwarding
// stderr and returning an error that carries stdout on failure.
func (r *Runner) Compose(ctx context.Context, args ...string) error {
	cmd := composeCmd(ctx, r, args...)
	cmd.Stderr = os.Stderr
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("docker compose %s: %w: %s",
			strings.Join(args, " "), err, strings.TrimSpace(out.String()))
	}
	return nil
}

// Up brings the named services up detached without forcing a rebuild; images
// are built by the -build flag, or by compose on first use.
func (r *Runner) Up(ctx context.Context, services ...string) error {
	args := append([]string{"up", "-d"}, services...)
	return r.Compose(ctx, args...)
}

// UpWithEnv brings the named services up with the env file, force-recreating
// them so freshly written sweep settings always take effect.
func (r *Runner) UpWithEnv(ctx context.Context, services ...string) error {
	args := append([]string{"--env-file", r.EnvFile, "up", "-d", "--force-recreate"}, services...)
	return r.Compose(ctx, args...)
}

// NewsID returns the id of the news container, cached after the first lookup.
func (r *Runner) NewsID(ctx context.Context) (string, error) {
	if r.NewsCid != "" {
		return r.NewsCid, nil
	}
	cmd := composeCmd(ctx, r, "ps", "-q", "news")
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("get news container id: %w", err)
	}
	id := strings.TrimSpace(out.String())
	if id == "" {
		return "", fmt.Errorf("news container not running?")
	}
	r.NewsCid = id
	return id, nil
}

// StreamerHealthy polls the streamer health endpoint until it answers 200.
func (r *Runner) StreamerHealthy(ctx context.Context) error {
	return r.waitHTTP(ctx, r.BaseURL+"/api/health", 120*time.Second, 1*time.Second,
		func(code int) bool { return code == 200 })
}

// AddNzbs uploads the generated nzbs of the given fixture sets through the app's api.
func (r *Runner) AddNzbs(ctx context.Context, sets []string) error {
	if len(sets) == 0 {
		sets = []string{""}
	}
	for _, set := range sets {
		// plain and plain2 are one set, so a set is a prefix.
		nzbs, err := filepath.Glob(r.nzbPath(set + "*.nzb"))
		if err != nil {
			return err
		}
		if len(nzbs) == 0 {
			return fmt.Errorf("no nzb for fixture %q", set)
		}
		for _, nzb := range nzbs {
			if err := r.addNzb(ctx, nzb); err != nil {
				return fmt.Errorf("add %s: %w", filepath.Base(nzb), err)
			}
		}
	}
	return nil
}

func (r *Runner) addNzb(ctx context.Context, path string) error {
	nzb, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part, err := mw.CreateFormFile("file", filepath.Base(path))
	if err != nil {
		return err
	}
	if _, err := part.Write(nzb); err != nil {
		return err
	}
	if err := mw.Close(); err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.BaseURL+"/api/add", &body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<12))
		return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	return nil
}

// ColdStart recreates the streamer with a fresh cache volume, so the read that
// follows measures a first-touch full download.
func (r *Runner) ColdStart(ctx context.Context) error {
	r.NewsCid = ""
	if err := r.Compose(ctx, "rm", "-sf", "streamer"); err != nil {
		return err
	}
	r.rmVolume(ctx, "cache")
	if err := r.UpWithEnv(ctx, "streamer"); err != nil {
		return err
	}
	return r.StreamerHealthy(ctx)
}

// WarmRestart recreates the streamer with the current env but keeps the cache
// volume, so the read that follows is a carried-over warm read.
func (r *Runner) WarmRestart(ctx context.Context) error {
	r.NewsCid = ""
	if err := r.UpWithEnv(ctx, "streamer"); err != nil {
		return err
	}
	return r.StreamerHealthy(ctx)
}

// rmVolume removes a named stack volume. Best-effort: the volume may not exist
// on a first run, and a removal that fails for any other reason surfaces in the
// step that then finds the volume still there.
func (r *Runner) rmVolume(ctx context.Context, name string) {
	_ = exec.CommandContext(ctx, "docker", "volume", "rm", r.projectName()+"_"+name).Run()
}

// DropFixtureVolumes tears the whole stack down with its volumes, leaving only
// the host bind mounts (payloads, nzbs).
func (r *Runner) DropFixtureVolumes(ctx context.Context) error {
	r.NewsCid = ""
	return r.Compose(ctx, "down", "-v")
}

// PostSets brings the news server up with the current fixtures. The news
// entrypoint only posts on a fresh spool, so the spool and db are wiped to
// make sure the delivered set matches the current builder — a changed
// archives.sh would otherwise never be reposted.
func (r *Runner) PostSets(ctx context.Context, sets []string) error {
	r.NewsCid = ""
	// A previous run's nzbs linger in build/nzb; a set removed from the builder
	// leaves its nzb behind, and AddNzbs would upload one nothing serves
	// anymore. Clean the dir before posting.
	if matches, _ := filepath.Glob(filepath.Join(filepath.Dir(r.ComposeFile), "build", "nzb", "*.nzb")); matches != nil {
		for _, m := range matches {
			_ = os.Remove(m)
		}
	}
	fmt.Println("posting fixtures and bringing the news server up...")
	// The news entrypoint filters on FIXTURE_SETS interpolated from this env
	// file; the rig defaults ride along, since the same file is the streamer's
	// env_file and a cell has not written its own yet.
	env := maps.Clone(RigDefaults)
	env["FIXTURE_SETS"] = strings.Join(sets, ",")
	if err := r.writeEnv(env); err != nil {
		return err
	}
	// Drop any stale news container from a -keep run first: its running mount
	// holds the db volume (and the .posted marker on it), so rmVolume below
	// would fail with "volume is in use" and the entrypoint would skip posting.
	// A first run has nothing to remove, which the command tolerates.
	if err := r.Compose(ctx, "--env-file", r.EnvFile, "rm", "-sf", "news"); err != nil {
		return err
	}
	// A fresh post's articles carry new (timestamped) message-ids, which makes
	// the streamer's persisted metadata index and any cached bytes stale — both
	// are keyed by the old post's segments. Free the streamer's volumes too,
	// wiping metadata and cache along with spool/db; the run then re-indexes
	// from the fresh nzbs and every read, cold or warm, is of the current post.
	if err := r.Compose(ctx, "--env-file", r.EnvFile, "rm", "-sf", "streamer"); err != nil {
		return err
	}
	for _, vol := range []string{"spool", "db", "metadata", "cache"} {
		r.rmVolume(ctx, vol)
	}
	// --force-recreate: an already running news container from a -keep run
	// would otherwise never pick up the wiped volumes, so the fresh post that
	// regenerates build/nzb/*.nzb would never happen.
	if err := r.Compose(ctx, "--env-file", r.EnvFile, "up", "-d", "--force-recreate", "news"); err != nil {
		return err
	}
	return r.WaitNewsHealthy(ctx)
}

// WaitNewsHealthy blocks until the news container reports healthy, i.e. it has
// finished posting its fixtures (the healthcheck is the .posted marker).
func (r *Runner) WaitNewsHealthy(ctx context.Context) error {
	id, err := r.NewsID(ctx)
	if err != nil {
		return err
	}
	deadline := time.Now().Add(240 * time.Second)
	for {
		cmd := exec.Command("docker", "inspect", "-f", "{{.State.Health.Status}}", id)
		var out bytes.Buffer
		cmd.Stdout = &out
		if err := cmd.Run(); err == nil && strings.TrimSpace(out.String()) == "healthy" {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("news not healthy within 240s")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// items is /api/items cut down to what the rig waits on.
type items struct {
	Queue   []queueItem         `json:"queue"`
	History []queueItem         `json:"history"`
	Files   map[string][]string `json:"files"`
}

type queueItem struct {
	ID    string `json:"id"`
	Stage string `json:"stage"`
	Err   string `json:"error"`
}

// presents reports whether any nzb presents this path.
func (it items) presents(path string) bool {
	for _, paths := range it.Files {
		if slices.Contains(paths, path) {
			return true
		}
	}
	return false
}

// finished returns the history entry of the add of id, if its add is over.
func (it items) finished(id string) (queueItem, bool) {
	for _, q := range it.Queue {
		if q.ID == id {
			return queueItem{}, false
		}
	}
	for _, h := range it.History {
		if h.ID == id {
			return h, true
		}
	}
	return queueItem{}, false
}

// WaitPath waits for the add of the nzb this path belongs to (its id is the
// first path segment) and reports whether it presents the path. Nothing is
// opened: an open starts the readahead window filling, which the cold reading
// that follows would then be racing.
func (r *Runner) WaitPath(ctx context.Context, path string, wantPresent bool) error {
	id, _, _ := strings.Cut(path, "/")
	deadline := time.Now().Add(120 * time.Second)
	var last error
	for {
		it, err := r.items(ctx)
		last = err
		if err == nil {
			done, over := it.finished(id)
			switch {
			case wantPresent && it.presents(path):
				return nil
			case !over:
			case wantPresent:
				return fmt.Errorf("nzb %s ended %s without presenting %s: %s", id, done.Stage, path, done.Err)
			case it.presents(path):
				return fmt.Errorf("nzb %s ended %s and presents %s, which it must not", id, done.Stage, path)
			default:
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("nzb %s did not settle %s within 120s (last: %w)", id, path, last)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// items polls the app's own listing of what it has added.
func (r *Runner) items(ctx context.Context) (items, error) {
	var out items
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.BaseURL+"/api/items", nil)
	if err != nil {
		return out, err
	}
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return out, fmt.Errorf("GET /api/items: %s", resp.Status)
	}
	return out, json.NewDecoder(resp.Body).Decode(&out)
}

// waitHTTP polls url with a GET until the predicate matches the status code or
// the timeout elapses.
func (r *Runner) waitHTTP(ctx context.Context, url string, timeout, interval time.Duration, ok func(int) bool) error {
	c := &http.Client{Timeout: 3 * time.Second}
	deadline := time.Now().Add(timeout)
	for {
		code := 0
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return err
		}
		if resp, err := c.Do(req); err == nil {
			code = resp.StatusCode
			resp.Body.Close()
		}
		if ok(code) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("waited %s on %s, last status %d", timeout, url, code)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
}

// ApplyNetem perturbs the news container's egress with delay/jitter only; the
// probe uses this. Sweep cells that also set a line rate use ApplyNetemFull.
func (r *Runner) ApplyNetem(ctx context.Context, latencyMs, jitterMs int) error {
	return r.ApplyNetemFull(ctx, latencyMs, jitterMs, 0, 0)
}

// ApplyNetemFull perturbs the news container's egress: delay plus a rate cap
// (the linespeed axis) and, when linejitter is set, a background loop that
// re-applies the qdisc every 250ms with a rate sampled around it — upload
// bandwidth on a real line wobbles, a fixed cap is the lie. netem runs from a
// small toolbox container in the news network namespace with NET_ADMIN. All
// axes zero clears any prior qdisc (best-effort) and stops the jitter loop.
func (r *Runner) ApplyNetemFull(ctx context.Context, latencyMs, jitterMs, rateMBs, rateJitterPct int) error {
	if r.netemStop != nil {
		close(r.netemStop)
		r.netemStop = nil
	}
	if latencyMs == 0 && jitterMs == 0 && rateMBs == 0 {
		_ = r.netemCmd(ctx, nil) // clear: an absent qdisc has nothing to remove
		return nil
	}
	if err := r.netemCmd(ctx, netemArgs(latencyMs, jitterMs, rateMBs)); err != nil {
		return err
	}
	if rateMBs > 0 && rateJitterPct > 0 {
		stop := make(chan struct{})
		r.netemStop = stop
		go r.lineJitterLoop(ctx, latencyMs, jitterMs, rateMBs, rateJitterPct, stop)
	}
	return nil
}

// lineJitterLoop re-applies the netem qdisc with a rate sampled uniformly in
// [-jitter%, +jitter%] around the base until stop closes (the next cell).
func (r *Runner) lineJitterLoop(ctx context.Context, latencyMs, jitterMs, rateMBs, rateJitterPct int, stop <-chan struct{}) {
	for {
		select {
		case <-stop:
			return
		case <-time.After(250 * time.Millisecond):
		}
		if ctx.Err() != nil {
			return
		}
		dev := rand.Intn(2*rateJitterPct+1) - rateJitterPct
		n := rateMBs + rateMBs*dev/100
		if n < 1 {
			n = 1
		}
		_ = r.netemCmd(ctx, netemArgs(latencyMs, jitterMs, n))
	}
}

// netemLimit sizes the qdisc's internal packet queue. The default 1000 packets
// drops under sustained oversubscription; a deep queue lets in-flight bytes
// build up like a real internet path.
const netemLimit = 4000

// netemArgs builds the tc netem arguments for the given delay and rate.
func netemArgs(latencyMs, jitterMs, rateMBs int) []string {
	args := []string{
		"netem", "delay",
		fmt.Sprintf("%dms", latencyMs),
		fmt.Sprintf("%dms", jitterMs),
	}
	if jitterMs > 0 {
		// tc rejects "distribution" unless there is a nonzero jitter; a
		// pure-latency cell must omit it.
		args = append(args, "distribution", "normal")
	}
	if rateMBs > 0 {
		args = append(args, "rate", fmt.Sprintf("%dmbit", rateMBs*8))
	}
	args = append(args, "limit", fmt.Sprintf("%d", netemLimit))
	return args
}

// netemCmd runs one tc operation against the news container's interface. A nil
// arg list clears the qdisc (best-effort); a real arg list (re)places it, so
// the jitter loop and the cell start can both issue it without an add/replace
// race.
func (r *Runner) netemCmd(ctx context.Context, args []string) error {
	id, err := r.NewsID(ctx)
	if err != nil {
		return err
	}
	if args == nil {
		cmd := exec.Command("docker", "run", "--rm", "--net=container:"+id, "--cap-add=NET_ADMIN",
			"nicolaka/netshoot", "tc", "qdisc", "del", "dev", "eth0", "root")
		_ = cmd.Run()
		return nil
	}
	argv := []string{"run", "--rm", "--net=container:" + id, "--cap-add=NET_ADMIN",
		"nicolaka/netshoot", "tc", "qdisc", "replace", "dev", "eth0", "root"}
	argv = append(argv, args...)
	cmd := exec.Command("docker", argv...)
	var out bytes.Buffer
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("netem: %w: %s", err, strings.TrimSpace(out.String()))
	}
	return nil
}

// cacheDevice returns the block device backing /app/.cache (e.g. /dev/sdX),
// which is what docker --device-write-bps/--device-read-bps expects to name.
// Requires the streamer container to be up.
func (r *Runner) cacheDevice(ctx context.Context) (string, error) {
	out, err := r.streamerCmd(ctx, "df -P /app/.cache")
	if err != nil {
		return "", err
	}
	// df -P last line: <device> <size> <used> ... <mount>; the device is the
	// first field of the final non-empty line.
	lines := strings.Split(out, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		fields := strings.Fields(lines[i])
		if len(fields) > 0 {
			return fields[0], nil
		}
	}
	return "", fmt.Errorf("df cache device: empty output")
}

// bpsOverridePath is the generated compose override that bakes device bandwidth
// caps into the streamer service definition.
func (r *Runner) bpsOverridePath() string {
	return filepath.Join(filepath.Dir(r.ComposeFile), "build", "bps-override.yaml")
}

// ApplyCacheThrottle caps the streamer cache device's read/write bandwidth by
// generating a compose override (blkio_config device_*_bps) that the next
// streamer recreate picks up. docker update cannot change device bps on a
// running container, so the cap has to live in the container's config; the
// per-cell restarts make baking it into an override equivalent to applying it.
// A 0 on either axis removes that cap. Requires the streamer up only to
// discover the device.
//
// CAVEAT: blkio device caps are routinely ignored by Docker Desktop / lima
// kernels; the probe measures (over a recreated container + direct-I/O write)
// whether this host honors them, and affected cells then carry a
// cap:device-bps note.
func (r *Runner) ApplyCacheThrottle(ctx context.Context, writeMBs, readMBs int) error {
	var b strings.Builder
	b.WriteString("services:\n  streamer:\n")
	if writeMBs != 0 || readMBs != 0 {
		// Only a real cap needs the device; clearing (below) must work even
		// when the container is down after a failed cap recreate.
		dev, err := r.cacheDevice(ctx)
		if err != nil {
			return err
		}
		// A tmpfs cache (-ram) has no block device behind it: "tmpfs" as a
		// device path is what the daemon refuses with "stat tmpfs: no such
		// file or directory", several steps away from the cause.
		if !strings.HasPrefix(dev, "/") {
			return fmt.Errorf("cache is on %s, which has no block device to cap (-ram)", dev)
		}
		b.WriteString("    blkio_config:\n")
		if writeMBs > 0 {
			fmt.Fprintf(&b, "      device_write_bps:\n        - path: %s\n          rate: '%dmb'\n", dev, writeMBs)
		}
		if readMBs > 0 {
			fmt.Fprintf(&b, "      device_read_bps:\n        - path: %s\n          rate: '%dmb'\n", dev, readMBs)
		}
	}
	path := r.bpsOverridePath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("mkdir bps override: %w", err)
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		return fmt.Errorf("write bps override: %w", err)
	}
	// Make sure the next compose invocation carries it (idempotent).
	for _, o := range r.Override {
		if o == path {
			return nil
		}
	}
	r.Override = append(r.Override, path)
	return nil
}

// streamerCmd runs one shell command inside the streamer container and returns
// its combined output.
func (r *Runner) streamerCmd(ctx context.Context, sh string) (string, error) {
	return r.composeExec(ctx, "streamer", sh)
}

// newsCmd runs one shell command inside the news server container and returns
// its combined output.
func (r *Runner) newsCmd(ctx context.Context, sh string) (string, error) {
	return r.composeExec(ctx, "news", sh)
}

// composeExec runs one shell command inside the given compose service container
// and returns its combined output.
func (r *Runner) composeExec(ctx context.Context, service, sh string) (string, error) {
	cmd := composeCmd(ctx, r, "exec", "-T", service, "sh", "-c", sh)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%s %q: %w: %s", service, sh, err, strings.TrimSpace(out.String()))
	}
	return strings.TrimSpace(out.String()), nil
}
