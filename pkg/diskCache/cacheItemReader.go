package diskCache

import (
	"io"
	"sync"
)

type CacheItemReader struct {
	lock             *sync.RWMutex
	underlyingReader io.ReadSeekCloser
}

func (r *CacheItemReader) Read(p []byte) (n int, err error) {
	return r.underlyingReader.Read(p)
}
func (r *CacheItemReader) Seek(offset int64, whence int) (int64, error) {
	return r.underlyingReader.Seek(offset, whence)
}
func (r *CacheItemReader) Close() error {
	r.lock.RUnlock()
	return r.underlyingReader.Close()
}
