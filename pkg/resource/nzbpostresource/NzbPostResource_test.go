package nzbpostresource_test

import (
	"errors"
	"io"
	"sync"
	"testing"

	"git.ruekov.eu/ruakij/nzbStreamer/pkg/resource"
	"git.ruekov.eu/ruakij/nzbStreamer/pkg/resource/nzbpostresource"
)

// Run with -race: concurrent readers of one segment all settle its length.
func TestConcurrentReadersSettleLength(t *testing.T) {
	const hint, body = 12, 10

	post := nzbpostresource.New("id", "group", hint, false, func(_, _ string) ([]byte, error) {
		return make([]byte, body), nil
	})

	if _, err := post.Size(); !errors.Is(err, resource.ErrSizeNotExact) {
		t.Errorf("a segment that was never read reported an exact size, error was %v", err)
	}

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()

			reader, err := post.Open()
			if err != nil {
				t.Errorf("failed opening: %v", err)
				return
			}
			defer reader.Close()

			if _, err := io.ReadAll(reader); err != nil {
				t.Errorf("failed reading: %v", err)
			}
			if _, err := post.SizeHint(); err != nil {
				t.Errorf("failed getting size hint: %v", err)
			}
		}()
	}
	wg.Wait()

	size, err := post.Size()
	if err != nil {
		t.Fatalf("size stayed inexact after reading: %v", err)
	}
	if size != body {
		t.Errorf("size is %d, expected the decoded %d", size, body)
	}
}
