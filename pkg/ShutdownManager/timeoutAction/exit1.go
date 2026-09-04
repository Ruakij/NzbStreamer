// Package timeoutaction provides the actions ShutdownManager runs when a
// shutdown times out, such as exiting hard.
package timeoutaction

import "os"

func Exit1() {
	os.Exit(1)
}
