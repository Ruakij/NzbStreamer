package filenameOps

import (
	"regexp"

	levenshtein "github.com/ka-weihe/fast-levenshtein"
)

var filenameExtensionRegexp *regexp.Regexp = regexp.MustCompile(`(\.([\w\-+\[\]()]{1,10}))+$`)

func GetBaseFilename(filename string) string {
	return filenameExtensionRegexp.ReplaceAllString(filename, "")
}

func GetOrDefaultWithExtensionBelowLevensteinSimilarity(a, b string, smilarity float32) string {
	aBase := GetBaseFilename(a)
	bBase := GetBaseFilename(b)

	if 1-float32(levenshtein.Distance(aBase, bBase))/float32(len(bBase)) < smilarity {
		return b + a[len(aBase):]
	}
	return a
}
