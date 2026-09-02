package nzbparser

import "testing"

func nzbWithMeta(meta map[string]string, filenames ...string) *NzbData {
	return &NzbData{Meta: meta, Files: filesNamed(filenames...)}
}

// An obfuscated nzb names its files, its recovery set and its archive members
// after the same hash, so the name of the file it arrived in is the only one left
// that says what the release is.
func TestResolveNameTakesTheFilename(t *testing.T) {
	const want = "Star.Trek.Lower.Decks.S01E03.720p.BluRay.x264-Gi6"

	nzb := nzbWithMeta(map[string]string{"Name": "95fb304da0f4e5d0d9a9feb5e230571a"},
		"95fb304da0f4e5d0d9a9feb5e230571a.part01.rar",
		"95fb304da0f4e5d0d9a9feb5e230571a.vol00-01.par2",
	)

	if got := resolveName(nzb, want+".nzb"); got != want {
		t.Errorf("resolveName = %q, want %q", got, want)
	}
}

// A source with no file behind it, such as the store, falls back through what the
// nzb says about itself.
func TestResolveNameFallsBackWithoutAFilename(t *testing.T) {
	for _, test := range []struct {
		name string
		nzb  *NzbData
		want string
	}{
		{"meta name", nzbWithMeta(map[string]string{"Name": "From.Meta"}, "a.mkv"), "From.Meta"},
		{"meta title", nzbWithMeta(map[string]string{"Title": "From.Title"}, "a.mkv"), "From.Title"},
		{"guessed", nzbWithMeta(nil, "Some.Release.part01.rar"), "Some.Release"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := resolveName(test.nzb, ""); got != test.want {
				t.Errorf("resolveName = %q, want %q", got, test.want)
			}
		})
	}
}
