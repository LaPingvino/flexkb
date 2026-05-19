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

// EngineHost is the optional engine-routing interface. When the
// daemon was constructed with one, ProcessKeyEvent first
// consults the host: if the input context has an engine bound
// AND that engine consumes the keystroke, we skip the in-process
// Session. Otherwise we fall back to the Session.
//
// The shape is a narrow interface (vs. importing internal/ibushost
// directly) to avoid a cycle and to let tests substitute a
// fake host.
type EngineHost interface {
	// EngineFor returns the engine bound to an input context, or
	// nil if no engine is selected.
	EngineFor(ctxPath dbus.ObjectPath) Engine
	// SelectEngine binds an input context to a named engine.
	SelectEngine(ctxPath dbus.ObjectPath, engineName string) (Engine, error)
	// ReleaseEngine drops the binding on input-context destroy.
	ReleaseEngine(ctxPath dbus.ObjectPath)
	// RegisterComponent is invoked when an engine subprocess
	// calls RegisterComponent on our IBus service.
	RegisterComponent(componentName string, engineNames []string, objectPath dbus.ObjectPath)
	// NotifyFocusIn / NotifyFocusOut tell the host which input
	// context is currently focused on a given engine.
	// Engine-side signals (CommitText, UpdatePreedit, …) route
	// to the focused context.
	NotifyFocusIn(engineName string, ctxPath dbus.ObjectPath)
	NotifyFocusOut(engineName string)
}

// Engine is the narrow client-side view of one hosted engine.
// Just enough surface for InputContext to forward keystrokes
// and focus events; the full client surface lives in
// internal/ibushost.
type Engine interface {
	Name() string
	Connected() bool
	ProcessKeyEvent(keyval, keycode, state uint32) (bool, error)
	FocusIn() error
	FocusOut() error
	Reset() error
}

// V2Router is the optional Wayland-v2-rebroadcast routing
// interface. When the daemon was started with the v2 side-socket
// listener enabled, ProcessKeyEvent consults the router FIRST
// (ahead of the EngineHost). If a downstream v2 IME has grabbed
// the keyboard and accepts the keystroke (returns consumed=true),
// we suppress local processing — the IME drives the commit /
// preedit asynchronously via the InputContext.Emit* hooks the
// daemon wires up.
//
// The shape is narrow on purpose, matching EngineHost: the v2
// transport's full surface lives in internal/wlim + internal/wlserver;
// here we only need "is there an active grab routing for this
// context, and would it like this key?".
type V2Router interface {
	// HasActiveGrab returns true when a downstream v2 IME has
	// grabbed the keyboard. If false, ProcessKeyEvent skips this
	// tier and falls through to the EngineHost / Session path.
	HasActiveGrab() bool
	// RouteKey forwards a keystroke to the active grab. keyval +
	// keycode + state use the same conventions as the rest of
	// ProcessKeyEvent. consumed=true means "the v2 IME will
	// handle this; do not double-dispatch via local tiers." err
	// is returned for transport failures (broken socket, etc.) so
	// the caller can log and fall back rather than dropping the
	// keystroke entirely.
	RouteKey(ctxPath dbus.ObjectPath, keyval, keycode, state uint32) (consumed bool, err error)
	// NotifyFocusIn / NotifyFocusOut bind the focused input context
	// to the v2 IME so commit_string responses route back to the
	// right path. Mirror of EngineHost's focus methods.
	NotifyFocusIn(ctxPath dbus.ObjectPath)
	NotifyFocusOut(ctxPath dbus.ObjectPath)
}

// Server is one running ibus-side dbus daemon. Start it once
// per process; it owns a goroutine for the conn's reader.
type Server struct {
	conn    *dbus.Conn
	log     *slog.Logger
	factory SessionFactory
	busName string
	host    EngineHost // optional — nil means "no external engine routing"
	v2      V2Router   // optional — nil means "no v2 rebroadcast"

	mu       sync.Mutex
	contexts map[dbus.ObjectPath]*InputContext
	nextID   uint64
}

// SetEngineHost attaches an EngineHost to this server. Call
// before Start so the RegisterComponent service method has the
// host available when engines register. nil disables routing.
func (s *Server) SetEngineHost(h EngineHost) {
	s.host = h
}

// SetV2Router attaches a Wayland-v2-rebroadcast router. Call
// before Start so the first ProcessKeyEvent already routes via
// the v2 path when a downstream IME has grabbed. Passing nil
// disables the tier — server then behaves exactly as it did
// before phase 3.1.
func (s *Server) SetV2Router(r V2Router) {
	s.v2 = r
}

// Conn exposes the underlying dbus connection for callers that
// need to share it (e.g. the EngineHost lives on the same
// session bus and uses Conn to call engines back).
func (s *Server) Conn() *dbus.Conn { return s.conn }

// ContextEmitter is the small surface ibushost needs to push
// engine-side signals back out to IM clients. Defined in
// ibushost (the side that does the dispatching); Server
// satisfies it by walking the contexts map for each ctxPath.
// Mirror declaration here lets the daemon pass *Server to
// host.SetContextEmitter without an adapter — Go auto-satisfies
// the structural interface.

// EmitCommitText re-emits an engine's CommitText signal on the
// input context's dbus path. Skips if no context exists at the
// path (race against Destroy is benign — engine signals arrive
// asynchronously and a recently-destroyed context is fine to
// drop).
func (s *Server) EmitCommitText(ctxPath dbus.ObjectPath, text string) {
	if ic := s.lookupContext(ctxPath); ic != nil {
		ic.emitCommitText(text)
	}
}

// EmitUpdatePreedit re-emits UpdatePreeditText on the context's
// path. cursor is the byte offset into text; visible toggles
// the preedit's display state.
func (s *Server) EmitUpdatePreedit(ctxPath dbus.ObjectPath, text string, cursor uint32, visible bool) {
	if ic := s.lookupContext(ctxPath); ic != nil {
		_ = visible // ibus's UpdatePreeditText carries visible;
		// flexkb's emitUpdatePreedit currently hard-codes true.
		// When we add the hide/show distinction explicitly,
		// this is where it goes.
		ic.emitUpdatePreedit(text, cursor)
	}
}

// EmitHidePreedit re-emits HidePreeditText.
func (s *Server) EmitHidePreedit(ctxPath dbus.ObjectPath) {
	if ic := s.lookupContext(ctxPath); ic != nil {
		ic.emitHidePreedit()
	}
}

// EmitShowPreedit re-emits ShowPreeditText. The IBus.InputContext
// interface has this as a separate signal; we emit unconditionally
// and let the IM client take care of state.
func (s *Server) EmitShowPreedit(ctxPath dbus.ObjectPath) {
	if ic := s.lookupContext(ctxPath); ic != nil {
		s.conn.Emit(ic.path, "org.freedesktop.IBus.InputContext.ShowPreeditText")
	}
}

// EmitForwardKeyEvent re-emits ForwardKeyEvent on the context's
// path. ibus engines emit this when they decide NOT to consume
// a keystroke but want to express "treat it as if you saw it
// fresh" — useful for engines that pre-process keys.
func (s *Server) EmitForwardKeyEvent(ctxPath dbus.ObjectPath, keyval, keycode, state uint32) {
	if ic := s.lookupContext(ctxPath); ic != nil {
		s.conn.Emit(ic.path,
			"org.freedesktop.IBus.InputContext.ForwardKeyEvent",
			keyval, keycode, state)
	}
}

// lookupContext returns the InputContext at ctxPath under the
// server lock, or nil if no context lives there. Used by the
// Emit* methods to translate engine signals to per-context
// signals.
func (s *Server) lookupContext(ctxPath dbus.ObjectPath) *InputContext {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.contexts[ctxPath]
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

// RegisterComponent is called by engine subprocesses (or any
// process implementing an engine) when they want to expose
// engines through this daemon. The component variant carries
// the full descriptor — we extract the engines list and bind
// each engine's name to its caller's object path via the
// configured EngineHost.
//
// If no EngineHost is configured the daemon ignores the
// registration but doesn't error; that lets engines coexist
// with flexkb-imed without forcing it to host them.
func (s *service) RegisterComponent(component dbus.Variant) *dbus.Error {
	if s.srv.host == nil {
		// Quiet success — engine remains usable via the
		// session bus's own dispatch; we just don't route
		// through it.
		return nil
	}
	// The component variant's signature is `(sa{sv}sav)` in
	// real ibus — opaque enough that we parse it defensively.
	// For v1 we extract just what we need: the component name
	// and the engines list. Failures to parse are logged and
	// dropped — the engine will retry or be discovered via
	// /usr/share/ibus/component on its next startup.
	name, engines, ok := decodeIBusComponent(component)
	if !ok {
		s.srv.log.Warn("RegisterComponent: malformed component variant")
		return nil
	}
	// We use the sender's path as the object root; engines
	// expose their service at /org/freedesktop/IBus/Engine.
	objectPath := dbus.ObjectPath("/org/freedesktop/IBus/Engine")
	s.srv.host.RegisterComponent(name, engines, objectPath)
	return nil
}

// decodeIBusComponent peels a `(sa{sv}sav)`-shaped IBusComponent
// dbus variant. Real ibus uses Variant<IBusComponent> with a
// nested struct; we only need the top-level name and the engines
// list. Returns false on shape mismatch (so callers can log
// and continue without disrupting other clients).
func decodeIBusComponent(v dbus.Variant) (componentName string, engineNames []string, ok bool) {
	raw := v.Value()
	// Try the common shape first: anonymous struct with first
	// field = name. We'll let dbus's reflection handle the rest.
	type ibusComponent struct {
		Name        string
		Attributes  map[string]dbus.Variant
		Description string
		Engines     []dbus.Variant
	}
	var c ibusComponent
	if err := dbus.Store([]interface{}{raw}, &c); err == nil {
		for _, ev := range c.Engines {
			type ibusEngineDesc struct {
				Name string
			}
			var ed ibusEngineDesc
			if err := dbus.Store([]interface{}{ev.Value()}, &ed); err == nil && ed.Name != "" {
				engineNames = append(engineNames, ed.Name)
			}
		}
		return c.Name, engineNames, true
	}
	return "", nil, false
}
