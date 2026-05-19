package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"

	"github.com/lapingvino/flexkb/internal/ibusengines"
)

// controlServer exposes a small Unix-socket control API the GUI
// (and other tooling) uses to ask the daemon about itself: which
// backends are running, which stack is configured, what the IM
// session's current state is. v1 surface is read-only — the GUI
// observes, doesn't drive. Drive-side commands (switch stacks,
// reload data) land in a follow-up alongside the
// "daemon-GUI bidirectional" work.
//
// Protocol: one JSON request per line, one JSON response per line.
// Simple newline-delimited JSON keeps the parser trivial in both
// directions and is easy to drive from `nc` or `socat` for
// debugging.
type controlServer struct {
	log     *slog.Logger
	path    string
	listen  net.Listener
	state   *controlState
	once    sync.Once
}

// controlState is the snapshot the daemon shares with control
// clients. Updated atomically (whole-struct replace under mutex)
// by the daemon as backends start and sessions change.
type controlState struct {
	mu sync.Mutex

	Backends   []string `json:"backends"`
	LayoutFile string   `json:"layout_file"`
	Variant    string   `json:"variant"`
	IM         string   `json:"im"`
}

func newControlState(layoutFile, variant, im string) *controlState {
	return &controlState{LayoutFile: layoutFile, Variant: variant, IM: im}
}

// addBackend records that a backend successfully started. The
// GUI status badge surfaces these so the user sees "wayland-v2,
// ibus" or "wayland-v2 only" etc.
func (s *controlState) addBackend(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, existing := range s.Backends {
		if existing == name {
			return
		}
	}
	s.Backends = append(s.Backends, name)
}

func (s *controlState) snapshot() controlStateSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return controlStateSnapshot{
		Backends:   append([]string(nil), s.Backends...),
		LayoutFile: s.LayoutFile,
		Variant:    s.Variant,
		IM:         s.IM,
	}
}

// controlStateSnapshot is the on-wire form. Separate from
// controlState so the mutex doesn't leak into JSON.
type controlStateSnapshot struct {
	Backends   []string `json:"backends"`
	LayoutFile string   `json:"layout_file"`
	Variant    string   `json:"variant"`
	IM         string   `json:"im,omitempty"`
}

// startControlServer begins listening on the conventional control
// socket path. Returns the server (so the daemon can call
// state.addBackend as backends come up) and any error.
//
// Bind path:
//   $XDG_RUNTIME_DIR/flexkb-imed.sock — primary
//   /tmp/flexkb-imed-<uid>.sock        — fallback when XDG unset
//
// Existing socket files are removed if present (stale from a
// crashed previous daemon). Same convention every desktop daemon
// uses.
func startControlServer(log *slog.Logger, state *controlState) (*controlServer, error) {
	path := controlSocketPath()
	if path == "" {
		return nil, errors.New("XDG_RUNTIME_DIR unset and /tmp fallback failed")
	}
	_ = os.Remove(path) // clean stale
	listener, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("listen %s: %w", path, err)
	}
	srv := &controlServer{log: log, path: path, listen: listener, state: state}
	go srv.acceptLoop()
	log.Info("control socket listening", "path", path)
	return srv, nil
}

func controlSocketPath() string {
	if xdg := os.Getenv("XDG_RUNTIME_DIR"); xdg != "" {
		return filepath.Join(xdg, "flexkb-imed.sock")
	}
	return filepath.Join("/tmp", fmt.Sprintf("flexkb-imed-%d.sock", os.Getuid()))
}

func (s *controlServer) close() {
	s.once.Do(func() {
		if s.listen != nil {
			s.listen.Close()
		}
		_ = os.Remove(s.path)
	})
}

func (s *controlServer) acceptLoop() {
	for {
		conn, err := s.listen.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			s.log.Debug("control accept", "err", err)
			continue
		}
		go s.handleConn(conn)
	}
}

// handleConn services one client. Reads JSON requests one per
// line, writes responses one per line. Closes on first error.
func (s *controlServer) handleConn(conn net.Conn) {
	defer conn.Close()
	dec := json.NewDecoder(conn)
	enc := json.NewEncoder(conn)
	for {
		var req struct {
			Op string `json:"op"`
		}
		if err := dec.Decode(&req); err != nil {
			if err == io.EOF {
				return
			}
			s.log.Debug("control decode", "err", err)
			return
		}
		switch req.Op {
		case "status":
			snap := s.state.snapshot()
			if err := enc.Encode(snap); err != nil {
				return
			}
		case "ping":
			if err := enc.Encode(map[string]string{"pong": "ok"}); err != nil {
				return
			}
		case "list-engines":
			components, _ := ibusengines.Discover()
			real := ibusengines.RealEngines(ibusengines.AllEngines(components))
			// Return a compact form — full Component info is
			// available via `flexkb-imed --list-engines` and the
			// XML files themselves; the GUI just needs the
			// pickable summary.
			type engineDTO struct {
				Name      string `json:"name"`
				LongName  string `json:"long_name"`
				Language  string `json:"language"`
				Component string `json:"component"`
			}
			out := make([]engineDTO, 0, len(real))
			for _, e := range real {
				out = append(out, engineDTO{
					Name:      e.Name,
					LongName:  e.LongName,
					Language:  e.Language,
					Component: e.ComponentName,
				})
			}
			if err := enc.Encode(out); err != nil {
				return
			}
		default:
			if err := enc.Encode(map[string]string{"error": "unknown op: " + req.Op}); err != nil {
				return
			}
		}
	}
}
