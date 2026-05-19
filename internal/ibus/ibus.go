// Package ibus implements the org.freedesktop.IBus dbus interface
// server-side so flexkb-imed can replace ibus-daemon for clients
// that already speak that protocol — GNOME's Mutter, X11 apps,
// GTK/Qt apps that loaded the ibus-IM module. The same
// imsession.Session abstraction the Wayland v2 transport uses
// drives this one; only the wire layer differs.
//
// References (verified against the live introspect on a system
// with ibus-daemon running, then cross-checked against
// fcitx5/src/frontend/dbusfrontend/ for edge cases):
//
//   org.freedesktop.IBus              — service object, factory for input contexts
//     methods we implement:
//       CreateInputContext(client_name) → object_path
//       Ping(v) → v
//       GetAddress() → s  (deprecated but still called by clients)
//     methods we stub:
//       ListEngines, ListActiveEngines, GetEnginesByNames — enumerate engines
//       SetGlobalEngine, GetGlobalEngine — engine selection
//
//   org.freedesktop.IBus.InputContext — per-app input context
//     methods we implement:
//       ProcessKeyEvent(keyval, keycode, state) → bool (true = consumed)
//       FocusIn, FocusOut
//       Reset
//       SetCapabilities, SetCursorLocation
//       SetEngine
//     signals we emit:
//       CommitText(IBusText)
//       UpdatePreeditText(IBusText, cursor_pos, visible)
//       ShowPreeditText, HidePreeditText
//       ForwardKeyEvent(keyval, keycode, state)
//
// The IBusText variant type is the messy bit. It's a serialized
// dbus struct with the signature "(sa{sv}sv)" carrying string +
// metadata; we ship a minimal flat form (no attributes) since
// flexkb doesn't emit colored / formatted preedit yet.
package ibus

import (
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"

	"github.com/godbus/dbus/v5"

	"github.com/lapingvino/flexkb/internal/imsession"
)

// BusName is the well-known dbus service name we claim. Owning
// this name effectively makes flexkb-imed the ibus daemon for
// the user session — any client that does `dbus.SessionBus().
// Object("org.freedesktop.IBus", ...)` reaches us.
const BusName = "org.freedesktop.IBus"

// ServicePath is where the top-level org.freedesktop.IBus
// interface lives.
const ServicePath = "/org/freedesktop/IBus"

// SessionFactory is the caller-supplied hook that produces a
// fresh imsession.Session for a newly-created input context.
// One Session per app focus context. flexkb-imed's main loop
// supplies a factory that builds the configured Resolver + IM.
type SessionFactory func() (*imsession.Session, error)

// Server is one running ibus-side dbus daemon. Start it once
// per process; it owns a goroutine for the conn's reader.
type Server struct {
	conn    *dbus.Conn
	log     *slog.Logger
	factory SessionFactory
	busName string

	mu       sync.Mutex
	contexts map[dbus.ObjectPath]*InputContext
	nextID   uint64
}

// New constructs a Server bound to the session bus. busName is
// the dbus well-known name to claim — pass "" to use the
// standard BusName ("org.freedesktop.IBus"). Tests override it
// to avoid colliding with a running ibus-daemon.
func New(log *slog.Logger, factory SessionFactory) (*Server, error) {
	return NewWithName(log, factory, BusName)
}

// NewWithName is like New but lets the caller override the
// claimed dbus name. Mostly useful for tests.
func NewWithName(log *slog.Logger, factory SessionFactory, busName string) (*Server, error) {
	if busName == "" {
		busName = BusName
	}
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		return nil, fmt.Errorf("connect session bus: %w", err)
	}
	return &Server{
		conn:     conn,
		log:      log,
		factory:  factory,
		busName:  busName,
		contexts: map[dbus.ObjectPath]*InputContext{},
	}, nil
}

// BusName returns the dbus name this server is registered under.
// Useful for tests that need to construct client calls.
func (s *Server) BusName() string { return s.busName }

// Start claims the server's dbus name on the session bus and
// exports the service object. Use replace=true to forcibly take
// the name from an existing owner (the dbus rules require that
// the previous owner allow replacement; ibus-daemon does).
// replace=false fails when the name is already owned.
func (s *Server) Start(replace bool) error {
	flags := dbus.NameFlagDoNotQueue
	if replace {
		flags |= dbus.NameFlagReplaceExisting
		flags |= dbus.NameFlagAllowReplacement
	}
	reply, err := s.conn.RequestName(s.busName, flags)
	if err != nil {
		return fmt.Errorf("request name: %w", err)
	}
	switch reply {
	case dbus.RequestNameReplyPrimaryOwner:
		s.log.Info("became primary owner", "name", s.busName)
	case dbus.RequestNameReplyAlreadyOwner:
		s.log.Info("already owned name", "name", s.busName)
	default:
		return fmt.Errorf("name request denied: another process owns %s "+
			"(start with --ibus replace to take over)", s.busName)
	}

	// Export the top-level service. Its only job is to mint new
	// input-context objects.
	svc := &service{srv: s}
	if err := s.conn.Export(svc, ServicePath, "org.freedesktop.IBus"); err != nil {
		return fmt.Errorf("export service: %w", err)
	}
	if err := s.conn.Export(svc, ServicePath, "org.freedesktop.IBus.Service"); err != nil {
		return fmt.Errorf("export ibus.Service: %w", err)
	}
	return nil
}

// Close releases the bus name and cleans up.
func (s *Server) Close() error {
	_, _ = s.conn.ReleaseName(s.busName)
	return s.conn.Close()
}

// allocPath produces a fresh, unique input-context object path.
// Matching ibus's path scheme (/org/freedesktop/IBus/InputContext_NN)
// makes debug tools and busctl introspects look natural.
func (s *Server) allocPath() dbus.ObjectPath {
	id := atomic.AddUint64(&s.nextID, 1)
	return dbus.ObjectPath(fmt.Sprintf("%s/InputContext_%d", ServicePath, id))
}

// --- service (org.freedesktop.IBus) ---

// service is the dbus-exported object at ServicePath. Methods
// match the interface name (CamelCase Go → CamelCase dbus).
type service struct {
	srv *Server
}

// CreateInputContext is the entry point every app calls when it
// gains focus. We mint a new InputContext, export it, return its
// object path. The client then issues method calls and listens
// for signals on that path.
func (s *service) CreateInputContext(name string) (dbus.ObjectPath, *dbus.Error) {
	path := s.srv.allocPath()
	sess, err := s.srv.factory()
	if err != nil {
		return "", dbus.NewError("org.freedesktop.IBus.Error.NoSession",
			[]interface{}{fmt.Sprintf("create session: %v", err)})
	}
	ic := &InputContext{
		srv:    s.srv,
		path:   path,
		client: name,
		sess:   sess,
	}
	if err := s.srv.conn.Export(ic, path, "org.freedesktop.IBus.InputContext"); err != nil {
		return "", dbus.NewError("org.freedesktop.IBus.Error.ExportFailed",
			[]interface{}{err.Error()})
	}
	if err := s.srv.conn.Export(ic, path, "org.freedesktop.IBus.Service"); err != nil {
		return "", dbus.NewError("org.freedesktop.IBus.Error.ExportFailed",
			[]interface{}{err.Error()})
	}
	s.srv.mu.Lock()
	s.srv.contexts[path] = ic
	s.srv.mu.Unlock()
	s.srv.log.Info("created input context", "client", name, "path", path)
	return path, nil
}

// Ping is the keepalive method ibus clients use; the variant
// passed in is echoed back. ibus uses the variant for protocol
// extensibility (future versions might pass structured payloads);
// for now we echo whatever the client sent.
func (s *service) Ping(data dbus.Variant) (dbus.Variant, *dbus.Error) {
	return data, nil
}

// GetAddress is deprecated but still called by older ibus
// clients during connection setup. Returns the bus address —
// for our case "session bus" suffices.
func (s *service) GetAddress() (string, *dbus.Error) {
	return string(s.srv.conn.BusObject().Path()), nil
}

// Destroy is the org.freedesktop.IBus.Service teardown method.
// Top-level service Destroy means "shut down the whole daemon"
// — we treat it as ill-advised RPC and refuse.
func (s *service) Destroy() *dbus.Error {
	return dbus.NewError("org.freedesktop.IBus.Error.Forbidden",
		[]interface{}{"top-level IBus service refuses Destroy from clients"})
}
