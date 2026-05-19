// Package wlim implements the client side of the wlroots
// zwp_input_method_v2 protocol on top of internal/wlclient.
//
// Three interfaces are bound here:
//
//   * zwp_input_method_manager_v2 — the global the compositor
//     advertises. The daemon binds it once per session, then
//     calls get_input_method on each wl_seat to obtain a
//     per-seat input-method handle.
//   * zwp_input_method_v2 — one per (seat, IM-client) pair.
//     Receives activate/deactivate as text-input clients focus
//     in/out; sends commit_string / set_preedit_string back as
//     the user types.
//   * zwp_input_method_keyboard_grab_v2 — created from an IM
//     object when the daemon wants raw keystrokes. Delivers
//     keycodes + modifiers + keymap fd; the daemon runs the
//     full Physical→IM stack on these and commits via the IM
//     object's request channel.
//
// Reference: wlroots-protocols' input-method-unstable-v2.xml.
// The wire-level details (opcodes, argument types) are
// transcribed directly from the protocol XML — see comments at
// each request/event for the exact form.
package wlim

import (
	"fmt"
	"log/slog"

	"github.com/lapingvino/flexkb/internal/wlclient"
	"github.com/lapingvino/flexkb/internal/wlwire"
)

// InterfaceManager is the protocol name the compositor advertises
// for the manager global. Used by the daemon's registry-walking
// code to spot it and call BindManager.
const InterfaceManager = "zwp_input_method_manager_v2"

// ManagerVersion is the highest version of the manager protocol
// this package implements. Bind requests use min(advertised,
// ManagerVersion).
const ManagerVersion = 1

// --- manager ---

// Manager wraps a bound zwp_input_method_manager_v2 object. Use
// BindManager from the registry handler that spots the global.
type Manager struct {
	d  *wlclient.Dispatcher
	id uint32
}

// BindManager binds the manager global. The caller supplies the
// numeric `name` from the wl_registry global event plus the
// advertised version (the daemon should pass min(version,
// ManagerVersion) — wlim doesn't enforce, leaves room for the
// daemon to negotiate per-policy).
func BindManager(reg *wlclient.Registry, name uint32, version uint32, d *wlclient.Dispatcher) (*Manager, error) {
	m := &Manager{d: d}
	id, err := reg.Bind(name, InterfaceManager, version, m)
	if err != nil {
		return nil, err
	}
	m.id = id
	return m, nil
}

// HandleEvent: the manager has no events. Anything that arrives
// here is forward-compatible protocol churn we ignore.
func (m *Manager) HandleEvent(opcode uint16, body []byte, _ []int) error {
	return nil
}

// GetInputMethod calls the manager's get_input_method request and
// returns the per-seat IM handle. seatID is the object id of a
// previously-bound wl_seat. handler receives the IM's events.
func (m *Manager) GetInputMethod(seatID uint32, handler *InputMethod) (*InputMethod, error) {
	id := m.d.NewID()
	handler.d = m.d
	handler.id = id
	if err := m.d.Register(id, handler); err != nil {
		return nil, err
	}
	body := wlwire.NewEncoder()
	body.PutUint(seatID)
	body.PutUint(id)
	// Manager request 0 = get_input_method.
	if err := m.d.Send(m.id, 0, body.Bytes(), nil); err != nil {
		m.d.Unregister(id)
		return nil, fmt.Errorf("get_input_method: %w", err)
	}
	return handler, nil
}

// Destroy issues the manager's destroy request.
func (m *Manager) Destroy() error {
	// Manager request 1 = destroy.
	return m.d.Send(m.id, 1, nil, nil)
}

// --- input_method ---

// InputMethod is one bound zwp_input_method_v2 object. It exposes
// the send-side requests (commit_string, set_preedit_string, …)
// directly as methods and dispatches received events to the
// callback fields populated by the daemon before calling
// Manager.GetInputMethod.
type InputMethod struct {
	d   *wlclient.Dispatcher
	id  uint32
	log *slog.Logger

	// OnActivate fires when a text-input client gains focus. The
	// daemon should start treating subsequent surrounding-text /
	// content-type events as the current session context.
	OnActivate func()
	// OnDeactivate fires when the text-input client loses focus.
	// The daemon resets per-session state.
	OnDeactivate func()
	// OnSurroundingText fires when the application reports its
	// cursor-area text (for IMs that want to look at it). cursor
	// and anchor are byte indices into text.
	OnSurroundingText func(text string, cursor, anchor uint32)
	// OnTextChangeCause fires before a surrounding_text event to
	// describe what caused the change. Cause values match the
	// zwp_text_input_v3 enum (0=input_method, 1=other).
	OnTextChangeCause func(cause uint32)
	// OnContentType reports the application's hint/purpose for
	// the current input field (URL field, password, email, …).
	OnContentType func(hint, purpose uint32)
	// OnDone fires after a logically-grouped batch of the above
	// events. The daemon uses it as a "commit pending state"
	// signal — apply surrounding text + content type together,
	// not piecemeal.
	OnDone func()
	// OnUnavailable fires once: the compositor refused this IM
	// because another IM already has the seat. The daemon should
	// log and tear down.
	OnUnavailable func()
}

// NewInputMethod returns a fresh handler with no callbacks set.
// Populate the On* fields before passing to Manager.GetInputMethod.
func NewInputMethod(log *slog.Logger) *InputMethod {
	return &InputMethod{log: log}
}

// HandleEvent routes the seven events the IM object can emit.
// Opcodes match the protocol XML's event order.
func (im *InputMethod) HandleEvent(opcode uint16, body []byte, _ []int) error {
	dec := wlwire.NewDecoder(body)
	switch opcode {
	case 0: // activate
		if im.OnActivate != nil {
			im.OnActivate()
		}
	case 1: // deactivate
		if im.OnDeactivate != nil {
			im.OnDeactivate()
		}
	case 2: // surrounding_text(text, cursor, anchor)
		text := dec.String()
		cursor := dec.Uint()
		anchor := dec.Uint()
		if im.OnSurroundingText != nil {
			im.OnSurroundingText(text, cursor, anchor)
		}
	case 3: // text_change_cause(cause)
		cause := dec.Uint()
		if im.OnTextChangeCause != nil {
			im.OnTextChangeCause(cause)
		}
	case 4: // content_type(hint, purpose)
		hint := dec.Uint()
		purpose := dec.Uint()
		if im.OnContentType != nil {
			im.OnContentType(hint, purpose)
		}
	case 5: // done
		if im.OnDone != nil {
			im.OnDone()
		}
	case 6: // unavailable
		if im.OnUnavailable != nil {
			im.OnUnavailable()
		}
	default:
		// Forward-compat — ignore unknown opcodes per Wayland convention.
	}
	return nil
}

// CommitString sends the commit_string request: the IM is
// finalising text into the application's input field.
//
// Per protocol: commit_string accumulates until Commit(serial) is
// called. The daemon typically calls CommitString followed
// immediately by Commit when emitting a finalised character.
func (im *InputMethod) CommitString(text string) error {
	body := wlwire.NewEncoder()
	body.PutString(text)
	// IM request 0 = commit_string.
	return im.d.Send(im.id, 0, body.Bytes(), nil)
}

// SetPreeditString sends the set_preedit_string request. cursor
// indices are byte offsets into text; -1 hides the cursor.
//
// Same accumulation rule: must be followed by Commit(serial) to
// actually apply.
func (im *InputMethod) SetPreeditString(text string, cursorBegin, cursorEnd int32) error {
	body := wlwire.NewEncoder()
	body.PutString(text)
	body.PutInt(cursorBegin)
	body.PutInt(cursorEnd)
	// IM request 1 = set_preedit_string.
	return im.d.Send(im.id, 1, body.Bytes(), nil)
}

// DeleteSurroundingText sends the delete_surrounding_text request.
// before/after are byte counts to delete on each side of the
// current cursor.
func (im *InputMethod) DeleteSurroundingText(before, after uint32) error {
	body := wlwire.NewEncoder()
	body.PutUint(before)
	body.PutUint(after)
	// IM request 2 = delete_surrounding_text.
	return im.d.Send(im.id, 2, body.Bytes(), nil)
}

// Commit finalises accumulated commit_string / set_preedit_string /
// delete_surrounding_text since the last Commit. serial echoes the
// most recent done event's implicit serial; for v2 the spec uses
// a monotonic counter the IM keeps and increments per Commit.
func (im *InputMethod) Commit(serial uint32) error {
	body := wlwire.NewEncoder()
	body.PutUint(serial)
	// IM request 3 = commit.
	return im.d.Send(im.id, 3, body.Bytes(), nil)
}

// GrabKeyboard returns a fresh keyboard-grab object. The caller
// populates the grab's On* callbacks before this request is sent.
// Until this is called, the IM only sees text events (surrounding_
// text, content_type); after, it also sees every keystroke.
func (im *InputMethod) GrabKeyboard(grab *KeyboardGrab) (*KeyboardGrab, error) {
	id := im.d.NewID()
	grab.d = im.d
	grab.id = id
	if err := im.d.Register(id, grab); err != nil {
		return nil, err
	}
	body := wlwire.NewEncoder()
	body.PutUint(id)
	// IM request 5 = grab_keyboard.
	if err := im.d.Send(im.id, 5, body.Bytes(), nil); err != nil {
		im.d.Unregister(id)
		return nil, fmt.Errorf("grab_keyboard: %w", err)
	}
	return grab, nil
}

// Destroy issues the IM object's destroy request.
func (im *InputMethod) Destroy() error {
	// IM request 6 = destroy.
	return im.d.Send(im.id, 6, nil, nil)
}

// --- keyboard_grab ---

// KeyboardGrab is the keystroke-delivery side of the IM. Once
// granted (via InputMethod.GrabKeyboard), it streams keymap +
// per-key events the same shape wl_keyboard does. The daemon
// runs each keycode through the runtime resolver and IM engine.
type KeyboardGrab struct {
	d  *wlclient.Dispatcher
	id uint32

	// OnKeymap fires once after grab — the compositor sends the
	// active xkb keymap so an IM that wants to lay out its own
	// symbol↔keycode mapping can read it. fd is the keymap file
	// descriptor (the IM must close it when done); size is the
	// keymap-text byte length. format=1 means xkb_v1 text form.
	//
	// flexkb-imed doesn't NEED this — we resolve keycodes via our
	// own data tree — but reading the keymap is useful sanity for
	// "what's the compositor's idea of the layout".
	OnKeymap func(format uint32, fd int, size uint32)

	// OnKey fires per keypress and release. serial is monotonic
	// per grab; time is a compositor-local millisecond timestamp;
	// key is the Linux evdev scancode; state is 0=released, 1=pressed.
	OnKey func(serial, time, key, state uint32)

	// OnModifiers fires whenever modifier state changes (Shift
	// pressed, Caps Lock toggled, …). All fields are xkb modifier
	// bitmasks.
	OnModifiers func(serial, depressed, latched, locked, group uint32)

	// OnRepeatInfo fires once after grab: rate in chars/sec,
	// delay in ms before repeat starts. The daemon honours these
	// for synthesising the repeat stream the IM engine sees.
	OnRepeatInfo func(rate, delay int32)
}

// HandleEvent routes the four grab events.
func (g *KeyboardGrab) HandleEvent(opcode uint16, body []byte, fds []int) error {
	dec := wlwire.NewDecoder(body)
	switch opcode {
	case 0: // keymap(format, fd, size)
		format := dec.Uint()
		// The fd is delivered via SCM_RIGHTS, NOT in the body.
		// The protocol XML reserves a slot in the args for it
		// but the bytes are zero on the wire; the actual fd is
		// in fds[0]. We still PutUint a placeholder when sending
		// fd-bearing requests — but for received events the
		// dispatcher hands us fds separately. The "fd" wire
		// argument always exists between format and size; skip
		// the 4 bytes by reading and discarding.
		_ = dec.Uint()
		size := dec.Uint()
		if len(fds) == 0 {
			return fmt.Errorf("zwp_input_method_keyboard_grab_v2::keymap missing fd")
		}
		fd := fds[0]
		if g.OnKeymap != nil {
			g.OnKeymap(format, fd, size)
		} else {
			// No handler — close the fd so we don't leak.
			closeFDFunc(fd)
		}
	case 1: // key(serial, time, key, state)
		serial := dec.Uint()
		time := dec.Uint()
		key := dec.Uint()
		state := dec.Uint()
		if g.OnKey != nil {
			g.OnKey(serial, time, key, state)
		}
	case 2: // modifiers(serial, depressed, latched, locked, group)
		serial := dec.Uint()
		depressed := dec.Uint()
		latched := dec.Uint()
		locked := dec.Uint()
		group := dec.Uint()
		if g.OnModifiers != nil {
			g.OnModifiers(serial, depressed, latched, locked, group)
		}
	case 3: // repeat_info(rate, delay)
		rate := dec.Int()
		delay := dec.Int()
		if g.OnRepeatInfo != nil {
			g.OnRepeatInfo(rate, delay)
		}
	default:
		// Forward-compat.
	}
	return nil
}

// Release ends the keyboard grab. The compositor stops delivering
// key events and the IM stops seeing keystrokes; a subsequent
// GrabKeyboard would re-establish the grab.
func (g *KeyboardGrab) Release() error {
	// Grab request 0 = release.
	return g.d.Send(g.id, 0, nil, nil)
}

// closeFDFunc is a hook so we don't reach into wlclient for its
// closeFD just to clean up a leaked keymap fd; package-private
// wrapper around syscall.Close.
var closeFDFunc = func(fd int) error { return closeRawFD(fd) }
