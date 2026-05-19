// Package wlclient implements the client-side core of the Wayland
// protocol on top of internal/wlwire: object ID allocation, dispatch
// of incoming events to per-object handlers, and the two
// always-present objects every client uses (wl_display and
// wl_registry). Per-extension code (input-method-v2 etc.) registers
// with this package's Dispatcher to receive its events.
//
// The "client-side core" subset is deliberately small — enough to
// list and bind globals advertised by the compositor and route
// their events. Specific interface bindings live in sibling
// packages (wlim/, wlkeyboard/, …) that import this one.
package wlclient

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"

	"github.com/lapingvino/flexkb/internal/wlwire"
)

// Wayland reserves object IDs into two ranges: client-allocated
// (1 ≤ id < 0xFF000000) and server-allocated (id ≥ 0xFF000000).
// We start client IDs at 2 because the wl_display always lives at
// ID 1 (the bootstrap object every connection starts with).
const (
	displayObjectID = 1
	firstClientID   = 2
	serverIDStart   = 0xFF000000
)

// EventHandler is what an object registers to receive its events.
// opcode identifies which event was emitted (per the interface's
// protocol XML); body is the message payload to decode with
// wlwire.NewDecoder; fds carries any file descriptors received via
// SCM_RIGHTS alongside this message.
//
// Handlers run on the dispatch goroutine — they should be fast or
// hand work off to other goroutines.
type EventHandler interface {
	HandleEvent(opcode uint16, body []byte, fds []int) error
}

// HandlerFunc lets you supply a handler as a plain function when
// the object's event surface is small enough not to warrant a
// dedicated type.
type HandlerFunc func(opcode uint16, body []byte, fds []int) error

func (f HandlerFunc) HandleEvent(opcode uint16, body []byte, fds []int) error {
	return f(opcode, body, fds)
}

// Dispatcher owns the object table for one Wayland connection. It
// allocates IDs, routes incoming events to the registered handler,
// and exposes a Send method that interface packages use to write
// requests through the wire layer.
type Dispatcher struct {
	conn *wlwire.Conn
	log  *slog.Logger

	mu       sync.Mutex
	handlers map[uint32]EventHandler
	nextID   uint32
}

// NewDispatcher wraps an open wlwire.Conn. The caller is responsible
// for invoking Run in a goroutine; until Run is started no events
// are delivered.
func NewDispatcher(conn *wlwire.Conn, log *slog.Logger) *Dispatcher {
	if log == nil {
		log = slog.Default()
	}
	return &Dispatcher{
		conn:     conn,
		log:      log,
		handlers: map[uint32]EventHandler{},
		nextID:   firstClientID,
	}
}

// Register binds an object ID to a handler. Returns an error if
// the ID is already in use — caller's responsibility to allocate
// via NewID before calling here.
//
// Pre-allocated IDs (like displayObjectID=1) are registered
// explicitly via the Bootstrap helper, since they aren't allocated
// from the same counter as fresh new-id allocations.
func (d *Dispatcher) Register(id uint32, h EventHandler) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, dup := d.handlers[id]; dup {
		return fmt.Errorf("wlclient: object ID %d already registered", id)
	}
	d.handlers[id] = h
	return nil
}

// Unregister drops a handler, used when a Wayland-side delete_id
// arrives (the server signalling that an object has been
// destroyed). Quiet no-op if the ID isn't registered.
func (d *Dispatcher) Unregister(id uint32) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.handlers, id)
}

// NewID allocates a fresh client-side object ID. Combined with a
// subsequent Send + Register, this is how a request that creates a
// new object (bind, get_pointer, get_keyboard, …) introduces that
// object into the dispatch table.
func (d *Dispatcher) NewID() uint32 {
	d.mu.Lock()
	defer d.mu.Unlock()
	id := d.nextID
	d.nextID++
	return id
}

// Send transmits a request on the wire. body should be encoded with
// wlwire.NewEncoder; fds carries file descriptors to pass via
// SCM_RIGHTS (empty for the common case). Requests that allocate a
// new object include that object's ID inside body (per the request's
// protocol XML); Send doesn't inspect or rewrite the payload.
func (d *Dispatcher) Send(targetID uint32, opcode uint16, body []byte, fds []int) error {
	return d.conn.WriteMessage(targetID, opcode, body, fds)
}

// Run is the dispatch loop. It reads messages one at a time and
// invokes the matching handler. Returns nil on a clean peer-close
// (io.EOF), or an error for any other failure (protocol error,
// handler error, transport error).
//
// Designed to be invoked in a dedicated goroutine. The caller can
// stop the loop by closing the Conn — Run will see io.EOF (or
// "use of closed network connection") and exit.
func (d *Dispatcher) Run() error {
	for {
		h, body, fds, err := d.conn.ReadMessage()
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
				// Clean shutdown — peer closed, or the local
				// caller closed the Conn to stop the loop.
				return nil
			}
			return err
		}
		d.mu.Lock()
		handler := d.handlers[h.ObjectID]
		d.mu.Unlock()
		if handler == nil {
			// Unknown object — Wayland's protocol design allows
			// this transiently around delete_id races. Log and
			// drop the message; closing any fds we received
			// since the handler that would have consumed them is
			// gone.
			d.log.Debug("event for unknown object", "id", h.ObjectID, "opcode", h.Opcode)
			closeFDs(fds)
			continue
		}
		if err := handler.HandleEvent(h.Opcode, body, fds); err != nil {
			closeFDs(fds)
			return fmt.Errorf("object %d opcode %d: %w", h.ObjectID, h.Opcode, err)
		}
	}
}

// closeFDs releases received file descriptors that the dispatcher
// couldn't deliver to a handler. The handler-success path leaves
// FD ownership to the handler.
func closeFDs(fds []int) {
	for _, fd := range fds {
		_ = closeFD(fd)
	}
}
