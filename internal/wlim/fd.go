package wlim

import "syscall"

// closeRawFD is the syscall.Close wrapper kept in its own file so
// any future per-OS variants sit alongside cross-platform code.
func closeRawFD(fd int) error { return syscall.Close(fd) }
