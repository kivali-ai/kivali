//go:build !unix

package files

// openNonblock is a no-op where FIFOs cannot block an open.
const openNonblock = 0
