package nzbparser

import (
	"regexp"
	"strings"
)

// recoveryExtensions are the files that carry no payload. They repeat the
// release name with a volume suffix of their own, so a name guessed from one
// describes the recovery set rather than the release.
var recoveryExtensions = map[string]bool{
	".par2": true,
	".nfo":  true,
	".sfv":  true,
	".srr":  true,
}

// partSuffixRegexp matches the suffix that distinguishes one volume of a set
// from the next. Only the forms that spell out what they number are stripped: a
// bare trailing number is as likely to be a year or an episode.
var partSuffixRegexp = regexp.MustCompile(`(?i)[.\-_](part\d+|vol\d+([+\-]\d+)?)$`)

// guessName derives a release name from the files an nzb lists, for one that
// carries no name of its own.
//
// Every volume of a set answers with the same name once its part suffix is
// stripped, so the name the most files agree on is the one describing the
// release. Counting beats picking a single file: which file is first, last or
// largest says nothing, while agreement across a set does.
func guessName(files []File) string {
	counts := make(map[string]int, len(files))

	var best string
	for i := range files {
		name := candidateName(files[i].Filename)
		if name == "" {
			continue
		}

		counts[name]++
		// Prefer a dotted name, which is how releases are written, and the
		// longer one on a tie so the choice does not depend on file order
		if better(name, best, counts) {
			best = name
		}
	}

	if best == "" && len(files) > 0 {
		return getBaseFilename(files[0].Filename)
	}

	return best
}

// candidateName reduces a filename to the release name it suggests, or empty for
// a file that suggests none.
func candidateName(filename string) string {
	extension := filename[len(getBaseFilename(filename)):]
	if recoveryExtensions[strings.ToLower(extension)] {
		return ""
	}

	return partSuffixRegexp.ReplaceAllString(getBaseFilename(filename), "")
}

func better(name, best string, counts map[string]int) bool {
	if best == "" {
		return true
	}

	if dotted := strings.Contains(name, "."); dotted != strings.Contains(best, ".") {
		return dotted
	}
	if counts[name] != counts[best] {
		return counts[name] > counts[best]
	}

	return len(name) > len(best)
}
