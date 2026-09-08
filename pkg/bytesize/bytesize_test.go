package bytesize

import "testing"

func TestBytesUnmarshal(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want Bytes
	}{
		{"0", 0},
		{"", 0},
		{"1024", 1024},
		{"12M", 12 * 1024 * 1024},
		{"12MB", 12 * 1024 * 1024},
		{"12MiB", 12 * 1024 * 1024},
		{" 8k ", 8 * 1024},
		{"12m", 12 * 1024 * 1024},
		{"12mib", 12 * 1024 * 1024},
		{"1G", 1024 * 1024 * 1024},
		{"1g", 1024 * 1024 * 1024},
		{"2T", 2 * 1024 * 1024 * 1024 * 1024},
	} {
		var got Bytes
		if err := got.UnmarshalText([]byte(tc.in)); err != nil {
			t.Errorf("%q: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("%q = %d, want %d", tc.in, got, tc.want)
		}
	}

	for _, in := range []string{"M", "12X", "twelve", "12 M B"} {
		var got Bytes
		if err := got.UnmarshalText([]byte(in)); err == nil {
			t.Errorf("%q = %d, want an error", in, got)
		}
	}
}

func TestBytesReadsAsItIsSaid(t *testing.T) {
	for _, tc := range []struct {
		in   Bytes
		want string
	}{
		{0, "0"},
		{512, "512"},
		{8 * 1024, "8K"},
		{12 * 1024 * 1024, "12M"},
		{1536 * 1024 * 1024, "1.5G"},
		{1024 * 1024 * 1024 * 1024, "1T"},
		{5 * 1024 * 1024 * 1024 * 1024, "5T"},
	} {
		if got := tc.in.String(); got != tc.want {
			t.Errorf("%d printed as %q, want %q", int64(tc.in), got, tc.want)
		}
	}
}
