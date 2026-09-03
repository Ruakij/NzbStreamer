package filenameops

import (
	"path"
	"regexp"
	"strings"
)

// FileClass says what role a file plays in a release, decided on its name alone.
type FileClass int

const (
	// ClassOther is everything with no bearing on whether the release plays:
	// subtitles, nfo, sample images.
	ClassOther FileClass = iota
	// ClassContent is the archive set and bare media. Missing bytes here make the
	// release unusable.
	ClassContent
	// ClassRecovery is par2. It carries no payload, but its size says how much
	// loss the release could repair.
	ClassRecovery
)

var contentExtensions = map[string]bool{
	".rar": true,
	".7z":  true,
	".zip": true,
	".mkv": true,
	".mp4": true,
	".avi": true,
	".m4v": true,
	".mov": true,
	".ts":  true,
}

// splitVolumeRegexp matches the continuation volumes of a split set: rar's
// `.r00`, zip's `.z01` and the numeric `.001` a 7z or a plain split uses.
var splitVolumeRegexp = regexp.MustCompile(`(?i)\.(r\d{2,3}|z\d{2}|\d{2,3})$`)

// Classify reports which tier a filename belongs to.
func Classify(filename string) FileClass {
	extension := strings.ToLower(path.Ext(filename))

	switch {
	case extension == ".par2":
		return ClassRecovery
	case contentExtensions[extension], splitVolumeRegexp.MatchString(filename):
		return ClassContent
	}
	return ClassOther
}
