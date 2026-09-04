package filenameops_test

import (
	"slices"
	"testing"

	"git.ruekov.eu/ruakij/nzbStreamer/pkg/filenameops"
)

func TestGroupPartFilenames(t *testing.T) {
	cases := map[string]struct {
		filenames []string
		group     string
		want      []string
	}{
		// A classic rar set spells its first volume differently from the rest,
		// and it is still the volume everything else follows
		"rar set": {
			filenames: []string{"Some.Release.r01", "Some.Release.rar", "Some.Release.r00", "Some.Release.r10"},
			group:     "Some.Release.rar",
			want:      []string{"Some.Release.rar", "Some.Release.r00", "Some.Release.r01", "Some.Release.r10"},
		},
		"part set": {
			filenames: []string{"Some.Release.part10.rar", "Some.Release.part2.rar", "Some.Release.part1.rar"},
			group:     "Some.Release.part.rar",
			want:      []string{"Some.Release.part1.rar", "Some.Release.part2.rar", "Some.Release.part10.rar"},
		},
	}

	for name, test := range cases {
		t.Run(name, func(t *testing.T) {
			grouped := filenameops.GroupPartFilenames(test.filenames)
			filenameops.SortGroupedFilenames(grouped)

			if len(grouped) != 1 {
				t.Fatalf("GroupPartFilenames(%v) = %v, want one group", test.filenames, grouped)
			}
			if got := grouped[test.group]; !slices.Equal(got, test.want) {
				t.Errorf("group %q = %v, want %v", test.group, got, test.want)
			}
		})
	}
}
