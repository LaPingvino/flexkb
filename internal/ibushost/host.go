// Package ibushost is the engine-multiplexing layer of
// flexkb-imed. It implements the *opposite* side of the ibus
// dbus protocol from internal/ibus: where that package is the
// server flexkb-imed presents to IM clients (Mutter, GTK, Qt),
// this one is the client flexkb-imed becomes when talking to
// external engine binaries (ibus-engine-libpinyin,
// ibus-engine-anthy, ibus-engine-mozc, …).
//
// Architectural shape:
//
//   IM client (Mutter / app) ──┐
//                              ├──→ flexkb-imed (internal/ibus server)
//                              │       │
//                              │       │ if input context's
//                              │       │ active engine is hosted,
//                              │       │ route ProcessKeyEvent
//                              │       ▼
//                              │     ibushost.Host
//                              │       │
//                              │       │ spawn + dbus call
//                              │       ▼
//                              └──── ibus-engine-libpinyin
//                                    (subprocess we manage)
//
// The engine's CommitText / UpdatePreedit / ForwardKeyEvent
// signals come back over dbus; ibushost forwards them to the
// originating input context, which re-emits them on its own
// signal channel to the IM client. Same trick fcitx5's dbus
// frontend uses, same shape we built in 5.2 with the directions
// flipped.
//
// Lifecycle:
//   * Host is constructed at daemon startup.
//   * Engines are spawned lazily — when an input context first
//     SetEngine's to one. We keep them running once started
//     (CJK engines have meaningful warm-up costs: dictionary
//     load, language model init).
//   * On daemon shutdown, Host.Close terminates all spawned
//     engine subprocesses.
package ibushost

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"

	"github.com/lapingvino/flexkb/internal/ibus"
	"github.com/lapingvino/flexkb/internal/ibusengines"
)

// Host owns the relationship between flexkb-imed's input contexts
// and any external ibus engines we've started. Safe for
// concurrent calls — each public method takes the inner mutex
// for its critical section.
type Host struct {
	conn *dbus.Conn
	log  *slog.Logger

	// catalog is the inventory of engines flexkb-imed knows
	// could exist on this system. Populated at construction
	// from ibusengines.Discover so we can answer "is X
	// hostable?" without re-walking the filesystem.
	catalog map[string]ibusengines.Engine

	mu      sync.Mutex
	engines map[string]*Engine          // by engine name
	routes  map[dbus.ObjectPath]*Engine // input-context path → engine
}

// New builds a Host on the supplied dbus connection. The
// connection is shared with internal/ibus's Server — engines
// register with us (the IBus service) and we call them back via
// the same conn.
func New(conn *dbus.Conn, log *slog.Logger) *Host {
	cats := map[string]ibusengines.Engine{}
	components, _ := ibusengines.Discover()
	for _, e := range ibusengines.AllEngines(components) {
		cats[e.Name] = e
	}
	return &Host{
		conn:    conn,
		log:     log,
		catalog: cats,
		engines: map[string]*Engine{},
		routes:  map[dbus.ObjectPath]*Engine{},
	}
}

// CatalogSize reports the number of engines this Host is aware
// of from the system's ibus component XML. Mostly for logs.
func (h *Host) CatalogSize() int {
	return len(h.catalog)
}

// AvailableEngines returns the engine names that exist in the
// catalog. Used by the daemon's control socket / GUI to surface
// "what's hostable" to users.
func (h *Host) AvailableEngines() []string {
	out := make([]string, 0, len(h.catalog))
	for n := range h.catalog {
		out = append(out, n)
	}
	return out
}

// SelectEngine binds an input context to a hosted engine. The
// engine is spawned on first use and reused thereafter. Returns
// the Engine handle so the caller can issue FocusIn / direct
// method calls. Idempotent: re-selecting the same engine for
// the same context is a no-op.
//
// Returns ibus.Engine (the interface) so ibushost.Host satisfies
// ibus.EngineHost directly — flexkb-imed wires them together
// without an adapter.
func (h *Host) SelectEngine(ctxPath dbus.ObjectPath, engineName string) (ibus.Engine, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	cat, ok := h.catalog[engineName]
	if !ok {
		return nil, fmt.Errorf("engine %q not in catalog (run `flexkb-imed --list-engines` to see what's available)", engineName)
	}
	if existing, ok := h.routes[ctxPath]; ok && existing.name == engineName {
		return existing, nil
	}
	eng, ok := h.engines[engineName]
	if !ok {
		var err error
		eng, err = h.spawnEngine(cat)
		if err != nil {
			return nil, fmt.Errorf("spawn %s: %w", engineName, err)
		}
		h.engines[engineName] = eng
	}
	h.routes[ctxPath] = eng
	return eng, nil
}

// Compile-time check that *Host satisfies ibus.EngineHost — if
// the interface ever changes, the build fails fast at the wire
// point rather than via a missing-method error at runtime.
var _ ibus.EngineHost = (*Host)(nil)

// ReleaseEngine drops the (context → engine) binding. Called by
// internal/ibus's InputContext.Destroy when an app disconnects.
// We don't kill the engine subprocess on release because other
// contexts may still be using it; subprocess shutdown happens
// in Close.
func (h *Host) ReleaseEngine(ctxPath dbus.ObjectPath) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.routes, ctxPath)
}

// EngineFor returns the engine currently bound to ctxPath, or
// nil if no engine is selected. Used by internal/ibus's
// InputContext.ProcessKeyEvent: when an engine is bound we
// route through it instead of (or in addition to) the in-process
// imsession.Session.
func (h *Host) EngineFor(ctxPath dbus.ObjectPath) ibus.Engine {
	h.mu.Lock()
	defer h.mu.Unlock()
	eng := h.routes[ctxPath]
	if eng == nil {
		// Return a typed nil through the interface — callers
		// check Connected() before issuing calls. Returning a
		// boxed nil would make nil-checks at the call site
		// confusing.
		return nil
	}
	return eng
}

// Close terminates every spawned engine subprocess. The daemon
// calls this during shutdown. Engine binaries handle SIGTERM
// gracefully; if any take longer than 2 seconds we SIGKILL
// to avoid stalling shutdown.
func (h *Host) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for name, eng := range h.engines {
		if eng.process == nil {
			continue
		}
		h.log.Info("terminating engine", "name", name, "pid", eng.process.Pid)
		_ = eng.process.Signal(os.Interrupt)
	}
	deadline := time.Now().Add(2 * time.Second)
	for _, eng := range h.engines {
		if eng.process == nil {
			continue
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			_ = eng.process.Kill()
			continue
		}
		done := make(chan struct{})
		go func(p *os.Process) {
			p.Wait()
			close(done)
		}(eng.process)
		select {
		case <-done:
		case <-time.After(remaining):
			_ = eng.process.Kill()
		}
	}
	h.engines = map[string]*Engine{}
	h.routes = map[dbus.ObjectPath]*Engine{}
}

// spawnEngine starts an engine binary as a subprocess. Real
// ibus engines connect to ibus-daemon via IBUS_ADDRESS or
// ~/.config/ibus/bus/HASH; we hand them the session bus
// address since flexkb-imed claims org.freedesktop.IBus on the
// session bus, and engines find us by well-known dbus name.
//
// Returns a partially-initialised Engine. The path/obj fields
// stay zero until the engine's subprocess calls
// RegisterComponent on our IBus service (which arrives via
// internal/ibus.service.RegisterComponent → Host.RegisterComponent).
// Callers should call eng.Connected() before issuing method
// calls; until then, ProcessKeyEvent NoOps and routing falls
// back to the in-process Session.
//
// Process lifecycle: spawnEngine starts the process but doesn't
// wait for it. The Wait goroutine logs exit status so a
// crashed engine surfaces in journal. Host.Close terminates
// every spawned process during shutdown.
func (h *Host) spawnEngine(cat ibusengines.Engine) (*Engine, error) {
	if cat.ExecPath == "" {
		return nil, fmt.Errorf("engine %q has no exec path in component XML", cat.Name)
	}
	if _, err := os.Stat(cat.ExecPath); err != nil {
		return nil, fmt.Errorf("engine %q exec %q not found: %w", cat.Name, cat.ExecPath, err)
	}

	// Spawn. --ibus tells the engine "you're under an
	// ibus-daemon" (vs. --xim or --daemonize). The session-bus
	// address from DBUS_SESSION_BUS_ADDRESS is what the engine
	// will connect to; we don't override since we're already
	// on the session bus.
	ctx := context.Background()
	cmd := exec.CommandContext(ctx, cat.ExecPath, "--ibus")
	// Pass through the daemon's env; the only IBUS-relevant
	// bits are DBUS_SESSION_BUS_ADDRESS and DISPLAY for X11
	// engines that may pull layout state from there.
	cmd.Env = os.Environ()
	// Stdout/stderr captured to the daemon's logger via a
	// pipe — engines occasionally log to stderr, surfacing
	// in our journal helps debugging without redirecting
	// to a separate file.
	cmd.Stdout = enginePipe(h.log, cat.Name, "stdout")
	cmd.Stderr = enginePipe(h.log, cat.Name, "stderr")

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", cat.ExecPath, err)
	}
	h.log.Info("spawned engine", "name", cat.Name, "exec", cat.ExecPath, "pid", cmd.Process.Pid)

	eng := &Engine{
		host:    h,
		name:    cat.Name,
		cat:     cat,
		process: cmd.Process,
	}
	// Reap the process in the background so a crashed engine
	// doesn't leave a zombie. The log line is the entire
	// "did this engine survive its first focus?" signal until
	// we add structured engine-health reporting.
	go func() {
		err := cmd.Wait()
		h.mu.Lock()
		delete(h.engines, cat.Name)
		// Drop any routes that pointed at this engine.
		for ctxPath, e := range h.routes {
			if e == eng {
				delete(h.routes, ctxPath)
			}
		}
		h.mu.Unlock()
		if err != nil {
			h.log.Error("engine exited", "name", cat.Name, "err", err)
		} else {
			h.log.Info("engine exited cleanly", "name", cat.Name)
		}
	}()
	return eng, nil
}

// enginePipe builds an io.Writer that forwards every line from
// an engine's stdout/stderr to the daemon's logger with
// consistent fields. log.Debug rather than Info so per-key
// engine chatter doesn't dominate journal output.
type lineWriter struct {
	log    *slog.Logger
	engine string
	stream string
	buf    []byte
}

func enginePipe(log *slog.Logger, engineName, stream string) *lineWriter {
	return &lineWriter{log: log, engine: engineName, stream: stream}
}

func (w *lineWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	for {
		idx := -1
		for i, b := range w.buf {
			if b == '\n' {
				idx = i
				break
			}
		}
		if idx < 0 {
			break
		}
		line := string(w.buf[:idx])
		w.buf = w.buf[idx+1:]
		w.log.Debug("engine output", "engine", w.engine, "stream", w.stream, "line", line)
	}
	return len(p), nil
}

// RegisterComponent is the hook internal/ibus.service calls when
// an engine that we spawned (or that was spawned externally and
// found our bus name) calls RegisterComponent. We complete the
// Engine handle by recording its dbus path so subsequent method
// calls can reach it.
//
// component is the descriptor sent by the engine; it carries
// enough metadata to match against our catalog.
func (h *Host) RegisterComponent(componentName string, engineNames []string, objectPath dbus.ObjectPath) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, engineName := range engineNames {
		// Bind every engine the component owns to a path under
		// its base object. Real ibus uses /org/freedesktop/IBus/
		// Engine/<id> per instance; for v1 we record one path
		// per engine name.
		eng, exists := h.engines[engineName]
		if !exists {
			cat, inCat := h.catalog[engineName]
			if !inCat {
				h.log.Warn("RegisterComponent advertised unknown engine",
					"component", componentName, "engine", engineName)
				continue
			}
			eng = &Engine{host: h, name: engineName, cat: cat}
			h.engines[engineName] = eng
		}
		eng.path = objectPath
		eng.obj = h.conn.Object("org.freedesktop.IBus", objectPath)
		h.log.Info("engine registered",
			"component", componentName, "engine", engineName, "path", objectPath)
	}
}

// Engine is the client-side handle to one ibus engine. Methods
// here translate to dbus calls on the engine's IBus.Engine
// object. All methods return immediately on a not-yet-connected
// engine — callers check Connected() if they care to wait.
type Engine struct {
	host    *Host
	name    string
	cat     ibusengines.Engine
	process *os.Process

	// path + obj are populated by Host.RegisterComponent when
	// the engine reports in. Calls before that NoOp; the
	// in-process imsession.Session still handles the keystroke.
	path dbus.ObjectPath
	obj  dbus.BusObject
}

// Name returns the engine name (e.g. "libpinyin"). Useful for
// logging in the routing layer.
func (e *Engine) Name() string { return e.name }

// Connected reports whether the engine has registered with us
// and is reachable. False on freshly-spawned engines before
// they've called RegisterComponent.
func (e *Engine) Connected() bool { return e.obj != nil }

// ProcessKeyEvent forwards a keystroke to the engine and returns
// whether it consumed it. keyval is the X11 keysym, keycode the
// X11 keycode (Linux scancode + 8), state the X11 modifier mask
// (with bit 0x40000000 = key release).
//
// On a disconnected engine returns false (not consumed) so the
// routing layer falls back to the in-process Session.
func (e *Engine) ProcessKeyEvent(keyval, keycode, state uint32) (bool, error) {
	if !e.Connected() {
		return false, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	var consumed bool
	call := e.obj.CallWithContext(ctx, "org.freedesktop.IBus.Engine.ProcessKeyEvent", 0,
		keyval, keycode, state)
	if err := call.Store(&consumed); err != nil {
		return false, fmt.Errorf("engine ProcessKeyEvent: %w", err)
	}
	return consumed, nil
}

// FocusIn tells the engine the input context is now active. The
// engine may load per-context state at this point.
func (e *Engine) FocusIn() error {
	if !e.Connected() {
		return nil
	}
	return e.obj.Call("org.freedesktop.IBus.Engine.FocusIn", 0).Store()
}

// FocusOut tells the engine the input context lost focus. The
// engine should clear any in-flight preedit.
func (e *Engine) FocusOut() error {
	if !e.Connected() {
		return nil
	}
	return e.obj.Call("org.freedesktop.IBus.Engine.FocusOut", 0).Store()
}

// Reset asks the engine to discard any in-flight state. ibus
// clients call this after major edits (e.g. clipboard paste);
// we forward to the engine.
func (e *Engine) Reset() error {
	if !e.Connected() {
		return nil
	}
	return e.obj.Call("org.freedesktop.IBus.Engine.Reset", 0).Store()
}

