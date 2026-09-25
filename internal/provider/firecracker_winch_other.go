//go:build !unix

package provider

import "os"

// watchResize has no SIGWINCH to watch here; the terminal keeps the size it started at.
func watchResize(*os.File, func(rows, cols int)) (stop func()) { return func() {} }
