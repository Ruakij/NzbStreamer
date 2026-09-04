package filenameops

import (
	"path"
	"regexp"
	"slices"
	"strings"
)

// var partNumberRegexp *regexp.Regexp = regexp.MustCompile(`([.\-_][\-_\w]{0,8})?\d{2,4}[\-_]?`)
var partNumberRegexp *regexp.Regexp = regexp.MustCompile(`([.\-_])?\d{1,4}[\-_]?`)

// continuationExtensions maps the extension a numbered continuation volume is
// left with once its number is stripped onto the extension of the sets first
// volume, which is spelled differently: a rar set is name.rar, name.r00,
// name.r01, and all of it is one archive.
var continuationExtensions = map[string]string{".r": ".rar"}

// firstVolumeExtensions are the extensions a set names its first volume with
// while numbering the rest, so the numbers say nothing about where it goes.
var firstVolumeExtensions = map[string]bool{".rar": true}

func GroupPartFilenames(filenames []string) map[string][]string {
	groupedFiles := make(map[string][]string, 1)

	for _, filename := range filenames {
		basename := GetBaseFilename(filename)
		extension, _ := strings.CutPrefix(filename, basename)

		extensionWithoutPartNumbers := partNumberRegexp.ReplaceAllString(extension, "")
		stripped := path.Ext(extensionWithoutPartNumbers)
		if first, isContinuation := continuationExtensions[strings.ToLower(stripped)]; isContinuation {
			extensionWithoutPartNumbers = strings.TrimSuffix(extensionWithoutPartNumbers, stripped) + first
		}

		groupName := basename + extensionWithoutPartNumbers

		groupedFiles[groupName] = append(groupedFiles[groupName], filename)
	}

	return groupedFiles
}

func SortGroupedFilenames(groupedFiles map[string][]string) {
	for _, filenames := range groupedFiles {
		slices.SortFunc(filenames, compareVolumes)
	}
}

// compareVolumes orders the volumes of one set. Where the first volume keeps the
// archives own extension and the rest are numbered, it comes first whatever the
// numbers compare to; everything else is ordered by them.
func compareVolumes(a, b string) int {
	firstA := firstVolumeExtensions[strings.ToLower(path.Ext(a))]
	firstB := firstVolumeExtensions[strings.ToLower(path.Ext(b))]
	if firstA != firstB {
		if firstA {
			return -1
		}
		return 1
	}
	return CompareNumberStrings(a, b)
}
