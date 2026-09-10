package diskcache

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// admit places data into the write-back buffer, returning almost immediately
// so the caller is not on the disk path. It blocks while the buffer is full and
// at least one entry is already pending, so a single oversized segment (bigger
// than the whole limit on an empty buffer) can always admit and never deadlock.
// Every admission wakes the writer, so the buffer is drained eagerly rather
// than accumulating until it is full.
func (c *Cache) admit(key Key, data []byte) (int64, error) {
	keyStr := key.String()
	c.mu.Lock()

	for !c.closed && c.writeBackBytes+int64(len(data)) > c.options.WriteBackSize && len(c.writeBack) > 0 {
		// Wake the writer so it drains entries and frees room before we block
		c.writeBackCond.Broadcast()
		c.writeBackCond.Wait()
	}

	// A Set after Close falls back to the synchronous store, so an entry behind
	// a cache that stopped draining still reaches disk instead of being left
	// pending forever with nothing to write it
	if c.closed {
		// A pending copy of this key has no writer left either, so it goes
		// before the store below writes the key itself
		if pending, exists := c.writeBack[keyStr]; exists {
			c.dropPendingLocked(keyStr, pending)
		}
		c.mu.Unlock()
		return c.store(key, func(file *os.File) (int64, error) {
			if err := c.evictFor(int64(len(data))); err != nil {
				return 0, err
			}
			n, err := file.Write(data)
			if err != nil {
				return int64(n), fmt.Errorf("failed writing item: %w", err)
			}
			return int64(n), nil
		})
	}
	defer c.mu.Unlock()

	// A pending entry holds none of currentSize, so a disk-resident copy of
	// this key must be unlinked now; otherwise the writer's finalize
	// "+size once" would double count.
	if header, exists := c.items[keyStr]; exists {
		if err := c.removeFileLocked(keyStr, header); err != nil {
			return 0, err
		}
	}

	old := c.writeBack[keyStr]
	c.writeBackGen++
	c.writeBack[keyStr] = pendingEntry{data: data, gen: c.writeBackGen, modTime: time.Now()}
	c.writeBackBytes += int64(len(data)) - int64(len(old.data))
	// Wake the writer eagerly so the segment is written to disk promptly
	c.writeBackCond.Broadcast()
	return int64(len(data)), nil
}

// writeBackLoop is the single writer goroutine: it drains the write-back buffer
// to disk, keeping each entry in the buffer until finalize renames it into
// place so Open can still serve it from memory meanwhile. It exits on Close,
// leaving whatever is left for Close to drain.
func (c *Cache) writeBackLoop() {
	defer c.writeWG.Done()

	for {
		c.mu.Lock()
		for !c.closed && len(c.writeBack) == 0 {
			c.writeBackCond.Wait()
		}
		if c.closed {
			c.mu.Unlock()
			return
		}
		var k string
		var entry pendingEntry
		for k = range c.writeBack {
			entry = c.writeBack[k]
			break
		}
		c.mu.Unlock()

		c.writePending(k, entry)
	}
}

// writePending writes one entry to disk, retrying a bounded number of times. An
// entry that cannot reach disk is dropped: the read path already served it from
// memory, so it is simply not cached.
func (c *Cache) writePending(k string, entry pendingEntry) {
	const attempts = 3
	sz := int64(len(entry.data))
	for attempt := 0; attempt < attempts; attempt++ {
		temp, err := c.writeTemp(entry.data, sz)
		if err == nil {
			// A finalize that fails leaves the entry pending, so it has to be
			// retried and dropped like a failed write; otherwise the writer
			// picks it straight back up and rewrites it forever
			err = c.finalize(k, entry, temp)
			if err == nil {
				return
			}
		}
		if attempt == attempts-1 {
			slog.Warn("Dropping write-back entry, could not reach disk", "key", k, "error", err)
			c.dropPending(k, entry.gen)
			return
		}
		time.Sleep(time.Duration(attempt+1) * 10 * time.Millisecond)
	}
}

// writeTemp writes the temp file that finalize renames into place, first
// evicting to make room on disk.
func (c *Cache) writeTemp(data []byte, sz int64) (string, error) {
	file, err := os.CreateTemp(c.options.TmpCacheDir, "")
	if err != nil {
		return "", fmt.Errorf("failed creating temp file: %w", err)
	}
	tempFilePath := file.Name()
	cleanup := func() {
		file.Close()
		os.Remove(tempFilePath)
	}

	if err := c.evictFor(sz); err != nil {
		cleanup()
		return "", err
	}
	if _, err := file.Write(data); err != nil {
		cleanup()
		return "", fmt.Errorf("failed writing item: %w", err)
	}
	if err := file.Sync(); err != nil {
		cleanup()
		return "", fmt.Errorf("failed syncing file: %w", err)
	}
	if err := file.Close(); err != nil {
		os.Remove(tempFilePath)
		return "", err
	}
	return tempFilePath, nil
}

// finalize renames a fully-written temp file into place, but only if the same
// admission is still pending. If it was removed or re-Set meanwhile, the temp
// file is discarded and no bookkeeping changes, so a dropped entry never leaves
// disk bytes behind.
func (c *Cache) finalize(k string, entry pendingEntry, tempPath string) error {
	filePath, err := Key(strings.Split(k, "/")).path(c.options.CacheDir)
	if err != nil {
		os.Remove(tempPath)
		return err
	}

	c.mu.Lock()
	pending, ok := c.writeBack[k]
	if !ok || pending.gen != entry.gen {
		c.mu.Unlock()
		os.Remove(tempPath)
		return nil
	}
	if err := ensureDirExists(filepath.Dir(filePath)); err != nil {
		c.mu.Unlock()
		os.Remove(tempPath)
		return err
	}
	err = os.Rename(tempPath, filePath)
	if err != nil {
		c.mu.Unlock()
		os.Remove(tempPath)
		return fmt.Errorf("failed renaming file: %w", err)
	}

	// The pending header carries over, so a read while it waited counts for its
	// LRU standing on disk
	sz := int64(len(pending.data))
	c.items[k] = pending.header()
	c.currentSize += sz
	delete(c.writeBack, k)
	c.writeBackBytes -= sz
	c.writeBytes.Add(sz)
	c.writes.Add(1)
	c.writeBackCond.Broadcast()
	c.mu.Unlock()

	return nil
}

// dropPending removes a pending entry that failed to reach disk, unless the key
// was re-admitted meanwhile.
func (c *Cache) dropPending(k string, gen uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if pending, ok := c.writeBack[k]; ok && pending.gen == gen {
		c.dropPendingLocked(k, pending)
	}
}

// dropPendingLocked forgets a pending entry, waking any Set blocked on
// admission. It must hold c.mu. A pending entry has no file and holds none of
// currentSize, so there is nothing else to undo.
func (c *Cache) dropPendingLocked(k string, entry pendingEntry) {
	delete(c.writeBack, k)
	c.writeBackBytes -= int64(len(entry.data))
	c.writeBackCond.Broadcast()
}
