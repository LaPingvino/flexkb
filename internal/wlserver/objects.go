// Package wlserver is the server-side counterpart to wlclient: it
// listens on a Unix-domain socket, accepts Wayland-protocol clients,
// and dispatches their requests to per-object handlers. Per-interface
// server code (wl_display, wl_registry, zwp_input_method_manager_v2,
// …) registers with this package's Dispatcher to receive requests
// and emit events.
//
// Same wire layer (internal/wlwire) as the client side — the binary
// framing is direction-agnostic. What differs is the object-id range:
// the server is responsible for the high range (≥ 0xFF000000), and
// the bootstrap object at ID 1 is wl_display on the SERVER side too,
// but the connection's request stream comes IN and the event stream
// goes OUT.
//
// Why not depend on libwayland-server? Same reason wlclient is
// hand-rolled: the protocol is small and stable, and owning both
// sides of the same wire layer lets the daemon serve as v2 IM bus
// to downstream IMEs without dragging in a C library or a fat
// pure-Go generator.
package wlserver

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"

	"github.com/lapingvino/flexkb/internal/wlwire"
)

// Wayland's two-range object-id allocation: client IDs occupy
// 1..0xFEFFFFFF, server IDs occupy 0xFF000000..0xFFFFFFFF.
// On the server, we ALLOCATE in the high range (for new_id args we
// generate, like a callback's id) and we RECEIVE clients' new_id
// requests in the low range.
const (
	displayObjectID = 1
	firstServerID   = 0xFF000000
)

// RequestHandler is what an object registers to receive its
// requests. opcode identifies the request per the interface's
// protocol XML; body is the message payload to decode with
// wlwire.NewDecoder; fds carries any file descriptors received via
// SCM_RIGHTS alongside this message.
//
// Handlers run on the per-connection dispatch goroutine — they
// should be fast or hand work off elsewhere.
type RequestHandler interface {
	HandleRequest(opcode uint16, body []byte, fds []int) error
}

// HandlerFunc lets you supply a handler as a plain function when an
// object's request surface is small.
type HandlerFunc func(opcode uint16, body []byte, fds []int) error

func (f HandlerFunc) HandleRequest(opcode uint16, body []byte, fds []int) error {
	return f(opcode, body, fds)
}

// Dispatcher owns the object table for one server-side Wayland
// connection. One Dispatcher per accepted client; the Listener
// constructs them in Accept.
//
// The shape mirrors wlclient.Dispatcher deliberately — same
// vocabulary, opposite direction. NewID hands out server-side IDs
// (high range) for objects the server creates and announces in the
// event stream (e.g. wl_callback for sync); Register binds incoming
// new-IDs from client requests (low range) to handlers.
type Dispatcher struct {
	conn *wlwire.Conn
	log  *slog.Logger

	mu       sync.Mutex
	handlers map[uint32]RequestHandler
	nextID   uint32

	// onClose fires once when Run exits, for the Listener to
	// drop this connection from its tracking set.
	onClose func()
}

// NewDispatcher wraps an already-accepted wlwire.Conn. The caller
// invokes Run in a goroutine; the Listener does that automatically
// when Accept hands a Conn over to NewDispatcher.
func NewDispatcher(conn *wlwire.Conn, log *slog.Logger) *Dispatcher {
	if log == nil {
		log = slog.Default()
	}
	return &Dispatcher{
		conn:     conn,
		log:      log,
		handlers: map[uint32]RequestHandler{},
		nextID:   firstServerID,
	}
}

// Conn returns the underlying wire connection. Exposed so interface
// implementations can perform fd-bearing sends or close the
// connection on protocol errors.
func (d *Dispatcher) Conn() *wlwire.Conn { return d.conn }

// Log returns the dispatcher's logger so interface code can record
// per-connection state without re-plumbing one through every call.
func (d *Dispatcher) Log() *slog.Logger { return d.log }

// Register binds an object ID to a handler. Used in two contexts:
//
//   1. The Listener registers wl_display at id=1 before starting
//      Run, so the first request the client sends is dispatchable.
//   2. Request handlers register the client-provided new-IDs that
//      their requests create (registry::bind allocates one for the
//      bound interface; display::sync allocates one for the
//      callback; etc.). The new-IDs come FROM the client request
//      and live in the low (client) range — we just bind them in
//      our table on receipt.
func (d *Dispatcher) Register(id uint32, h RequestHandler) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, dup := d.handlers[id]; dup {
		return fmt.Errorf("wlserver: object ID %d already registered", id)
	}
	d.handlers[id] = h
	return nil
}

// Unregister drops a handler. Called when the protocol explicitly
// destroys an object (most interfaces have a destroy request) or
// when the server sends delete_id (we don't normally — clients
// allocate; we just acknowledge their destroy requests).
func (d *Dispatcher) Unregister(id uint32) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.handlers, id)
}

// NewServerID hands out a fresh ID in the server range. The server
// allocates these only for objects IT creates and announces — for
// the v2 IM rebroadcast plan, none of the IM-side requests
// actually require server-allocated objects (the manager and
// input_method objects come from client new_id requests). The
// helper exists for completeness and for future use by other
// interfaces (wl_data_offer, wl_pointer cursor surface, etc.).
func (d *Dispatcher) NewServerID() uint32 {
	d.mu.Lock()
	defer d.mu.Unlock()
	id := d.nextID
	d.nextID++
	return id
}

// Send emits an event on the wire. body should be encoded with
// wlwire.NewEncoder; fds carries file descriptors to pass via
// SCM_RIGHTS (empty for events that don't have an fd argument).
// The caller is responsible for matching the interface's event
// XML — Send doesn't inspect or rewrite the payload.
func (d *Dispatcher) Send(targetID uint32, opcode uint16, body []byte, fds []int) error {
	return d.conn.WriteMessage(targetID, opcode, body, fds)
}

// Run is the dispatch loop. Blocks until peer-close or error.
// Closes any received fds on unknown-object events; the protocol
// allows that transiently (the client may have destroyed the
// object between sending and our reading), so we drop the message
// rather than error.
func (d *Dispatcher) Run() error {
	if d.onClose != nil {
		defer d.onClose()
	}
	for {
		h, body, fds, err := d.conn.ReadMessage()
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		d.mu.Lock()
		handler := d.handlers[h.ObjectID]
		d.mu.Unlock()
		if handler == nil {
			d.log.Debug("request for unknown object", "id", h.ObjectID, "opcode", h.Opcode)
			closeFDs(fds)
			continue
		}
		if err := handler.HandleRequest(h.Opcode, body, fds); err != nil {
			closeFDs(fds)
			return fmt.Errorf("object %d opcode %d: %w", h.ObjectID, h.Opcode, err)
		}
	}
}

// Close tears down the dispatcher's connection. Safe to call from
// any goroutine; Run will see the close and return nil.
func (d *Dispatcher) Close() error { return d.conn.Close() }

// closeFDs releases received file descriptors that no handler
// consumed.
func closeFDs(fds []int) {
	for _, fd := range fds {
		_ = closeFD(fd)
	}
}
