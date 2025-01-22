package hookedresource

import "io"

// OpenHook defines a function that intercepts resource opening operations.
// It receives a next function that represents the next hook in the chain or
// the actual open operation if it's the last hook.
//
// The hook can:
// - Modify the returned ReadSeekCloser
// - Handle errors from subsequent hooks
// - Prevent the operation by returning an error
// - Add logging or metrics
//
// Example:
//
//	resource.AddOpenHook(func(next func() (io.ReadSeekCloser, error)) (io.ReadSeekCloser, error) {
//	    fmt.Println("Opening resource")
//	    return next()
//	})
type OpenHook func(next func() (io.ReadSeekCloser, error)) (io.ReadSeekCloser, error)

// ReadHook defines a function that intercepts read operations.
// It receives the byte slice to read into and a next function that represents
// the next hook in the chain or the actual read operation if it's the last hook.
//
// The hook can:
// - Modify the data being read
// - Transform the data
// - Handle errors from subsequent hooks
// - Add logging or metrics
//
// Example:
//
//	resource.AddReadHook(func(p []byte, next func([]byte) (int, error)) (int, error) {
//	    n, err := next(p)
//	    if err == nil {
//	        // Transform data in p[:n]
//	    }
//	    return n, err
//	})
type ReadHook func(p []byte, next func([]byte) (int, error)) (int, error)

// SeekHook defines a function that intercepts seek operations.
// It receives the seek offset, whence value, and a next function that represents
// the next hook in the chain or the actual seek operation if it's the last hook.
//
// The hook can:
// - Validate or modify seek parameters
// - Prevent seeking to certain positions
// - Handle errors from subsequent hooks
// - Add logging or metrics
//
// Example:
//
//	resource.AddSeekHook(func(offset int64, whence int, next func(int64, int) (int64, error)) (int64, error) {
//	    if offset < 0 {
//	        return 0, fmt.Errorf("negative seek not allowed")
//	    }
//	    return next(offset, whence)
//	})
type SeekHook func(offset int64, whence int, next func(int64, int) (int64, error)) (int64, error)

// CloseHook defines a function that intercepts close operations.
// It receives a next function that represents the next hook in the chain or
// the actual close operation if it's the last hook.
//
// The hook can:
// - Perform cleanup operations
// - Handle errors from subsequent hooks
// - Add logging or metrics
//
// Example:
//
//	resource.AddCloseHook(func(next func() error) error {
//	    fmt.Println("Closing resource")
//	    return next()
//	})
type CloseHook func(next func() error) error
