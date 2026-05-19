package wlserver

import "syscall"

// closeFD is the syscall.Close wrapper kept in its own file for
// symmetry with internal/wlclient and internal/wlim.
func closeFD(fd int) error { return syscall.Close(fd) }
