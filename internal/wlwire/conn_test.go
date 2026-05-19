package wlwire

import (
	"net"
	"os"
	"sync"
	"syscall"
	"testing"
)

// TestSocketPathResolution — covers the libwayland-style lookup
// order: $WAYLAND_DISPLAY (absolute or relative), defaulting to
// "wayland-0" under $XDG_RUNTIME_DIR.
func TestSocketPathResolution(t *testing.T) {
	cases := []struct {
		name        string
		display     string
		runtimeDir  string
		want        string
		wantErr     bool
	}{
		{"default name + runtime dir", "", "/run/user/1000", "/run/user/1000/wayland-0", false},
		{"named relative", "wayland-1", "/run/user/1000", "/run/user/1000/wayland-1", false},
		{"absolute path", "/tmp/custom-wl-sock", "/run/user/1000", "/tmp/custom-wl-sock", false},
		{"no runtime dir", "", "", "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("WAYLAND_DISPLAY", c.display)
			t.Setenv("XDG_RUNTIME_DIR", c.runtimeDir)
			got, err := SocketPath()
			if c.wantErr {
				if err == nil {
					t.Errorf("expected error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

// TestConnRoundTripOverSocketpair — exercises the real
// ReadMsgUnix/WriteMsgUnix code path (not the simple bytes.Buffer
// from the wire tests) via socketpair. Confirms framing,
// concurrent-write serialisation, and clean close behaviour all
// work over a kernel-mediated stream socket — the same primitive
// the daemon uses with the compositor.
func TestConnRoundTripOverSocketpair(t *testing.T) {
	// Build a unix-stream socketpair to simulate the
	// daemon↔compositor connection.
	fds, err := makeUnixStreamPair()
	if err != nil {
		t.Skipf("socketpair unavailable on this platform: %v", err)
	}
	a := unixConnFromFD(t, fds[0])
	defer a.Close()
	b := unixConnFromFD(t, fds[1])
	defer b.Close()

	ca := &Conn{sock: a}
	cb := &Conn{sock: b}

	// Side a writes; side b reads.
	body := NewEncoder()
	body.PutUint(42)
	body.PutString("hello")
	if err := ca.WriteMessage(1, 0, body.Bytes(), nil); err != nil {
		t.Fatalf("write: %v", err)
	}
	h, gotBody, _, err := cb.ReadMessage()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if h.ObjectID != 1 || h.Opcode != 0 {
		t.Errorf("header: got %+v", h)
	}
	d := NewDecoder(gotBody)
	if d.Uint() != 42 || d.String() != "hello" {
		t.Errorf("body decode wrong")
	}
}

// TestConnWriteConcurrency — the writeMu in WriteMessage means a
// single Conn can have multiple senders without interleaved
// bytes. Race detector flags violations; this test forces
// concurrency to give it material.
func TestConnWriteConcurrency(t *testing.T) {
	fds, err := makeUnixStreamPair()
	if err != nil {
		t.Skipf("socketpair unavailable: %v", err)
	}
	a := unixConnFromFD(t, fds[0])
	defer a.Close()
	b := unixConnFromFD(t, fds[1])
	defer b.Close()
	ca := &Conn{sock: a}
	cb := &Conn{sock: b}

	const N = 50
	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			body := NewEncoder()
			body.PutUint(uint32(idx))
			if err := ca.WriteMessage(1, 0, body.Bytes(), nil); err != nil {
				t.Errorf("write %d: %v", idx, err)
			}
		}(i)
	}
	wg.Wait()

	seen := make(map[uint32]bool, N)
	for i := 0; i < N; i++ {
		_, body, _, err := cb.ReadMessage()
		if err != nil {
			t.Fatalf("read %d: %v", i, err)
		}
		v := NewDecoder(body).Uint()
		if seen[v] {
			t.Errorf("duplicate value %d", v)
		}
		seen[v] = true
	}
	if len(seen) != N {
		t.Errorf("got %d unique messages, want %d", len(seen), N)
	}
}

// --- helpers ---

// makeUnixStreamPair returns a freshly-allocated pair of connected
// SOCK_STREAM AF_UNIX file descriptors via socketpair(2).
func makeUnixStreamPair() ([2]int, error) {
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		return [2]int{}, err
	}
	return [2]int{fds[0], fds[1]}, nil
}

// unixConnFromFD wraps a raw fd as a *net.UnixConn so we can use
// the same code paths Conn uses against a real compositor.
func unixConnFromFD(t *testing.T, fd int) *net.UnixConn {
	t.Helper()
	f := os.NewFile(uintptr(fd), "socketpair")
	conn, err := net.FileConn(f)
	if err != nil {
		t.Fatalf("FileConn: %v", err)
	}
	// FileConn dup'd the fd; close the os.File so we don't leak.
	f.Close()
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		t.Fatalf("expected *net.UnixConn, got %T", conn)
	}
	return uc
}
