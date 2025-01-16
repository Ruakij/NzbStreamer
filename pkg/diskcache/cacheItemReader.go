package diskcache

import (
	"fmt"
	"io"
	"sync"
)

type CacheItemReader struct {
	lock             *sync.RWMutex
	underlyingReader io.ReadSeekCloser
}

func (r *CacheItemReader) Read(p []byte) (n int, err error) {
	n, err = r.underlyingReader.Read(p)
	if err != nil {
		return n, fmt.Errorf("failed reading from underlying reader: %w", err)
	}
	return n, nil
}

func (r *CacheItemReader) Seek(offset int64, whence int) (int64, error) {
	pos, err := r.underlyingReader.Seek(offset, whence)
	if err != nil {
		return pos, fmt.Errorf("failed seeking in underlying reader: %w", err)
	}
	return pos, nil
}

func (r *CacheItemReader) Close() error {
	r.lock.RUnlock()
	err := r.underlyingReader.Close()
	if err != nil {
		return fmt.Errorf("failed closing underlying reader: %w", err)
	}
	return nil
}
