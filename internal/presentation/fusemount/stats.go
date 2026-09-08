package fusemount

import "sync/atomic"

// What the mount is serving, kept here rather than passed around: a process has
// one mount, and a file handle has no way back to the FileSystem that made it.
var (
	openFiles   atomic.Int64
	servedBytes atomic.Int64
)

// Stats reports the files open now and the bytes handed to their readers since
// the process started.
func Stats() (open, served int64) {
	return openFiles.Load(), servedBytes.Load()
}
