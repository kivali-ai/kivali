//go:build !unix

package web

// diskFree cannot read free space here; -1 skips the checks that use it.
func diskFree(string) int64 { return -1 }
