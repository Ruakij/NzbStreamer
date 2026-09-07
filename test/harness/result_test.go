package harness

import (
	"strings"
	"testing"
	"time"
)

// A reading that moved no bytes (the damaged fixtures pass by failing to read)
// is counted but must not enter the speed samples, where it would drag the mean
// down and put p5 at zero.
func TestSummaryExcludesZeroByteReadings(t *testing.T) {
	m := Matrix{}
	cell := m.Cells()[0]
	res := []Result{
		{Test: "webdav-plain", Phase: "cold", Cell: cell, OK: true, Bytes: 1 << 20, Wall: time.Second, MiBs: 10},
		{Test: "webdav-plain", Phase: "cold", Cell: cell, OK: true, Bytes: 1 << 20, Wall: time.Second, MiBs: 20},
		{Test: "webdav-plain", Phase: "cold", Cell: cell, OK: true, Bytes: 0},
		{Test: "webdav-plain", Phase: "cold", Cell: cell, OK: false},
	}
	var b strings.Builder
	if err := WriteSummaryCSV(&b, m, res, nil); err != nil {
		t.Fatalf("WriteSummaryCSV: %v", err)
	}
	rows := strings.Split(strings.TrimSpace(b.String()), "\n")
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want header + 1", len(rows))
	}
	head, row := strings.Split(rows[0], ","), strings.Split(rows[1], ",")
	col := func(name string) string {
		for i, h := range head {
			if h == name {
				return row[i]
			}
		}
		t.Fatalf("no column %q in %v", name, head)
		return ""
	}
	if col("count") != "3" || col("failures") != "1" || col("measured") != "2" {
		t.Fatalf("count/failures/measured = %s/%s/%s, want 3/1/2",
			col("count"), col("failures"), col("measured"))
	}
	if col("mean_mibs") != "15.00" || col("p5_mibs") != "10.50" {
		t.Fatalf("mean/p5 = %s/%s, want 15.00/10.50", col("mean_mibs"), col("p5_mibs"))
	}
}

// Every pair of options from every pair of axes has to appear together in at
// least one cell, which is the only property the greedy promises.
func TestCellsPairwiseCoversEveryPair(t *testing.T) {
	m := Matrix{
		App: []AppVar{{Name: "USENET_MAX_CONN", Values: []string{"4", "8", "16"}}},
		TestEnv: TestEnvConfig{
			ReadTypes: []string{"sequential", "random", "tail"},
			LatencyMs: []int{0, 50, 100},
			Seeds:     []int64{1, 2, 3},
		},
	}
	cells := m.CellsPairwise()
	full := m.Cells()
	if len(cells) == 0 || len(cells) >= len(full) {
		t.Fatalf("pairwise returned %d cells, want between 1 and %d", len(cells), len(full))
	}

	// Rows of the cartesian product are the ground truth for what an axis pair
	// can be; every such pairing must show up in the pairwise rows too.
	seen := map[string]bool{}
	for _, c := range cells {
		f := m.RowFields(c)
		for i := range f {
			for j := i + 1; j < len(f); j++ {
				seen[pairKey(i, j, f[i], f[j])] = true
			}
		}
	}
	for _, c := range full {
		f := m.RowFields(c)
		for i := range f {
			for j := i + 1; j < len(f); j++ {
				if k := pairKey(i, j, f[i], f[j]); !seen[k] {
					t.Fatalf("pair %s uncovered", k)
				}
			}
		}
	}
}

func pairKey(i, j int, a, b string) string {
	return string(rune('a'+i)) + a + "/" + string(rune('a'+j)) + b
}
