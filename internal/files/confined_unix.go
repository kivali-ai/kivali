//go:build unix

package files

import "syscall"

// openNonblock keeps an open of a FIFO from blocking on a missing
// writer. See OpenNoFollowIn.
const openNonblock = syscall.O_NONBLOCK
