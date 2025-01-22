// Package hookedresource provides a mechanism to intercept and modify resource operations
// through a hook system. This allows for adding middleware-like functionality to any
// ReadSeekCloseableResource implementation.
package hookedresource

import (
	"fmt"
	"io"

	"git.ruekov.eu/ruakij/nzbStreamer/pkg/resource"
)

// HookedResource wraps a ReadSeekCloseableResource and provides hook points for
// all major operations (Open, Read, Seek, Close). Hooks are executed in the order
// they were added, with each hook having the ability to modify the operation or
// prevent it entirely.
type HookedResource struct {
	underlying resource.ReadSeekCloseableResource
	openHooks  []OpenHook
	readHooks  []ReadHook
	seekHooks  []SeekHook
	closeHooks []CloseHook
}

// NewHookedResource creates a new HookedResource that wraps the given underlying
// resource. The returned HookedResource starts with no hooks attached.
func NewHookedResource(underlying resource.ReadSeekCloseableResource) *HookedResource {
	return &HookedResource{
		underlying: underlying,
	}
}

func (r *HookedResource) AddOpenHook(hook OpenHook) {
	r.openHooks = append(r.openHooks, hook)
}

func (r *HookedResource) AddReadHook(hook ReadHook) {
	r.readHooks = append(r.readHooks, hook)
}

func (r *HookedResource) AddSeekHook(hook SeekHook) {
	r.seekHooks = append(r.seekHooks, hook)
}

func (r *HookedResource) AddCloseHook(hook CloseHook) {
	r.closeHooks = append(r.closeHooks, hook)
}

// Size returns the size of the underlying resource.
// It implements the ReadSeekCloseableResource interface.
func (r *HookedResource) Size() (int64, error) {
	size, err := r.underlying.Size()
	if err != nil {
		return 0, fmt.Errorf("hookedresource: size from underlying resource: %w", err)
	}
	return size, nil
}

func (r *HookedResource) Open() (io.ReadSeekCloser, error) {
	var openChain func() (io.ReadSeekCloser, error)

	openChain = func() (io.ReadSeekCloser, error) {
		reader, err := r.underlying.Open()
		if err != nil {
			return nil, fmt.Errorf("failed to open underlying resource: %w", err)
		}
		return &HookReader{
			reader:     reader,
			readHooks:  r.readHooks,
			seekHooks:  r.seekHooks,
			closeHooks: r.closeHooks,
		}, nil
	}

	// Chain open hooks in reverse order
	for i := len(r.openHooks) - 1; i >= 0; i-- {
		hook := r.openHooks[i]
		nextChain := openChain
		openChain = func() (io.ReadSeekCloser, error) {
			return hook(nextChain)
		}
	}

	return openChain()
}

// HookReader implements io.ReadSeekCloser and manages the execution
// of hooks for read, seek, and close operations.
type HookReader struct {
	reader     io.ReadSeekCloser
	readHooks  []ReadHook
	seekHooks  []SeekHook
	closeHooks []CloseHook
}

func (r *HookReader) GetUnderlyingReader() io.ReadSeekCloser {
	return r.reader
}

func (r *HookReader) Read(p []byte) (int, error) {
	var readChain func([]byte) (int, error)

	readChain = func(p []byte) (int, error) {
		n, err := r.reader.Read(p)
		if err != nil {
			return n, fmt.Errorf("failed to read from underlying reader: %w", err)
		}
		return n, nil
	}

	// Chain read hooks in reverse order
	for i := len(r.readHooks) - 1; i >= 0; i-- {
		hook := r.readHooks[i]
		nextChain := readChain
		readChain = func(p []byte) (int, error) {
			return hook(p, nextChain)
		}
	}

	return readChain(p)
}

func (r *HookReader) Seek(offset int64, whence int) (int64, error) {
	var seekChain func(int64, int) (int64, error)

	seekChain = func(offset int64, whence int) (int64, error) {
		pos, err := r.reader.Seek(offset, whence)
		if err != nil {
			return pos, fmt.Errorf("failed to seek in underlying reader: %w", err)
		}
		return pos, nil
	}

	// Chain seek hooks in reverse order
	for i := len(r.seekHooks) - 1; i >= 0; i-- {
		hook := r.seekHooks[i]
		nextChain := seekChain
		seekChain = func(offset int64, whence int) (int64, error) {
			return hook(offset, whence, nextChain)
		}
	}

	return seekChain(offset, whence)
}

func (r *HookReader) Close() error {
	var closeChain func() error

	closeChain = func() error {
		if err := r.reader.Close(); err != nil {
			return fmt.Errorf("failed to close underlying reader: %w", err)
		}
		return nil
	}

	// Chain close hooks in reverse order
	for i := len(r.closeHooks) - 1; i >= 0; i-- {
		hook := r.closeHooks[i]
		nextChain := closeChain
		closeChain = func() error {
			return hook(nextChain)
		}
	}

	return closeChain()
}
