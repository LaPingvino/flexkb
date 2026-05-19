package wlclient

import (
	"fmt"
	"log/slog"

	"github.com/lapingvino/flexkb/internal/wlwire"
)

// Display is the bootstrap object (ID 1) every Wayland client
// starts with. Its core protocol surface is small — sync for
// round-tripping, get_registry for global discovery, error and
// delete_id events from the server.
//
// References (the wl_display interface in /usr/share/wayland/
// wayland.xml):
//   request 0: sync   (new_id callback)
//   request 1: get_registry (new_id registry)
//   event 0: error   (object_id, code, message)
//   event 1: delete_id (id)
type Display struct {
	d *Dispatcher
}

// ConnectDisplay wraps the well-known wl_display at object ID 1
// and registers its event handler. The Dispatcher must have been
// freshly constructed (no other object at ID 1) — Bootstrap is
// the very first thing a client does after Dial.
func ConnectDisplay(d *Dispatcher) (*Display, error) {
	disp := &Display{d: d}
	if err := d.Register(displayObjectID, disp); err != nil {
		return nil, err
	}
	return disp, nil
}

// HandleEvent dispatches the two wl_display events. The error
// event is fatal — by spec the server has decided the client is
// misbehaving and closes the connection after sending. We surface
// it as an error from the dispatch loop so the daemon tears down
// cleanly.
func (d *Display) HandleEvent(opcode uint16, body []byte, _ []int) error {
	dec := wlwire.NewDecoder(body)
	switch opcode {
	case 0:
		offendingID := dec.Uint()
		code := dec.Uint()
		msg := dec.String()
		return fmt.Errorf("wl_display::error object=%d code=%d %q", offendingID, code, msg)
	case 1:
		id := dec.Uint()
		d.d.Unregister(id)
		return nil
	default:
		// Forward-compat: future protocol additions show up as
		// unknown opcodes. Spec says ignore them.
		d.d.log.Debug("wl_display unknown event", "opcode", opcode)
		return nil
	}
}

// GetRegistry asks the server to expose its global registry as a
// new object and returns the Registry binding. The dispatch table
// has the new ID registered before the call returns so subsequent
// global events route correctly.
func (d *Display) GetRegistry(onGlobal func(name uint32, iface string, version uint32), onGlobalRemove func(name uint32)) (*Registry, error) {
	regID := d.d.NewID()
	reg := &Registry{
		d:              d.d,
		id:             regID,
		onGlobal:       onGlobal,
		onGlobalRemove: onGlobalRemove,
	}
	if err := d.d.Register(regID, reg); err != nil {
		return nil, err
	}
	body := wlwire.NewEncoder()
	body.PutUint(regID)
	// wl_display request 1 = get_registry.
	if err := d.d.Send(displayObjectID, 1, body.Bytes(), nil); err != nil {
		return nil, fmt.Errorf("wl_display::get_registry send: %w", err)
	}
	return reg, nil
}

// Sync issues a sync request: the server replies with a "done"
// event on a fresh callback object, which lets the client detect
// when all prior requests have been processed. Used right after
// GetRegistry to wait for the initial flurry of global events
// before deciding which interfaces to bind.
//
// The serial argument echoes back through the callback's done
// event so callers can correlate multiple in-flight syncs.
func (d *Display) Sync(serial uint32, done func(callbackData uint32)) error {
	cbID := d.d.NewID()
	cb := &syncCallback{d: d.d, id: cbID, done: done}
	if err := d.d.Register(cbID, cb); err != nil {
		return err
	}
	body := wlwire.NewEncoder()
	body.PutUint(cbID)
	// wl_display request 0 = sync.
	return d.d.Send(displayObjectID, 0, body.Bytes(), nil)
}

// syncCallback handles the one event a wl_callback can emit
// (event 0 = done(callbackData)) and unregisters itself.
type syncCallback struct {
	d    *Dispatcher
	id   uint32
	done func(uint32)
}

func (c *syncCallback) HandleEvent(opcode uint16, body []byte, _ []int) error {
	if opcode != 0 {
		return nil
	}
	dec := wlwire.NewDecoder(body)
	data := dec.Uint()
	c.d.Unregister(c.id)
	if c.done != nil {
		c.done(data)
	}
	return nil
}

// --- registry ---

// Registry mirrors the wl_registry interface (advertised globals
// + the bind request).
//
// References:
//   request 0: bind (name, new_id with explicit interface+version)
//   event 0: global (name, interface, version)
//   event 1: global_remove (name)
type Registry struct {
	d              *Dispatcher
	id             uint32
	onGlobal       func(name uint32, iface string, version uint32)
	onGlobalRemove func(name uint32)
	log            *slog.Logger
}

func (r *Registry) HandleEvent(opcode uint16, body []byte, _ []int) error {
	dec := wlwire.NewDecoder(body)
	switch opcode {
	case 0:
		name := dec.Uint()
		iface := dec.String()
		version := dec.Uint()
		if r.onGlobal != nil {
			r.onGlobal(name, iface, version)
		}
	case 1:
		name := dec.Uint()
		if r.onGlobalRemove != nil {
			r.onGlobalRemove(name)
		}
	default:
		// Unknown event — same forward-compat rule.
	}
	return nil
}

// Bind invokes the registry::bind request, allocating a new
// object ID and registering its handler. The request body is the
// "new_id with explicit interface" variant: registry::bind takes
// a name (the global's identifier from the global event), then
// (string interface, uint version, uint new_id) — the so-called
// generic new-id encoding used only by wl_registry::bind.
//
// Returns the freshly-allocated object ID; the caller passes it
// to interface-specific request methods.
func (r *Registry) Bind(name uint32, iface string, version uint32, handler EventHandler) (uint32, error) {
	newID := r.d.NewID()
	if err := r.d.Register(newID, handler); err != nil {
		return 0, err
	}
	body := wlwire.NewEncoder()
	body.PutUint(name)
	body.PutString(iface)
	body.PutUint(version)
	body.PutUint(newID)
	// wl_registry request 0 = bind.
	if err := r.d.Send(r.id, 0, body.Bytes(), nil); err != nil {
		r.d.Unregister(newID)
		return 0, fmt.Errorf("wl_registry::bind %s: %w", iface, err)
	}
	return newID, nil
}
