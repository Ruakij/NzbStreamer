package harness

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"git.ruekov.eu/ruakij/nzbStreamer/test/readclient"
)

// MatrixChunk is the byte granularity read patterns operate at, passed through
// to readclient.Plan for every cell.
const MatrixChunk int64 = 1 << 20 // 1 MiB

// AppVar is one application env var axis the sweep controls: the harness passes
// each option value through verbatim to the streamer's container env. A name
// must match ^[A-Z][A-Z0-9_]*$ and names within one matrix must be unique.
type AppVar struct {
	Name   string   // env var name as passed to the container, e.g. USENET_MAX_CONN
	Values []string // verbatim option values; "" means "omit the var" so the application's own default applies
}

// TestEnvConfig are the harness's own rig knobs: the read pattern the product
// serves and the conditions imposed on the rig (news-link latency, link and
// cache-device caps, entropy seed). These always carry harness-held defaults,
// applied at cell construction, because they are harness concepts — the product
// has no such settings.
type TestEnvConfig struct {
	ReadTypes       []string
	LatencyMs       []int
	JitterMs        []int
	LineSpeed       []int
	LineJitter      []int
	CacheWriteSpeed []int
	CacheReadSpeed  []int
	Seeds           []int64
}

// Matrix is the cartesian sweep applied to every selected test: application env
// var axes (written to env only when explicitly swept, else the app default
// applies) plus the harness's rig knobs (always carried by the harness and
// applied at cell construction).
//
// Matrix{} (zero value) is the valid default: -matrix ” yields one cell with no
// app env vars and the harness's default rig conditions.
type Matrix struct {
	App     []AppVar
	TestEnv TestEnvConfig
}

// Cell is one point of the sweep: a single combination of every axis. AppEnv
// holds the resolved application env this cell runs with, one entry per swept
// app var axis with a non-empty option; a var whose option was "" (or that was
// never swept) has no entry, so the application's own default applies.
type Cell struct {
	ReadType string
	AppEnv   map[string]string

	LatencyMs, JitterMs             int
	LineSpeed, LineJitter           int
	CacheWriteSpeed, CacheReadSpeed int
	Seed                            int64
}

// axis is one sweep axis: its option count and a setter writing the option at
// index i into a Cell. The axes are laid out in the canonical sweep order
// (readtype outermost, seed innermost).
type axis struct {
	n   int
	set func(c *Cell, i int)
}

// sortedApp returns the app var axes sorted ascending by Name: the canonical
// sweep order and CSV column order for app vars.
func (m Matrix) sortedApp() []AppVar {
	app := slices.Clone(m.App)
	slices.SortStableFunc(app, func(a, b AppVar) int { return strings.Compare(a.Name, b.Name) })
	return app
}

func (m Matrix) axes() []axis {
	// App axes: empty -> n=1 with a no-op setter so the app's own default
	// applies (AppEnv stays nil). Test-env axes: empty -> n=1 with a setter
	// writing the harness default.
	te := m.TestEnv
	axes := []axis{valueAxis(te.ReadTypes, "sequential", func(c *Cell, v string) { c.ReadType = v })}
	for _, v := range m.sortedApp() {
		axes = append(axes, axis{lenOr1(v.Values), func(c *Cell, i int) {
			if len(v.Values) == 0 || v.Values[i] == "" {
				return
			}
			if c.AppEnv == nil {
				c.AppEnv = map[string]string{}
			}
			c.AppEnv[v.Name] = v.Values[i]
		}})
	}
	return append(axes,
		valueAxis(te.LatencyMs, 0, func(c *Cell, v int) { c.LatencyMs = v }),
		valueAxis(te.JitterMs, 0, func(c *Cell, v int) { c.JitterMs = v }),
		valueAxis(te.LineSpeed, 0, func(c *Cell, v int) { c.LineSpeed = v }),
		valueAxis(te.LineJitter, 0, func(c *Cell, v int) { c.LineJitter = v }),
		valueAxis(te.CacheWriteSpeed, 0, func(c *Cell, v int) { c.CacheWriteSpeed = v }),
		valueAxis(te.CacheReadSpeed, 0, func(c *Cell, v int) { c.CacheReadSpeed = v }),
		valueAxis(te.Seeds, 1, func(c *Cell, v int64) { c.Seed = v }),
	)
}

// valueAxis is an axis over the values the matrix was given, or a single option
// carrying the harness default when it was given none.
func valueAxis[T any](vals []T, def T, set func(*Cell, T)) axis {
	return axis{lenOr1(vals), func(c *Cell, i int) {
		v := def
		if len(vals) > 0 {
			v = vals[i]
		}
		set(c, v)
	}}
}

// lenOr1 returns the axis option count, or 1 when the matrix leaves the axis
// empty (a single no-op / default option), so the product of ns is always >= 1.
func lenOr1[T any](s []T) int {
	if len(s) == 0 {
		return 1
	}
	return len(s)
}

// Cells is the full cartesian product of the matrix, readtype outermost and
// seed innermost so sequential sweeps of one axis read like prose. The product
// of per-axis counts is always >= 1 (every axis defaults to at least one
// option), so it always yields at least one cell.
func (m Matrix) Cells() []Cell {
	axes := m.axes()
	counts := make([]int, len(axes))
	total := 1
	for i, a := range axes {
		counts[i] = a.n
		total *= a.n
	}
	out := make([]Cell, 0, total)
	idx := make([]int, len(axes))
	for k := 0; k < total; k++ {
		var c Cell
		for i, a := range axes {
			a.set(&c, idx[i])
		}
		out = append(out, c)
		for i := len(idx) - 1; i >= 0; i-- { // seed innermost, so carry from the last axis
			idx[i]++
			if idx[i] < counts[i] {
				break
			}
			idx[i] = 0
		}
	}
	return out
}

// CellsPairwise returns a deterministic greedy pairwise (2-wise) covering of
// the matrix: every pair of options from every pair of axes appears together
// in at least one returned cell.
func (m Matrix) CellsPairwise() []Cell { return m.cellsN(2) }

// tuple is one t-tuple: t distinct axes, one option index each, sorted by axis
// index for canonical identity.
type tuple []val

type val struct{ ax, opt int }

// cellsN returns a deterministic greedy t-wise covering: every t-tuple of
// option values across t distinct axes appears in at least one returned cell.
//
// The candidate pool is only the combos the greedy constructs, never the full
// cartesian product, so the output stays small as axes multiply. Deterministic:
// unchanged input always yields unchanged output — tie-breaks are lowest axis
// index then lowest option index, and tuples are walked in fixed order. Safe
// with empty or single-option axes, and never returns zero cells when at least
// one axis has an option.
func (m Matrix) cellsN(t int) []Cell {
	axes := m.axes()
	// Only axes with at least one option participate in tuples; keep their
	// original indices so subset ordering stays canonical and stable.
	var base []int
	for i, a := range axes {
		if a.n > 0 {
			base = append(base, i)
		}
	}

	// Enumerate every t-tuple: choose t distinct axes (increasing), one option
	// each, deduped by (axis,option) set.
	var list []tuple
	keyOf := func(t tuple) string {
		s := make([]string, len(t))
		for i, v := range t {
			s[i] = fmt.Sprintf("%d:%d", v.ax, v.opt)
		}
		return strings.Join(s, ",")
	}
	seen := map[string]bool{}
	var rec func(start int, cur tuple)
	rec = func(start int, cur tuple) {
		if len(cur) == t {
			k := keyOf(cur)
			if !seen[k] {
				seen[k] = true
				list = append(list, append(tuple{}, cur...))
			}
			return
		}
		for i := start; i < len(base); i++ {
			if len(base)-i < t-len(cur) {
				break
			}
			ax := base[i]
			for o := 0; o < axes[ax].n; o++ {
				nc := make(tuple, len(cur), len(cur)+1)
				copy(nc, cur)
				rec(i+1, append(nc, val{ax, o}))
			}
		}
	}
	rec(0, nil)

	covered := make([]bool, len(list))
	cand := make([]int, len(axes)) // option index per axis, -1 while unassigned
	// countUncovered counts uncovered tuples involving axis ai, consistent with
	// the candidate so far, that would be covered by choosing option o there.
	countUncovered := func(ai, o int) int {
		cnt := 0
		for ti, done := range covered {
			if done {
				continue
			}
			t := list[ti]
			matches := false
			ok := true
			for _, v := range t {
				if v.ax == ai {
					if v.opt != o {
						ok = false
					}
					matches = true
				} else if cand[v.ax] >= 0 && cand[v.ax] != v.opt {
					ok = false
				}
			}
			if matches && ok {
				cnt++
			}
		}
		return cnt
	}

	var out []Cell
	for {
		// Seed with the first uncovered tuple.
		seed := -1
		for i, done := range covered {
			if !done {
				seed = i
				break
			}
		}
		if seed == -1 {
			break
		}
		for i := range cand {
			cand[i] = -1
		}
		for _, v := range list[seed] {
			cand[v.ax] = v.opt
		}
		// Fill the remaining axes one at a time, each step choosing the
		// (axis, option) covering the most still-uncovered tuples; ties go to
		// lowest axis index then lowest option index (strictly-greater replace).
		for {
			bestAx, bestOpt, bestCnt := -1, -1, -1
			for _, ax := range base {
				if cand[ax] >= 0 {
					continue
				}
				for o := 0; o < axes[ax].n; o++ {
					if cnt := countUncovered(ax, o); cnt > bestCnt {
						bestCnt, bestAx, bestOpt = cnt, ax, o
					}
				}
			}
			if bestAx == -1 {
				break
			}
			cand[bestAx] = bestOpt
		}
		var c Cell
		for _, ax := range base {
			axes[ax].set(&c, cand[ax])
		}
		out = append(out, c)
		for ti, done := range covered {
			if done {
				continue
			}
			covered[ti] = comboCovers(cand, list[ti])
		}
	}

	// With fewer distinct axes than t there are no tuples; still yield a single
	// cell so a non-empty matrix never produces an empty sweep.
	if len(out) == 0 && len(base) > 0 {
		var c Cell
		for _, ax := range base {
			axes[ax].set(&c, 0)
		}
		out = append(out, c)
	}
	return out
}

// comboCovers reports whether a fully-assigned candidate covers a tuple.
func comboCovers(cand []int, t tuple) bool {
	for _, v := range t {
		if cand[v.ax] != v.opt {
			return false
		}
	}
	return true
}

// RigDefaults are the application settings every cell runs with unless it
// sweeps them: the two whose application default is wrong for a benchmark rig
// rather than wrong in general - an unbounded cache would fill the volume, and
// per-read logging is measurable work. Everything else is left to the
// application's own default, so a run with no axes measures the product as it
// ships. compose.yaml deliberately sets neither, since a value there would win
// over the env file and no sweep of them could take effect.
var RigDefaults = map[string]string{
	"LOGLEVEL":       "WARN",
	"CACHE_MAX_SIZE": "2147483648",
}

// Env is the container environment a cell resolves to, written into the env
// file the compose stack reads on streamer restart: the rig defaults with the
// cell's own axes over them. The application env vars in AppEnv are emitted
// only when the matching axis was swept with a non-empty option, so otherwise
// the rig default, or failing that the application's own, applies. Values pass
// through verbatim.
func (c Cell) Env() map[string]string {
	env := maps.Clone(RigDefaults)
	for k, v := range c.AppEnv {
		env[k] = v
	}
	return env
}

// HeaderFields are the column names of the sweep axes: the read type, then each
// app var axis under its own env-var name (sorted), then the rig axes. The CSV
// writers append their outcome columns to this.
func (m Matrix) HeaderFields() []string {
	cols := []string{"readtype"}
	for _, v := range m.sortedApp() {
		cols = append(cols, v.Name)
	}
	return append(cols,
		"latency", "jitter", "linespeed", "linejitter",
		"cachewritespeed", "cachereadspeed", "seed")
}

// RowFields is this cell's sweep axes as values matching HeaderFields. An app
// var the cell does not set renders as the empty field, signalling the
// application default applies.
func (m Matrix) RowFields(c Cell) []string {
	vals := []string{c.ReadType}
	for _, v := range m.sortedApp() {
		vals = append(vals, c.AppEnv[v.Name])
	}
	return append(vals,
		strconv.Itoa(c.LatencyMs),
		strconv.Itoa(c.JitterMs),
		strconv.Itoa(c.LineSpeed),
		strconv.Itoa(c.LineJitter),
		strconv.Itoa(c.CacheWriteSpeed),
		strconv.Itoa(c.CacheReadSpeed),
		strconv.FormatInt(c.Seed, 10),
	)
}

// Header and Row are the same two things as one comma-joined string, which is
// what identifies a cell in a progress line, a profile filename and a summary
// group key.
func (m Matrix) Header() string    { return strings.Join(m.HeaderFields(), ",") }
func (m Matrix) Row(c Cell) string { return strings.Join(m.RowFields(c), ",") }

// ParseMatrix turns a spec string into a Matrix of rig keys. Keys are
// space-separated "key=v1,v2" tokens; a missing key means "leave default": the
// harness default applied at cell build. Every key is an integer list.
//
// Example: "readtype=seq,random jitter=0,5 seed=1,2".
func ParseMatrix(spec string) (Matrix, error) {
	m := Matrix{}
	if strings.TrimSpace(spec) == "" {
		return m, nil
	}
	for _, kv := range strings.Fields(spec) {
		parts := strings.SplitN(kv, "=", 2)
		if len(parts) != 2 {
			return m, fmt.Errorf("matrix: want key=v1,v2, got %q", kv)
		}
		key, vals := parts[0], strings.Split(parts[1], ",")
		var err error
		switch key {
		case "readtype":
			for i, rt := range vals {
				if rt == "seq" { // common alias for the full cover
					vals[i] = "sequential"
				}
				if !readclient.KnownPattern(vals[i]) {
					return m, fmt.Errorf("matrix readtype: unknown pattern %q, want one of %s",
						vals[i], strings.Join(readclient.ReadPatterns, ", "))
				}
			}
			m.TestEnv.ReadTypes = vals
		case "latency":
			m.TestEnv.LatencyMs, err = ints(vals)
		case "jitter":
			m.TestEnv.JitterMs, err = ints(vals)
		case "linespeed":
			m.TestEnv.LineSpeed, err = ints(vals)
		case "linejitter":
			m.TestEnv.LineJitter, err = ints(vals)
		case "cachewritespeed":
			m.TestEnv.CacheWriteSpeed, err = ints(vals)
		case "cachereadspeed":
			m.TestEnv.CacheReadSpeed, err = ints(vals)
		case "seed":
			m.TestEnv.Seeds, err = ints64(vals)
		default:
			return m, fmt.Errorf("matrix: unknown key %q", key)
		}
		if err != nil {
			return m, fmt.Errorf("matrix %s: %w", key, err)
		}
	}
	return m, nil
}

// ParseAppEnv parses the -app-env spec into app var axes: space-separated
// "NAME=v1,v2" tokens (same grammar as ParseMatrix), where NAME is any env var
// name and values pass through verbatim (whitespace-trimmed). A name must match
// ^[A-Z][A-Z0-9_]*$ and be unique; a value must not contain a newline (the env
// file is line-based). An empty value ("NAME=,4" -> ["","4"]) means "omit the
// var for that cell" so the application's own default applies. Return a
// non-nil, non-empty slice on success, an error otherwise.
func ParseAppEnv(spec string) ([]AppVar, error) {
	if strings.TrimSpace(spec) == "" {
		return nil, nil
	}
	// The env file is line-based, so a newline in any value would inject a fake
	// env line. strings.Fields splits on whitespace, so catch newlines here
	// before the tokens lose them.
	if strings.ContainsRune(spec, '\n') {
		return nil, fmt.Errorf("app-env: values must not contain newlines")
	}
	seen := map[string]bool{}
	var vars []AppVar
	for _, kv := range strings.Fields(spec) {
		parts := strings.SplitN(kv, "=", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("app-env: want NAME=v1,v2, got %q", kv)
		}
		name, vals := parts[0], strings.Split(parts[1], ",")
		if !appEnvNameRE.MatchString(name) {
			return nil, fmt.Errorf("app-env: invalid env var name %q", name)
		}
		if seen[name] {
			return nil, fmt.Errorf("app-env: duplicate env var %q", name)
		}
		seen[name] = true
		values := trimmed(vals)
		// Tokens are space-separated and commas separate one var's sweep values,
		// so a value spelling another assignment is a comma where a space was
		// meant. Taken literally it is a sweep whose later cells set garbage.
		for _, v := range values {
			if n, _, ok := strings.Cut(v, "="); ok && appEnvNameRE.MatchString(n) {
				return nil, fmt.Errorf("app-env: value %q of %q looks like another variable; separate variables with a space, values with a comma", v, name)
			}
		}
		vars = append(vars, AppVar{Name: name, Values: values})
	}
	return vars, nil
}

var appEnvNameRE = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)

func ints(vals []string) ([]int, error) {
	out := make([]int, len(vals))
	for i, v := range vals {
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			return nil, err
		}
		out[i] = n
	}
	return out, nil
}

func ints64(vals []string) ([]int64, error) {
	out := make([]int64, len(vals))
	for i, v := range vals {
		n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		if err != nil {
			return nil, err
		}
		out[i] = n
	}
	return out, nil
}

// trimmed trims whitespace around each raw value so string-valued axes (e.g.
// app env var values) pass through without surrounding spaces.
func trimmed(vals []string) []string {
	out := make([]string, len(vals))
	for i, v := range vals {
		out[i] = strings.TrimSpace(v)
	}
	return out
}
