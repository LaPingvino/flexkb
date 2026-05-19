package wlserver

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"

	"github.com/lapingvino/flexkb/internal/wlwire"
)

// Listener accepts client connections to a server-side Wayland
// socket. flexkb-imed instantiates one to host its own
// zwp_input_method_manager_v2 global so downstream v2 IMEs
// (fcitx5, custom v2-native engines) can plug in.
//
// Lifetime: Listen returns a Listener that owns the underlying
// *net.UnixListener. Accept must be invoked from a goroutine; it
// loops, accepting one connection at a time and constructing a
// Dispatcher for each. Close stops accepting and tears down all
// active connections.
type Listener struct {
	sock *net.UnixListener
	path string
	log  *slog.Logger

	// onAccept is invoked for each new connection. The handler
	// is responsible for registering the per-connection wl_display
	// + wl_registry handlers, populating the registry with the
	// globals to advertise, and then calling Dispatcher.Run (or
	// letting the Listener call Run after onAccept returns —
	// whichever feels cleaner; the Listener calls Run, so onAccept
	// should be quick).
	onAccept func(*Dispatcher) error

	mu          sync.Mutex
	active      map[*Dispatcher]struct{}
	closed      bool
	acceptStop  chan struct{}
	acceptDoneC chan struct{}
}

// Listen creates a server-side socket at path. Removes any stale
// socket file first (a previous instance crashed without unlinking
// is the common case). Caller passes onAccept to do per-connection
// setup — it's where Display/Registry get registered with the
// dispatcher and globals are populated.
func Listen(path string, log *slog.Logger, onAccept func(*Dispatcher) error) (*Listener, error) {
	if log == nil {
		log = slog.Default()
	}
	// Remove stale socket (the kernel won't let us bind otherwise).
	// We're cautious — only unlink if it's a socket; refuse to
	// blow away a regular file at the same path.
	if info, err := os.Stat(path); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("wlserver: %s exists and is not a socket; refusing to overwrite", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, fmt.Errorf("wlserver: remove stale %s: %w", path, err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("wlserver: stat %s: %w", path, err)
	}
	addr, err := net.ResolveUnixAddr("unix", path)
	if err != nil {
		return nil, fmt.Errorf("wlserver: resolve %s: %w", path, err)
	}
	sock, err := net.ListenUnix("unix", addr)
	if err != nil {
		return nil, fmt.Errorf("wlserver: listen %s: %w", path, err)
	}
	// Restrict access — only the same user. Matches the compositor's
	// own socket, which is created with mode 0700 on most systems.
	if err := os.Chmod(path, 0700); err != nil {
		_ = sock.Close()
		return nil, fmt.Errorf("wlserver: chmod %s: %w", path, err)
	}
	return &Listener{
		sock:        sock,
		path:        path,
		log:         log,
		onAccept:    onAccept,
		active:      map[*Dispatcher]struct{}{},
		acceptStop:  make(chan struct{}),
		acceptDoneC: make(chan struct{}),
	}, nil
}

// Path returns the socket path, useful for error messages and
// for the daemon to surface as the WAYLAND_DISPLAY override.
func (l *Listener) Path() string { return l.path }

// Serve runs the accept loop. Blocks until Close. Errors during
// individual accept calls are logged and the loop continues; the
// only thing that breaks Serve is the listener being closed.
func (l *Listener) Serve() error {
	defer close(l.acceptDoneC)
	for {
		c, err := l.sock.AcceptUnix()
		if err != nil {
			l.mu.Lock()
			closed := l.closed
			l.mu.Unlock()
			if closed {
				return nil
			}
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			l.log.Warn("wlserver accept error", "err", err)
			continue
		}
		conn := wlwire.NewConnFromUnixConn(c)
		d := NewDispatcher(conn, l.log)
		l.mu.Lock()
		l.active[d] = struct{}{}
		l.mu.Unlock()
		d.onClose = func() {
			l.mu.Lock()
			delete(l.active, d)
			l.mu.Unlock()
		}
		if err := l.onAccept(d); err != nil {
			l.log.Warn("wlserver onAccept failed", "err", err)
			_ = d.Close()
			continue
		}
		go func() {
			if err := d.Run(); err != nil {
				l.log.Debug("wlserver client run ended", "err", err)
			}
		}()
	}
}

// Close stops accepting new connections and tears down all
// existing ones. Safe to call multiple times.
func (l *Listener) Close() error {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return nil
	}
	l.closed = true
	dispatchers := make([]*Dispatcher, 0, len(l.active))
	for d := range l.active {
		dispatchers = append(dispatchers, d)
	}
	l.mu.Unlock()

	err := l.sock.Close()
	// Close all active dispatchers — their Run loops return on
	// EOF, then onClose drops them from the active set.
	for _, d := range dispatchers {
		_ = d.Close()
	}
	// Remove the socket file so the next instance can listen.
	// Best-effort — if the path is already gone, fine.
	_ = os.Remove(l.path)
	return err
}

// DefaultSocketPath returns $XDG_RUNTIME_DIR/flexkb-imed-v2.sock,
// the side socket flexkb-imed exposes its v2 server on. Downstream
// IMEs set WAYLAND_DISPLAY to this path before connecting.
//
// Returns an error if $XDG_RUNTIME_DIR isn't set — there's no
// sensible fallback (writing under /tmp would be a security
// downgrade vs. the per-user runtime dir).
func DefaultSocketPath() (string, error) {
	rt := os.Getenv("XDG_RUNTIME_DIR")
	if rt == "" {
		return "", errors.New("wlserver: XDG_RUNTIME_DIR not set; cannot pick default socket path")
	}
	return filepath.Join(rt, "flexkb-imed-v2.sock"), nil
}
