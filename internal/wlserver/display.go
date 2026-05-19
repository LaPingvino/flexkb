package wlserver

import (
	"fmt"
	"log/slog"
	"sync"

	"github.com/lapingvino/flexkb/internal/wlwire"
)

// Display is the server-side wl_display object that lives at
// object ID 1 on every accepted connection. Its request surface
// is exactly mirror-image to the client side:
//
//   request 0: sync (callback new_id)         — server replies with done
//   request 1: get_registry (registry new_id) — server starts emitting global events
//
//   event 0: error (object_id, code, message) — fatal
//   event 1: delete_id (id)                   — acknowledgment for client destroy
type Display struct {
	d *Dispatcher
	r *Registry
}

// RegisterDisplay binds the server-side wl_display at object ID 1.
// Called by the per-connection onAccept hook before Run starts so
// the first request the client sends finds its handler.
func RegisterDisplay(d *Dispatcher, r *Registry) (*Display, error) {
	disp := &Display{d: d, r: r}
	if err := d.Register(displayObjectID, disp); err != nil {
		return nil, err
	}
	return disp, nil
}

// HandleRequest dispatches the two wl_display requests.
func (s *Display) HandleRequest(opcode uint16, body []byte, _ []int) error {
	dec := wlwire.NewDecoder(body)
	switch opcode {
	case 0: // sync(callback)
		cbID := dec.Uint()
		// We don't need to register a handler for the callback —
		// it never receives any requests. We just emit done and
		// then delete_id to clean up its slot on the client side.
		body := wlwire.NewEncoder()
		// Wayland uses a 32-bit serial echoed back via the done
		// event; we keep a per-display counter. Real compositors
		// use the wl_callback's id; for us, a monotonic counter
		// is enough since downstream clients don't correlate.
		body.PutUint(s.nextSerial())
		// wl_callback event 0 = done(callback_data).
		if err := s.d.Send(cbID, 0, body.Bytes(), nil); err != nil {
			return fmt.Errorf("sync done emit: %w", err)
		}
		// And then delete_id so the client cleans up.
		delBody := wlwire.NewEncoder()
		delBody.PutUint(cbID)
		// wl_display event 1 = delete_id.
		return s.d.Send(displayObjectID, 1, delBody.Bytes(), nil)
	case 1: // get_registry(registry new_id)
		regID := dec.Uint()
		// The registry was created by RegisterDisplay; we bind it
		// to this newly-allocated client-side ID and emit the
		// global advertisements.
		if err := s.r.attach(regID); err != nil {
			return err
		}
		s.r.emitAllGlobals(regID)
		return nil
	default:
		s.d.log.Debug("wl_display unknown request", "opcode", opcode)
		return nil
	}
}

var syncSerialCounter struct {
	mu  sync.Mutex
	val uint32
}

func (s *Display) nextSerial() uint32 {
	syncSerialCounter.mu.Lock()
	defer syncSerialCounter.mu.Unlock()
	syncSerialCounter.val++
	return syncSerialCounter.val
}

// SendError emits a wl_display::error event. This is the
// protocol's fatal-error signal — the client tears down after
// receiving it. We use it for malformed requests, version
// mismatches, and missing required fds.
func (s *Display) SendError(offendingID, code uint32, msg string) error {
	body := wlwire.NewEncoder()
	body.PutUint(offendingID)
	body.PutUint(code)
	body.PutString(msg)
	// wl_display event 0 = error.
	return s.d.Send(displayObjectID, 0, body.Bytes(), nil)
}

// --- registry ---

// Registry mirrors wl_registry on the server side. Globals get
// advertised when a client binds the registry; bind requests get
// serviced by invoking the OnBind callback supplied to AddGlobal.
//
//   request 0: bind (name, interface, version, new_id)
//   event 0: global (name, interface, version)
//   event 1: global_remove (name)
type Registry struct {
	d   *Dispatcher
	log *slog.Logger

	mu       sync.Mutex
	nextName uint32
	globals  map[uint32]*global
	// id is the client-allocated object id for this connection's
	// registry. Set via attach when the client sends get_registry.
	// Zero means "client hasn't requested the registry yet".
	id uint32
}

type global struct {
	iface   string
	version uint32
	onBind  GlobalBindHandler
}

// GlobalBindHandler is invoked when a client binds a global the
// server has advertised. newID is the client-allocated object id;
// the handler MUST register a per-binding RequestHandler at that
// ID before returning, otherwise subsequent requests to that
// object will get logged-and-dropped.
type GlobalBindHandler func(d *Dispatcher, newID uint32, version uint32) error

// NewRegistry constructs an unattached registry. Add globals with
// AddGlobal before any connection's wl_display::get_registry
// arrives; new globals can also be added later (the registry will
// emit a global event to any already-attached registry).
func NewRegistry(d *Dispatcher) *Registry {
	return &Registry{
		d:        d,
		log:      d.log,
		nextName: 1,
		globals:  map[uint32]*global{},
	}
}

// AddGlobal adds a global to the registry's advertisement list.
// Returns the assigned "name" (a numeric handle the client sees
// in the global event). If the registry is already attached, the
// new global is also advertised immediately.
func (r *Registry) AddGlobal(iface string, version uint32, onBind GlobalBindHandler) uint32 {
	r.mu.Lock()
	defer r.mu.Unlock()
	name := r.nextName
	r.nextName++
	r.globals[name] = &global{iface: iface, version: version, onBind: onBind}
	if r.id != 0 {
		// Already attached — emit the global event now.
		// Drop the lock while sending, then reacquire is too
		// fiddly for a fast path; we hold it through the wire
		// write because attach also holds it during emitAll.
		// Wayland wire writes are cheap (one syscall) so the
		// contention impact is negligible.
		r.emitGlobalLocked(r.id, name, r.globals[name])
	}
	return name
}

// RemoveGlobal emits a global_remove event and drops the global
// from the advertisement list. Used when a hosted IME goes away
// at runtime (engines registering/unregistering).
func (r *Registry) RemoveGlobal(name uint32) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.globals[name]; !ok {
		return
	}
	delete(r.globals, name)
	if r.id == 0 {
		return
	}
	body := wlwire.NewEncoder()
	body.PutUint(name)
	// wl_registry event 1 = global_remove.
	if err := r.d.Send(r.id, 1, body.Bytes(), nil); err != nil {
		r.log.Debug("global_remove send failed", "err", err)
	}
}

// attach binds the registry to a client-allocated object id from a
// get_registry request. Idempotent: a second get_registry on the
// same connection re-attaches to the new id.
func (r *Registry) attach(id uint32) error {
	r.mu.Lock()
	r.id = id
	r.mu.Unlock()
	return r.d.Register(id, r)
}

// emitAllGlobals sends the initial global events for every
// registered global. Called right after attach.
func (r *Registry) emitAllGlobals(id uint32) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for name, g := range r.globals {
		r.emitGlobalLocked(id, name, g)
	}
}

// emitGlobalLocked emits one wl_registry::global event. Must be
// called with r.mu held.
func (r *Registry) emitGlobalLocked(regID, name uint32, g *global) {
	body := wlwire.NewEncoder()
	body.PutUint(name)
	body.PutString(g.iface)
	body.PutUint(g.version)
	// wl_registry event 0 = global.
	if err := r.d.Send(regID, 0, body.Bytes(), nil); err != nil {
		r.log.Debug("global emit failed", "iface", g.iface, "err", err)
	}
}

// HandleRequest services bind requests from the client.
func (r *Registry) HandleRequest(opcode uint16, body []byte, _ []int) error {
	dec := wlwire.NewDecoder(body)
	switch opcode {
	case 0: // bind(name, interface, version, new_id)
		name := dec.Uint()
		iface := dec.String()
		version := dec.Uint()
		newID := dec.Uint()
		r.mu.Lock()
		g, ok := r.globals[name]
		r.mu.Unlock()
		if !ok {
			return fmt.Errorf("wl_registry::bind unknown name %d (iface=%s)", name, iface)
		}
		if g.iface != iface {
			return fmt.Errorf("wl_registry::bind name %d declared %s, client asked %s", name, g.iface, iface)
		}
		if version > g.version {
			version = g.version
		}
		if g.onBind == nil {
			return fmt.Errorf("wl_registry::bind no handler for %s", iface)
		}
		return g.onBind(r.d, newID, version)
	default:
		r.log.Debug("wl_registry unknown request", "opcode", opcode)
		return nil
	}
}
