package wlclient

import "syscall"

// closeFD is the syscall.Close wrapper kept in its own file so
// future per-OS variants (Windows isn't a Wayland target, but
// keeping the abstraction clean costs nothing) sit naturally
// alongside the cross-platform code in objects.go.
func closeFD(fd int) error { return syscall.Close(fd) }
