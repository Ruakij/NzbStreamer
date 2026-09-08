package webdav

import "sync/atomic"

// What the tree is serving, kept here rather than passed around: a process has
// one tree. A reader is pooled for reuse, so one held open is not necessarily
// one a client is reading from at that moment.
var (
	openReaders atomic.Int64
	servedBytes atomic.Int64
)

// Stats reports the readers open now and the bytes handed to them since the
// process started.
func Stats() (open, served int64) {
	return openReaders.Load(), servedBytes.Load()
}
