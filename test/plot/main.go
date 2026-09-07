// Command plot renders a summary CSV as one SVG of small multiples: a panel per
// test, a series per remaining sweep axis combination, and a legend in its own
// column beside the panels rather than on top of them.
//
// Every series gets its own colour, dash pattern and marker, so two lines that
// cross are still told apart in print and by a colour-blind reader. SVG, so it
// needs no plotting dependency and stays readable at any zoom.
package main

import (
	"encoding/csv"
	"flag"
	"fmt"
	"math"
	"os"
	"slices"
	"strconv"
	"strings"
)

func main() {
	in := flag.String("in", "test/build/summary.csv", "summary CSV to plot")
	out := flag.String("out", "test/build/summary.svg", "SVG to write")
	xCol := flag.String("x", "", "column on the x axis, '' = the sweep axis with the most values")
	yCol := flag.String("y", "mean_mibs", "column on the y axis")
	errCol := flag.String("err", "ci95_pm", "column drawn as an error bar around y, '' = none")
	facetCol := flag.String("facet", "test", "column to give each panel to")
	width := flag.Int("width", 1100, "SVG width in px")
	panelH := flag.Int("panel-height", 260, "panel height in px")
	logY := flag.Bool("logy", false, "logarithmic y axis, for series that span orders of magnitude")
	bandCol := flag.String("band", "", "two columns 'lo,hi' shaded behind each line, e.g. p5_mibs,p95_mibs; '' = none")
	y2Col := flag.String("y2", "", "second column drawn dashed against a right-hand axis, e.g. mean_peak_rss_mib; '' = none")
	flag.Parse()

	rows, header, err := readCSV(*in)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	c, err := newChart(rows, header, options{
		x: *xCol, y: *yCol, err: *errCol, facet: *facetCol, band: *bandCol, y2: *y2Col,
		width: *width, panelH: *panelH, logY: *logY,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := os.WriteFile(*out, []byte(c.render()), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("wrote %d panels, %d series to %s\n", len(c.facets), len(c.series), *out)
}

type options struct {
	x, y, err, facet string
	band             string // "lo,hi" column pair, shaded behind the line
	y2               string // second measure, dashed against a right-hand axis
	width, panelH    int
	logY             bool
}

// bandCols splits the -band pair, empty where it is off or malformed.
func (o options) bandCols() (string, string) {
	lo, hi, ok := strings.Cut(o.band, ",")
	if !ok {
		return "", ""
	}
	return lo, hi
}

// readCSV returns the rows as column-keyed maps plus the header order.
func readCSV(path string) ([]map[string]string, []string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	recs, err := csv.NewReader(f).ReadAll()
	if err != nil {
		return nil, nil, fmt.Errorf("read %s: %w", path, err)
	}
	if len(recs) < 2 {
		return nil, nil, fmt.Errorf("%s has no data rows", path)
	}
	head := recs[0]
	rows := make([]map[string]string, 0, len(recs)-1)
	for _, r := range recs[1:] {
		m := map[string]string{}
		for i, h := range head {
			if i < len(r) {
				m[h] = r[i]
			}
		}
		rows = append(rows, m)
	}
	return rows, head, nil
}

// point is one plotted observation: an x category, a value and its error bar.
type point struct {
	x       string
	y, ebar float64
	lo, hi  float64 // band bounds; equal means no band
	y2      float64 // right-axis value; NaN where there is none
}

type series struct {
	label  string
	facet  string
	points []point
}

type chart struct {
	opt     options
	facets  []string // panel order
	xVals   []string // shared x categories, in plot order
	series  []series
	styleOf map[string]style // label -> style, shared across panels
}

// style is grouped rather than sequential: the first series axis picks the
// colour and the rest the dash and marker, so a two-axis chart reads as
// families of lines instead of one arbitrary sequence.
type style struct{ col, minor int }

// statStart is the first column of the statistics block; everything left of it
// identifies the group (test, phase and the sweep axes) and is what a series or
// a facet can be keyed on.
const statStart = "count"

func newChart(rows []map[string]string, header []string, opt options) (*chart, error) {
	keyCols := header
	if i := slices.Index(header, statStart); i > 0 {
		keyCols = header[:i]
	}
	if !slices.Contains(header, opt.y) {
		return nil, fmt.Errorf("no column %q in the CSV", opt.y)
	}
	if opt.err != "" && !slices.Contains(header, opt.err) {
		opt.err = ""
	}
	if lo, hi := opt.bandCols(); lo != "" && (!slices.Contains(header, lo) || !slices.Contains(header, hi)) {
		return nil, fmt.Errorf("no columns %q and %q in the CSV", lo, hi)
	}
	if opt.y2 != "" && !slices.Contains(header, opt.y2) {
		return nil, fmt.Errorf("no column %q in the CSV", opt.y2)
	}

	// Only a column that actually varies distinguishes anything; a constant one
	// would add a legend entry that says the same thing on every line.
	varying := func(col string) bool {
		seen := map[string]bool{}
		for _, r := range rows {
			seen[r[col]] = true
		}
		return len(seen) > 1
	}
	if opt.x == "" {
		best, bestN := "", 1
		for _, c := range keyCols {
			if c == opt.facet {
				continue
			}
			n := len(distinct(rows, c))
			if n > bestN {
				best, bestN = c, n
			}
		}
		if best == "" {
			return nil, fmt.Errorf("nothing varies across the rows; pass -x explicitly")
		}
		opt.x = best
	}

	var seriesCols []string
	for _, c := range keyCols {
		if c != opt.x && c != opt.facet && varying(c) {
			seriesCols = append(seriesCols, c)
		}
	}

	c := &chart{opt: opt, xVals: sortedCategories(distinct(rows, opt.x)), styleOf: map[string]style{}}
	c.facets = sortedCategories(distinct(rows, opt.facet))

	index := map[string]int{} // facet+label -> position in c.series
	for _, r := range rows {
		y, err := strconv.ParseFloat(r[opt.y], 64)
		if err != nil {
			continue // a group with nothing measured has no point to draw
		}
		var e float64
		if opt.err != "" {
			e, _ = strconv.ParseFloat(r[opt.err], 64)
		}
		label := seriesLabel(r, seriesCols)
		key := r[opt.facet] + "\x00" + label
		i, ok := index[key]
		if !ok {
			i = len(c.series)
			index[key] = i
			c.series = append(c.series, series{label: label, facet: r[opt.facet]})
		}
		pt := point{x: r[opt.x], y: y, ebar: e, y2: math.NaN()}
		if opt.y2 != "" {
			pt.y2, _ = strconv.ParseFloat(r[opt.y2], 64)
		}
		if lo, hi := opt.bandCols(); lo != "" {
			pt.lo, _ = strconv.ParseFloat(r[lo], 64)
			pt.hi, _ = strconv.ParseFloat(r[hi], 64)
		}
		c.series[i].points = append(c.series[i].points, pt)
	}
	// One style per label, so the same series means the same thing in every
	// panel of the chart.
	var major []string
	if len(seriesCols) > 0 {
		major = sortedCategories(distinct(rows, seriesCols[0]))
	}
	minors := map[string]int{}
	for _, s := range c.series {
		if _, ok := c.styleOf[s.label]; ok {
			continue
		}
		head, rest, _ := strings.Cut(s.label, " ")
		m, ok := minors[rest]
		if !ok {
			m = len(minors)
			minors[rest] = m
		}
		col := 0
		if len(major) > 0 {
			col = max(slices.Index(major, strings.TrimPrefix(head, seriesCols[0]+"=")), 0)
		}
		c.styleOf[s.label] = style{col: col, minor: m}
	}
	return c, nil
}

func seriesLabel(r map[string]string, cols []string) string {
	if len(cols) == 0 {
		return "all"
	}
	parts := make([]string, 0, len(cols))
	for _, c := range cols {
		parts = append(parts, c+"="+r[c])
	}
	return strings.Join(parts, " ")
}

func distinct(rows []map[string]string, col string) []string {
	seen := map[string]bool{}
	var out []string
	for _, r := range rows {
		if v := r[col]; !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

// sortedCategories orders values numerically where they all are numbers and
// lexically otherwise, so a latency axis reads 0,50,100 rather than 0,100,50.
func sortedCategories(vals []string) []string {
	nums := make([]float64, len(vals))
	allNum := true
	for i, v := range vals {
		n, err := strconv.ParseFloat(v, 64)
		if err != nil {
			allNum = false
			break
		}
		nums[i] = n
	}
	out := slices.Clone(vals)
	if allNum {
		slices.SortFunc(out, func(a, b string) int {
			x, _ := strconv.ParseFloat(a, 64)
			y, _ := strconv.ParseFloat(b, 64)
			return cmpFloat(x, y)
		})
		return out
	}
	slices.Sort(out)
	return out
}

func cmpFloat(a, b float64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// The palette is Okabe-Ito, which stays distinguishable under every common form
// of colour blindness; dash and marker vary with it so the series are still
// separable in greyscale and where two lines overlap exactly.
var (
	colours = []string{"#0072B2", "#D55E00", "#009E73", "#CC79A7", "#E69F00", "#56B4E9", "#F0E442", "#000000"}
	dashes  = []string{"", "7 3", "2 3", "9 3 2 3", "1 4", "5 2 1 2"}
	markers = []string{"circle", "square", "diamond", "triangle", "cross"}
)

// layout constants: the legend lives in its own column to the right of every
// panel, which is what keeps it off the lines.
const (
	padL, padR   = 62.0, 16.0
	padR2        = 52.0 // extra right margin the second axis needs
	padT, padB   = 52.0, 46.0
	legendW      = 250.0
	panelGap     = 26.0
	markerR      = 3.5
	legendLineLn = 26.0
	legendLineH  = 13.0
)

func (c *chart) render() string {
	plotW := float64(c.opt.width) - legendW - padL - padR
	if c.opt.y2 != "" {
		plotW -= padR2 // room for the right-hand axis and its title
	}
	panelH := float64(c.opt.panelH)
	height := padT + (panelH+panelGap)*float64(len(c.facets)) + padB

	var b strings.Builder
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" width="%d" height="%.0f" viewBox="0 0 %d %.0f" font-family="system-ui,sans-serif" font-size="11">`,
		c.opt.width, height, c.opt.width, height)
	fmt.Fprint(&b, `<rect width="100%" height="100%" fill="#ffffff"/>`)
	title := fmt.Sprintf("%s by %s", c.opt.y, c.opt.x)
	if c.opt.y2 != "" {
		title += fmt.Sprintf(", %s dashed on the right", c.opt.y2)
	}
	fmt.Fprintf(&b, `<text x="%.1f" y="20" font-size="14" font-weight="600">%s</text>`, padL, esc(title))

	for i, f := range c.facets {
		top := padT + (panelH+panelGap)*float64(i)
		c.panel(&b, f, padL, top, plotW, panelH)
	}
	c.legend(&b, float64(c.opt.width)-legendW+8, padT)
	fmt.Fprint(&b, "</svg>")
	return b.String()
}

// panel draws one facet: axes, gridlines and every series of that facet.
func (c *chart) panel(b *strings.Builder, facet string, x0, y0, w, h float64) {
	maxY := 0.0
	for _, s := range c.series {
		if s.facet != facet {
			continue
		}
		for _, p := range s.points {
			maxY = math.Max(maxY, math.Max(p.y+p.ebar, p.hi))
		}
	}
	step, top := niceScale(maxY)
	ticks, yPos := c.yAxis(y0, h, step, top)

	if c.opt.facet != "" {
		fmt.Fprintf(b, `<text x="%.1f" y="%.1f" font-weight="600">%s</text>`, x0, y0-8, esc(c.opt.facet+"="+facet))
	}
	// axis titles, so a panel says what it plots without its caption
	fmt.Fprintf(b, `<text x="%.1f" y="%.1f" text-anchor="middle" fill="#555" transform="rotate(-90 %.1f %.1f)">%s</text>`,
		x0-44, y0+h/2, x0-44, y0+h/2, esc(c.opt.y))
	if facet == c.facets[len(c.facets)-1] { // only the bottom panel has room under it
		fmt.Fprintf(b, `<text x="%.1f" y="%.1f" text-anchor="middle" fill="#555">%s</text>`, x0+w/2, y0+h+32, esc(c.opt.x))
	}
	fmt.Fprintf(b, `<rect x="%.1f" y="%.1f" width="%.1f" height="%.1f" fill="#fafafa" stroke="#ccc"/>`, x0, y0, w, h)

	// Horizontal gridlines with their value, so a reader never has to count.
	for _, v := range ticks {
		y := yPos(v)
		fmt.Fprintf(b, `<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" stroke="#e4e4e4"/>`, x0, y, x0+w, y)
		fmt.Fprintf(b, `<text x="%.1f" y="%.1f" text-anchor="end" fill="#555">%s</text>`, x0-6, y+3.5, trimNum(v))
	}

	// The right-hand axis is a second measure on its own scale, so a cost can be
	// read against the throughput it bought.
	y2Pos := func(float64) float64 { return 0 }
	if c.opt.y2 != "" {
		max2 := 0.0
		for _, s := range c.series {
			if s.facet == facet {
				for _, p := range s.points {
					if !math.IsNaN(p.y2) {
						max2 = math.Max(max2, p.y2)
					}
				}
			}
		}
		step2, top2 := niceScale(max2)
		var ticks2 []float64
		ticks2, y2Pos = c.yAxis(y0, h, step2, top2)
		for _, v := range ticks2 {
			fmt.Fprintf(b, `<text x="%.1f" y="%.1f" fill="#555">%s</text>`, x0+w+6, y2Pos(v)+3.5, trimNum(v))
		}
		fmt.Fprintf(b, `<text x="%.1f" y="%.1f" text-anchor="middle" fill="#555" transform="rotate(-90 %.1f %.1f)">%s</text>`,
			x0+w+44, y0+h/2, x0+w+44, y0+h/2, esc(c.opt.y2))
	}

	// Categorical x: evenly spaced slots, one per distinct value, so a crowded
	// numeric axis never collapses two points onto each other.
	n := len(c.xVals)
	slot := func(v string) float64 {
		i := slices.Index(c.xVals, v)
		if n == 1 {
			return x0 + w/2
		}
		return x0 + w*float64(i)/float64(n-1)*0.9 + w*0.05
	}
	for _, v := range c.xVals {
		fmt.Fprintf(b, `<text x="%.1f" y="%.1f" text-anchor="middle" fill="#555">%s</text>`,
			slot(v), y0+h+14, esc(v))
	}

	for _, s := range c.series {
		if s.facet != facet {
			continue
		}
		st := c.styleOf[s.label]
		col := colours[st.col%len(colours)]
		dash := dashes[st.minor%len(dashes)]
		mark := markers[st.minor%len(markers)]

		pts := slices.Clone(s.points)
		slices.SortFunc(pts, func(a, b point) int {
			return cmpFloat(slot(a.x), slot(b.x))
		})
		var path strings.Builder
		for i, p := range pts {
			x := slot(p.x)
			y := yPos(p.y)
			if i == 0 {
				fmt.Fprintf(&path, "M%.1f %.1f", x, y)
			} else {
				fmt.Fprintf(&path, "L%.1f %.1f", x, y)
			}
		}
		if lo, _ := c.opt.bandCols(); lo != "" {
			var area strings.Builder
			for i, p := range pts {
				verb := "L"
				if i == 0 {
					verb = "M"
				}
				fmt.Fprintf(&area, "%s%.1f %.1f", verb, slot(p.x), yPos(p.hi))
			}
			for i := len(pts) - 1; i >= 0; i-- {
				fmt.Fprintf(&area, "L%.1f %.1f", slot(pts[i].x), yPos(pts[i].lo))
			}
			fmt.Fprintf(b, `<path d="%sZ" fill="%s" fill-opacity="0.12" stroke="none"/>`, area.String(), col)
		}
		fmt.Fprintf(b, `<path d="%s" fill="none" stroke="%s" stroke-width="1.8" stroke-dasharray="%s"/>`,
			path.String(), col, dash)
		if c.opt.y2 != "" {
			var p2 strings.Builder
			for _, p := range pts {
				if math.IsNaN(p.y2) {
					continue
				}
				verb := "L"
				if p2.Len() == 0 {
					verb = "M"
				}
				fmt.Fprintf(&p2, "%s%.1f %.1f", verb, slot(p.x), y2Pos(p.y2))
			}
			fmt.Fprintf(b, `<path d="%s" fill="none" stroke="%s" stroke-width="1.2" stroke-dasharray="3 3" stroke-opacity="0.6"/>`,
				p2.String(), col)
		}

		for _, p := range pts {
			x := slot(p.x)
			y := yPos(p.y)
			if p.ebar > 0 {
				// Clamped at zero: a mean minus its interval is still a speed,
				// and a bar reaching below the axis is only ever a drawing bug.
				yl := yPos(p.y - p.ebar)
				yh := yPos(p.y + p.ebar)
				fmt.Fprintf(b, `<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" stroke="%s" stroke-width="1"/>`, x, yl, x, yh, col)
				fmt.Fprintf(b, `<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" stroke="%s" stroke-width="1"/>`, x-3, yl, x+3, yl, col)
				fmt.Fprintf(b, `<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" stroke="%s" stroke-width="1"/>`, x-3, yh, x+3, yh, col)
			}
			marker(b, mark, x, y, col)
		}
	}
}

// legend lists every series once, beside the panels.
func (c *chart) legend(b *strings.Builder, x, y float64) {
	labels := make([]string, 0, len(c.styleOf))
	for l := range c.styleOf {
		labels = append(labels, l)
	}
	slices.SortFunc(labels, func(a, b string) int {
		x, y := c.styleOf[a], c.styleOf[b]
		if x.col != y.col {
			return x.col - y.col
		}
		return x.minor - y.minor
	})

	fmt.Fprintf(b, `<text x="%.1f" y="%.1f" font-weight="600">series</text>`, x, y-8)
	ly := y + 8
	for _, l := range labels {
		st := c.styleOf[l]
		col := colours[st.col%len(colours)]
		dash := dashes[st.minor%len(dashes)]
		mark := markers[st.minor%len(markers)]
		fmt.Fprintf(b, `<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" stroke="%s" stroke-width="1.8" stroke-dasharray="%s"/>`,
			x, ly, x+legendLineLn, ly, col, dash)
		marker(b, mark, x+legendLineLn/2, ly, col)
		for i, line := range wrapLabel(l) {
			fmt.Fprintf(b, `<text x="%.1f" y="%.1f" fill="#333">%s</text>`,
				x+legendLineLn+8, ly+3.5+float64(i)*legendLineH, esc(line))
		}
		ly += math.Max(float64(len(wrapLabel(l)))*legendLineH, 20) + 4
	}
}

// wrapLabel breaks a series label at its spaces so a long one stays inside the
// legend column instead of running off the image. A label is key=value pairs,
// so the pieces are what it splits on.
func wrapLabel(l string) []string {
	const perLine = 30 // characters that fit the legend column at 11px
	var lines []string
	cur := ""
	for _, part := range strings.Fields(l) {
		switch {
		case cur == "":
			cur = part
		case len(cur)+1+len(part) <= perLine:
			cur += " " + part
		default:
			lines = append(lines, cur)
			cur = part
		}
	}
	if cur != "" {
		lines = append(lines, cur)
	}
	return lines
}

func marker(b *strings.Builder, kind string, x, y float64, col string) {
	switch kind {
	case "square":
		fmt.Fprintf(b, `<rect x="%.1f" y="%.1f" width="%.1f" height="%.1f" fill="%s"/>`,
			x-markerR, y-markerR, 2*markerR, 2*markerR, col)
	case "diamond":
		fmt.Fprintf(b, `<polygon points="%.1f,%.1f %.1f,%.1f %.1f,%.1f %.1f,%.1f" fill="%s"/>`,
			x, y-markerR-1, x+markerR+1, y, x, y+markerR+1, x-markerR-1, y, col)
	case "triangle":
		fmt.Fprintf(b, `<polygon points="%.1f,%.1f %.1f,%.1f %.1f,%.1f" fill="%s"/>`,
			x, y-markerR-1, x+markerR+1, y+markerR, x-markerR-1, y+markerR, col)
	case "cross":
		fmt.Fprintf(b, `<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" stroke="%s" stroke-width="1.8"/>`,
			x-markerR, y-markerR, x+markerR, y+markerR, col)
		fmt.Fprintf(b, `<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" stroke="%s" stroke-width="1.8"/>`,
			x-markerR, y+markerR, x+markerR, y-markerR, col)
	default:
		fmt.Fprintf(b, `<circle cx="%.1f" cy="%.1f" r="%.1f" fill="%s"/>`, x, y, markerR, col)
	}
}

// yAxis returns the gridline values and the value-to-pixel mapping of the y
// axis. A log axis needs a floor rather than a zero, so it starts a decade
// below the smallest gridline a linear axis would have drawn; anything at or
// under it is pinned to the bottom.
func (c *chart) yAxis(y0, h, step, top float64) ([]float64, func(float64) float64) {
	if !c.opt.logY {
		var ticks []float64
		for v := 0.0; v <= top+1e-9; v += step {
			ticks = append(ticks, v)
		}
		return ticks, func(v float64) float64 {
			return y0 + h - h*math.Min(math.Max(v, 0), top)/top
		}
	}

	lo := math.Pow(10, math.Floor(math.Log10(step))-1)
	var ticks []float64
	for v := lo; v <= top*(1+1e-9); v *= 10 {
		for _, m := range []float64{1, 2, 5} {
			if t := v * m; t >= lo && t <= top*(1+1e-9) {
				ticks = append(ticks, t)
			}
		}
	}
	span := math.Log10(top) - math.Log10(lo)
	return ticks, func(v float64) float64 {
		if v <= lo {
			return y0 + h
		}
		return y0 + h - h*(math.Log10(math.Min(v, top))-math.Log10(lo))/span
	}
}

// niceScale picks a 1/2/5-times-power-of-ten gridline step targeting about five
// lines, and the axis top that step lands on.
func niceScale(maxY float64) (step, top float64) {
	if maxY <= 0 {
		return 1, 1
	}
	raw := maxY / 5
	mag := math.Pow(10, math.Floor(math.Log10(raw)))
	for _, m := range []float64{1, 2, 5, 10} {
		if step = m * mag; step >= raw {
			break
		}
	}
	return step, math.Ceil(maxY/step) * step
}

// trimNum formats a gridline value without trailing zeroes.
func trimNum(v float64) string {
	s := strconv.FormatFloat(v, 'f', 2, 64)
	s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	if s == "" || s == "-" {
		return "0"
	}
	return s
}

var escaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")

func esc(s string) string { return escaper.Replace(s) }
