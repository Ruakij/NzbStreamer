package harness

import (
	"reflect"
	"strings"
	"testing"
)

// sweepMatrix is a non-default matrix with a single-option and a multi-option
// app var plus rig axes, used to exercise the cartesian enumerator.
func sweepMatrix() Matrix {
	return Matrix{
		App: []AppVar{
			{Name: "USENET_MAX_CONN", Values: []string{"8", "16"}},
			{Name: "READAHEAD_SIZE", Values: []string{"32M"}},
		},
		TestEnv: TestEnvConfig{
			ReadTypes: []string{"sequential", "random"},
			LatencyMs: []int{0, 50},
			Seeds:     []int64{1, 2},
		},
	}
}

func TestCellsCartesianProduct(t *testing.T) {
	m := sweepMatrix()
	cells := m.Cells()
	want := 1
	for _, n := range []int{
		len(m.TestEnv.ReadTypes), len(m.App[0].Values), len(m.App[1].Values),
		len(m.TestEnv.LatencyMs), len(m.TestEnv.Seeds),
	} {
		want *= n
	}
	if len(cells) != want {
		t.Fatalf("Cells() returned %d cells, want %d", len(cells), want)
	}
	// Multi-option var USENET_MAX_CONN: each value appears exactly half the time.
	count := map[string]int{}
	for _, c := range cells {
		count[c.AppEnv["USENET_MAX_CONN"]]++
	}
	for _, v := range m.App[0].Values {
		if count[v] != want/len(m.App[0].Values) {
			t.Fatalf("value %s appears %d times, want %d", v, count[v], want/len(m.App[0].Values))
		}
	}
	// Single-option var present verbatim in every cell.
	for _, c := range cells {
		if c.AppEnv["READAHEAD_SIZE"] != "32M" {
			t.Fatalf("READAHEAD_SIZE = %q, want 32M", c.AppEnv["READAHEAD_SIZE"])
		}
	}
}

func TestEnv(t *testing.T) {
	m := Matrix{App: []AppVar{
		{Name: "USENET_MAX_CONN", Values: []string{"8"}},
		{Name: "READAHEAD_SIZE", Values: []string{"", "64M"}},
	}}
	cells := m.Cells()
	// cells[0]: conns=8, readahead omitted ("" option). cells[1]: both set.
	if env := cells[0].Env(); env["LOGLEVEL"] != "WARN" || env["USENET_MAX_CONN"] != "8" {
		t.Fatalf("Env = %v, want LOGLEVEL WARN and USENET_MAX_CONN 8", env)
	} else if _, ok := env["READAHEAD_SIZE"]; ok {
		t.Fatalf("Env should omit READAHEAD_SIZE when option is \"\", got %v", env)
	}
	if env := cells[1].Env(); env["USENET_MAX_CONN"] != "8" || env["READAHEAD_SIZE"] != "64M" {
		t.Fatalf("Env = %v, want both vars verbatim", env)
	}
}

func TestZeroMatrixDefaults(t *testing.T) {
	m := Matrix{}
	cells := m.Cells()
	if len(cells) != 1 {
		t.Fatalf("Cells() returned %d cells, want 1", len(cells))
	}
	c := cells[0]
	if c.ReadType != "sequential" {
		t.Fatalf("ReadType = %q, want sequential", c.ReadType)
	}
	if c.Seed != 1 {
		t.Fatalf("Seed = %d, want 1", c.Seed)
	}
	if c.LatencyMs != 0 {
		t.Fatalf("LatencyMs = %d, want 0", c.LatencyMs)
	}
	if len(c.AppEnv) != 0 || c.AppEnv != nil {
		t.Fatal("AppEnv should be nil on a zero matrix")
	}
	env := c.Env()
	if env["LOGLEVEL"] != "WARN" {
		t.Fatalf("LOGLEVEL = %q, want WARN", env["LOGLEVEL"])
	}
}

func TestParseMatrix(t *testing.T) {
	m, err := ParseMatrix("readtype=seq,random latency=50 seed=1,2")
	if err != nil {
		t.Fatalf("ParseMatrix: %v", err)
	}
	if !reflect.DeepEqual(m.TestEnv.ReadTypes, []string{"sequential", "random"}) {
		t.Fatalf("ReadTypes = %v, want [sequential random]", m.TestEnv.ReadTypes)
	}
	if !reflect.DeepEqual(m.TestEnv.LatencyMs, []int{50}) {
		t.Fatalf("LatencyMs = %v, want [50]", m.TestEnv.LatencyMs)
	}
	if !reflect.DeepEqual(m.TestEnv.Seeds, []int64{1, 2}) {
		t.Fatalf("Seeds = %v, want [1 2]", m.TestEnv.Seeds)
	}
	// An app key like conns=4 is no longer a rig key -> unknown key.
	if _, err := ParseMatrix("conns=4"); err == nil || !strings.Contains(err.Error(), "unknown key") {
		t.Fatalf("ParseMatrix(conns=4) err = %v, want unknown key", err)
	}
}

func TestParseAppEnv(t *testing.T) {
	vars, err := ParseAppEnv("USENET_MAX_CONN=8,16 READAHEAD_SIZE=32M")
	if err != nil {
		t.Fatalf("ParseAppEnv: %v", err)
	}
	want := []AppVar{
		{Name: "USENET_MAX_CONN", Values: []string{"8", "16"}},
		{Name: "READAHEAD_SIZE", Values: []string{"32M"}},
	}
	if !reflect.DeepEqual(vars, want) {
		t.Fatalf("ParseAppEnv = %v, want %v", vars, want)
	}

	// ""-option.
	vars, err = ParseAppEnv("USENET_MAX_CONN=,4")
	if err != nil {
		t.Fatalf("ParseAppEnv(,4): %v", err)
	}
	if !reflect.DeepEqual(vars[0].Values, []string{"", "4"}) {
		t.Fatalf("Values = %v, want [\"\" 4]", vars[0].Values)
	}

	// Empty spec -> no error, nil result.
	if vars, err = ParseAppEnv("   "); err != nil || vars != nil {
		t.Fatalf("ParseAppEnv(spaces) = %v, %v; want nil, nil", vars, err)
	}

	// Bad name.
	if _, err := ParseAppEnv("lower=1"); err == nil || !strings.Contains(err.Error(), "invalid env var name") {
		t.Fatalf("ParseAppEnv(lower=1) err = %v, want invalid name", err)
	}
	// Duplicate name.
	if _, err := ParseAppEnv("READAHEAD_SIZE=32M READAHEAD_SIZE=64M"); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("ParseAppEnv(dup) err = %v, want duplicate", err)
	}
	// Newline in a value.
	if _, err := ParseAppEnv("READAHEAD_SIZE=32\nM"); err == nil || !strings.Contains(err.Error(), "newline") {
		t.Fatalf("ParseAppEnv(newline) err = %v, want newline", err)
	}
	// Token without '='.
	if _, err := ParseAppEnv("USENET_MAX_CONN"); err == nil || !strings.Contains(err.Error(), "want NAME=v1,v2") {
		t.Fatalf("ParseAppEnv(no=) err = %v, want want NAME=v1,v2", err)
	}
}

func TestHeaderRow(t *testing.T) {
	m := sweepMatrix()
	// App names are sorted ascending in the header; USENET_MAX_CONN < READAHEAD_SIZE.
	header := m.Header()
	wantHeader := "readtype,READAHEAD_SIZE,USENET_MAX_CONN,latency,jitter,linespeed,linejitter,cachewritespeed,cachereadspeed,seed"
	if header != wantHeader {
		t.Fatalf("Header = %q, want %q", header, wantHeader)
	}

	cells := m.Cells()
	row := m.Row(cells[0])
	fields := strings.Split(row, ",")
	// cells[0] is the first of the sweep: conns=8, readahead=32M.
	if fields[1] != "32M" || fields[2] != "8" {
		t.Fatalf("Row app columns = %q, want [32M 8]", fields[1:3])
	}
	if len(fields) != len(strings.Split(wantHeader, ",")) {
		t.Fatalf("Row has %d fields, header has %d", len(fields), len(strings.Split(wantHeader, ",")))
	}

	// A cell whose var option is "" renders the empty field in its column.
	m2 := Matrix{App: []AppVar{{Name: "READAHEAD_SIZE", Values: []string{""}}}}
	if row := m2.Row(m2.Cells()[0]); strings.Split(row, ",")[1] != "" {
		t.Fatalf("Row default cell column = %q, want empty", strings.Split(row, ",")[1])
	}
}
