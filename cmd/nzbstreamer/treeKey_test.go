package main

import (
	"regexp"
	"testing"
)

// The key is what a stored tree is restored against, so it has to be a function
// of the settings alone. Two Configs compiled separately from the same variables
// are what two starts of the process see.
func TestTheTreeKeyIsTheSettingsAndNothingElse(t *testing.T) {
	config := func(blacklist string, depth int) Config {
		var c Config
		c.NzbConfig.FileBlacklist = []regexp.Regexp{*regexp.MustCompile(blacklist)}
		c.Filesystem.Blacklist = []regexp.Regexp{*regexp.MustCompile(`(?i)\.par2$`)}
		c.Filesystem.FlattenMaxDepth = depth
		return c
	}

	if first, second := treeKey(config(`\.nfo$`, 0)), treeKey(config(`\.nfo$`, 0)); first != second {
		t.Errorf("the same settings hashed to %s and %s", first, second)
	}
	if unchanged, changed := treeKey(config(`\.nfo$`, 0)), treeKey(config(`\.sfv$`, 0)); unchanged == changed {
		t.Errorf("another blacklist hashed to the same key %s", changed)
	}
	if unchanged, changed := treeKey(config(`\.nfo$`, 0)), treeKey(config(`\.nfo$`, 2)); unchanged == changed {
		t.Errorf("another flatten depth hashed to the same key %s", changed)
	}
}
