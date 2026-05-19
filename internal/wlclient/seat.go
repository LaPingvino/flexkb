package wlclient

import "github.com/lapingvino/flexkb/internal/wlwire"

// InterfaceSeat is the protocol name compositors advertise for the
// per-input-device root object. The IM daemon needs a seat to
// hand to zwp_input_method_manager_v2::get_input_method, so we
// always bind the first one advertised. Multi-seat setups are
// uncommon and aren't a v1 target — when they show up, the
// daemon can iterate over all seat globals and bind each.
const InterfaceSeat = "wl_seat"

// SeatVersion is the minimum we ask for. wl_seat v7 added the
// `name` event (server tells the client its seat name like
// "seat0") which we'd want for multi-seat in the future; for now
// we don't depend on it and asking for the compositor's
// advertised version is fine.
const SeatVersion = 1

// Seat is a minimum wl_seat binding: we hold its object id so we
// can pass it to get_input_method, and we acknowledge but don't
// act on the capabilities / name events. The daemon doesn't need
// pointer/keyboard/touch handles directly — keyboard input comes
// from the IM's keyboard_grab, not from wl_seat::get_keyboard.
type Seat struct {
	d  *Dispatcher
	id uint32

	// Capabilities is the most recent capabilities bitmask
	// (pointer=1, keyboard=2, touch=4). Updated from the
	// capabilities event; the daemon may inspect it for logging.
	Capabilities uint32
	// Name is the seat name from the v2+ name event, or "" when
	// the compositor didn't send it.
	Name string
}

// BindSeat binds a wl_seat global advertised by the registry.
func BindSeat(reg *Registry, name uint32, version uint32, d *Dispatcher) (*Seat, error) {
	s := &Seat{d: d}
	id, err := reg.Bind(name, InterfaceSeat, version, s)
	if err != nil {
		return nil, err
	}
	s.id = id
	return s, nil
}

// ID returns the seat's object id — what gets passed to
// zwp_input_method_manager_v2::get_input_method.
func (s *Seat) ID() uint32 { return s.id }

// HandleEvent routes the two events wl_seat emits: capabilities
// (event 0) and name (event 1, v2+). Forward-compatible: unknown
// opcodes are ignored.
func (s *Seat) HandleEvent(opcode uint16, body []byte, _ []int) error {
	dec := wlwire.NewDecoder(body)
	switch opcode {
	case 0:
		s.Capabilities = dec.Uint()
	case 1:
		s.Name = dec.String()
	}
	return nil
}
