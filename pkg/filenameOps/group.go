package filenameOps

import (
	"regexp"
	"slices"
	"strings"
)

// var partNumberRegexp *regexp.Regexp = regexp.MustCompile(`([.\-_][\-_\w]{0,8})?\d{2,4}[\-_]?`)
var partNumberRegexp *regexp.Regexp = regexp.MustCompile(`([.\-_])?\d{2,4}[\-_]?`)

func GroupPartFilenames(filenames []string) map[string][]string {
	groupedFiles := make(map[string][]string, 1)

	for _, filename := range filenames {
		basename := GetBaseFilename(filename)
		extension, _ := strings.CutPrefix(filename, basename)

		extensionWithoutPartNumbers := partNumberRegexp.ReplaceAllString(extension, "")

		groupName := basename + extensionWithoutPartNumbers

		groupedFiles[groupName] = append(groupedFiles[groupName], filename)
	}

	return groupedFiles
}

func SortGroupedFilenames(groupedFiles map[string][]string) {
	for _, filenames := range groupedFiles {
		slices.SortFunc(filenames, CompareNumberStrings)
	}
}
