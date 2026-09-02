package nzbparser

import "testing"

func filesNamed(names ...string) []File {
	files := make([]File, len(names))
	for i, name := range names {
		files[i] = File{Filename: name}
	}

	return files
}

// The payload carries the release name and the recovery set only repeats it with
// a volume suffix, so the recovery set must not be what the name is taken from.
func TestGuessNameIgnoresRecoveryFiles(t *testing.T) {
	const want = "Star.Trek.Lower.Decks.S01E01.Second.Contact.1080p.BluRay.DDP.5.1.x265-edge2020"

	files := filesNamed(
		"c52d50fede146861c10d19f6f2ed12cc.par2",
		"c52d50fede146861c10d19f6f2ed12cc.vol00-01.par2",
		want+".mkv",
		"c52d50fede146861c10d19f6f2ed12cc.vol07-09.par2",
	)

	if got := guessName(files); got != want {
		t.Errorf("guessName = %q, want %q", got, want)
	}
}

// Every volume of a set names the same release, so they agree on one name rather
// than each offering its own.
func TestGuessNameCollapsesVolumes(t *testing.T) {
	const want = "Some.Release.1080p.BluRay-GRP"

	files := filesNamed(
		want+".part01.rar",
		want+".part02.rar",
		want+".part03.rar",
		"Some.Release.1080p.BluRay-GRP.par2",
	)

	if got := guessName(files); got != want {
		t.Errorf("guessName = %q, want %q", got, want)
	}
}

// A trailing number that is not a volume number belongs to the name.
func TestGuessNameKeepsTrailingNumber(t *testing.T) {
	const want = "Blade.Runner.2049"

	if got := guessName(filesNamed(want + ".mkv")); got != want {
		t.Errorf("guessName = %q, want %q", got, want)
	}
}

// An nzb of nothing but a recovery set still has to be named something.
func TestGuessNameFallsBackToFirstFile(t *testing.T) {
	files := filesNamed("abc123.par2", "abc123.vol00-01.par2")

	if got := guessName(files); got != "abc123" {
		t.Errorf("guessName = %q, want %q", got, "abc123")
	}
}
