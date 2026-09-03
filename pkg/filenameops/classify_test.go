package filenameops_test

import (
	"testing"

	"git.ruekov.eu/ruakij/nzbStreamer/pkg/filenameops"
)

func TestClassify(t *testing.T) {
	cases := map[string]filenameops.FileClass{
		"Some.Release.part01.rar":     filenameops.ClassContent,
		"Some.Release.rar":            filenameops.ClassContent,
		"Some.Release.r07":            filenameops.ClassContent,
		"Some.Release.7z.002":         filenameops.ClassContent,
		"Some.Release.1080p.x264.mkv": filenameops.ClassContent,
		"Some.Release.vol00+01.par2":  filenameops.ClassRecovery,
		"Some.Release.par2":           filenameops.ClassRecovery,
		"Some.Release.nfo":            filenameops.ClassOther,
		"Some.Release.eng.srt":        filenameops.ClassOther,
		"Some.Release":                filenameops.ClassOther,
	}

	for filename, want := range cases {
		if got := filenameops.Classify(filename); got != want {
			t.Errorf("Classify(%q) = %v, want %v", filename, got, want)
		}
	}
}
