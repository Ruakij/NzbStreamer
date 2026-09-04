package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// treeRules is bumped whenever the factory's own naming rules change - the
// grouping of an archive's volumes, the deobfuscation, how an unpacked archive
// is named. A stored tree is only as good as the code that built it, and the
// settings alone cannot say that code is the same one.
const treeRules = 1

// treeKey identifies what an nzb's presented tree depends on besides the nzb
// itself: the rules above and the settings that decide which files are
// presented and what they are called. A tree stored under another key is
// rebuilt, so a settings change takes effect on the next start without throwing
// away the trees it did not affect.
//
// NZB_EAGER_EXACT_SIZE_CLASSES is deliberately not part of it. It decides
// whether a size was measured, not what the tree looks like, and each stored row
// already says which of the two it is.
// A regexp is hashed as the pattern it was compiled from. The compiled form is
// full of pointers into itself, so anything that reads the struct hashes the
// address it happens to sit at and no two processes agree on a key.
func treeKey(c Config) string {
	hash := sha256.New()

	fmt.Fprintf(hash, "%d\x00%d\x00%d\x00%v\x00",
		treeRules,
		c.NzbConfig.MaxArchiveDepth,
		c.Filesystem.FlattenMaxDepth,
		c.Filesystem.FixFilenameThreshold,
	)
	for _, pattern := range c.NzbConfig.FileBlacklist {
		fmt.Fprintf(hash, "nzb=%s\x00", pattern.String())
	}
	for _, pattern := range c.Filesystem.Blacklist {
		fmt.Fprintf(hash, "fs=%s\x00", pattern.String())
	}

	return hex.EncodeToString(hash.Sum(nil)[:8])
}
