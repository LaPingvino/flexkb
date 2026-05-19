package wlim

import (
	"fmt"
	"log/slog"
	"sync"

	"github.com/lapingvino/flexkb/internal/wlserver"
	"github.com/lapingvino/flexkb/internal/wlwire"
)

// This file is the server-side counterpart to im.go. The client
// side handles being an IME inside someone else's compositor; the
// server side handles BEING a compositor (from the v2 protocol's
// point of view) so downstream IMEs can plug into flexkb-imed as
// if it were a wlroots compositor.
//
// The protocol surface is the same — only direction is swapped:
//   * `Manager` (client) issued get_input_method requests; the
//     `ServerManager` here SERVICES them.
//   * `InputMethod` (client) RECEIVED activate/done events and
//     ISSUED commit_string requests; the `ServerInputMethod` here
//     EMITS activate/done events and RECEIVES commit_string.
//   * `KeyboardGrab` (client) RECEIVED key events; the
//     `ServerKeyboardGrab` here EMITS key events.
//
// The wire layer is direction-agnostic, so the bytes on either side
// of the socket are identical to what wlroots would produce — a
// downstream fcitx5 or custom v2 IME has no way to know it is
// talking to flexkb-imed rather than a real compositor.

// ServerManager hosts a single bound zwp_input_method_manager_v2
// object per accepted client. The wlserver.Registry's AddGlobal
// hook constructs one when a client binds the manager.
//
// One manager instance per connection. Lifetime is the connection.
// Implementations of the IM stack on top of this manager use
// SetCallbacks to receive notifications when downstream IMEs
// create/destroy input methods and grabs.
type ServerManager struct {
	d   *wlserver.Dispatcher
	id  uint32
	log *slog.Logger

	mu       sync.Mutex
	current  *ServerInputMethod
	callback ServerManagerCallback
}

// ServerManagerCallback is the daemon's hook into manager-level
// state changes. The bridge wiring in ibus.InputContext registers
// one of these so it can pick up the active input method (the
// "v2 client has grabbed" state) and route keystrokes through it.
type ServerManagerCallback interface {
	// OnInputMethodCreated fires when a downstream client calls
	// get_input_method. Only one input_method may exist per
	// seat per connection; the daemon hands ownership through to
	// the bridge.
	OnInputMethodCreated(im *ServerInputMethod)
	// OnInputMethodDestroyed fires when the downstream client
	// destroys the input_method (either explicit destroy request
	// or connection teardown).
	OnInputMethodDestroyed(im *ServerInputMethod)
}

// AddManagerGlobal registers zwp_input_method_manager_v2 with the
// per-connection registry. Returns the assigned "name" so callers
// can correlate logs. The provided callback receives lifecycle
// events for every input_method instantiated under this manager.
func AddManagerGlobal(reg *wlserver.Registry, log *slog.Logger, cb ServerManagerCallback) uint32 {
	if log == nil {
		log = slog.Default()
	}
	return reg.AddGlobal(InterfaceManager, ManagerVersion, func(d *wlserver.Dispatcher, newID, version uint32) error {
		m := &ServerManager{
			d:        d,
			id:       newID,
			log:      log,
			callback: cb,
		}
		if err := d.Register(newID, m); err != nil {
			return fmt.Errorf("register manager: %w", err)
		}
		return nil
	})
}

// HandleRequest services the manager's two requests:
//
//   request 0: get_input_method(seat, input_method new_id)
//   request 1: destroy
func (m *ServerManager) HandleRequest(opcode uint16, body []byte, _ []int) error {
	dec := wlwire.NewDecoder(body)
	switch opcode {
	case 0: // get_input_method
		_ = dec.Uint() // seat id — we don't multi-seat yet, just acknowledge
		imID := dec.Uint()
		im := &ServerInputMethod{
			d:       m.d,
			id:      imID,
			log:     m.log,
			manager: m,
		}
		if err := m.d.Register(imID, im); err != nil {
			return fmt.Errorf("register input_method: %w", err)
		}
		m.mu.Lock()
		previous := m.current
		m.current = im
		cb := m.callback
		m.mu.Unlock()
		if previous != nil {
			// Spec: only one input_method per seat. If the client
			// asks for a second one, the first becomes unavailable.
			// Emit unavailable on the prior one before swapping.
			previous.sendUnavailable()
			if cb != nil {
				cb.OnInputMethodDestroyed(previous)
			}
		}
		if cb != nil {
			cb.OnInputMethodCreated(im)
		}
		return nil
	case 1: // destroy
		m.d.Unregister(m.id)
		return nil
	default:
		m.log.Debug("ServerManager unknown request", "opcode", opcode)
		return nil
	}
}

// --- input_method ---

// ServerInputMethod is one zwp_input_method_v2 object on the server
// side. The bridge in ibus.InputContext calls Activate/Deactivate
// to drive its lifecycle, calls Done after batches of state
// updates, and reads the committed text via the OnCommitString /
// OnSetPreedit callbacks.
type ServerInputMethod struct {
	d       *wlserver.Dispatcher
	id      uint32
	log     *slog.Logger
	manager *ServerManager

	// Callbacks set by the daemon BEFORE the downstream client's
	// first request lands. Setting them after the bridge wiring
	// is fine (handlers are nil-guarded), but you'll miss any
	// early commits.

	// OnCommitString fires when the downstream IME commits text
	// (a finalised character or word). The accompanying Commit
	// request will follow; the bridge typically accumulates and
	// applies them together.
	OnCommitString func(text string)
	// OnSetPreeditString fires when the IME updates its preedit
	// (the in-progress, not-yet-committed text the user sees as
	// they type).
	OnSetPreeditString func(text string, cursorBegin, cursorEnd int32)
	// OnDeleteSurroundingText fires when the IME wants the
	// application to delete bytes around the cursor.
	OnDeleteSurroundingText func(before, after uint32)
	// OnCommit fires after the IME has accumulated zero or more
	// commit_string/set_preedit_string/delete_surrounding_text
	// requests and is signalling "apply all of the above now".
	// The bridge does the actual ibus.CommitText / UpdatePreedit
	// emission here, not in the per-action callbacks.
	OnCommit func(serial uint32)
	// OnGrabKeyboard fires when the downstream IME wants raw
	// keystrokes. The returned ServerKeyboardGrab is what the
	// bridge sends key events through. May be nil — the bridge
	// is allowed to refuse the grab (returning nil drops the
	// grab silently; the IME sees no key events).
	OnGrabKeyboard func(grab *ServerKeyboardGrab)
	// OnDestroy fires once the input_method is torn down. After
	// it fires, the bridge must stop routing through this IM.
	OnDestroy func()

	mu   sync.Mutex
	grab *ServerKeyboardGrab
}

// ID returns the wayland object id of this input method. Useful
// for logging and for the bridge's per-IM correlation.
func (im *ServerInputMethod) ID() uint32 { return im.id }

// HandleRequest services the input_method's six requests:
//
//   request 0: commit_string(text)
//   request 1: set_preedit_string(text, cursor_begin, cursor_end)
//   request 2: delete_surrounding_text(before_length, after_length)
//   request 3: commit(serial)
//   request 4: get_input_popup_surface(surface)        — ignored (UI flair)
//   request 5: grab_keyboard(keyboard new_id)
//   request 6: destroy
func (im *ServerInputMethod) HandleRequest(opcode uint16, body []byte, _ []int) error {
	dec := wlwire.NewDecoder(body)
	switch opcode {
	case 0: // commit_string
		text := dec.String()
		if im.OnCommitString != nil {
			im.OnCommitString(text)
		}
	case 1: // set_preedit_string
		text := dec.String()
		cb := dec.Int()
		ce := dec.Int()
		if im.OnSetPreeditString != nil {
			im.OnSetPreeditString(text, cb, ce)
		}
	case 2: // delete_surrounding_text
		before := dec.Uint()
		after := dec.Uint()
		if im.OnDeleteSurroundingText != nil {
			im.OnDeleteSurroundingText(before, after)
		}
	case 3: // commit
		serial := dec.Uint()
		if im.OnCommit != nil {
			im.OnCommit(serial)
		}
	case 4: // get_input_popup_surface — UI flair we ignore
		im.log.Debug("input_method::get_input_popup_surface ignored")
	case 5: // grab_keyboard
		grabID := dec.Uint()
		grab := &ServerKeyboardGrab{
			d:  im.d,
			id: grabID,
			im: im,
		}
		if err := im.d.Register(grabID, grab); err != nil {
			return fmt.Errorf("register keyboard_grab: %w", err)
		}
		im.mu.Lock()
		im.grab = grab
		hook := im.OnGrabKeyboard
		im.mu.Unlock()
		if hook != nil {
			hook(grab)
		}
	case 6: // destroy
		im.detach()
	default:
		im.log.Debug("ServerInputMethod unknown request", "opcode", opcode)
	}
	return nil
}

func (im *ServerInputMethod) detach() {
	im.mu.Lock()
	grab := im.grab
	im.grab = nil
	im.mu.Unlock()
	if grab != nil {
		im.d.Unregister(grab.id)
	}
	im.d.Unregister(im.id)
	if im.manager != nil {
		im.manager.mu.Lock()
		if im.manager.current == im {
			im.manager.current = nil
		}
		cb := im.manager.callback
		im.manager.mu.Unlock()
		if cb != nil {
			cb.OnInputMethodDestroyed(im)
		}
	}
	if im.OnDestroy != nil {
		im.OnDestroy()
	}
}

// Grab returns the active keyboard grab, or nil if the downstream
// IME hasn't grabbed yet. Used by the bridge to decide where to
// route keystrokes: if Grab() == nil, fall through to the next
// tier (ibushost engines, then in-process Session).
func (im *ServerInputMethod) Grab() *ServerKeyboardGrab {
	im.mu.Lock()
	defer im.mu.Unlock()
	return im.grab
}

// SendActivate emits the activate event — "a text-input client has
// gained focus; subsequent state events describe its context."
// The bridge calls this when its associated ibus InputContext
// gains focus.
//
// Per protocol, activate is followed by zero or more state events
// (surrounding_text, content_type, …) and then a done event before
// the IME starts seeing keystrokes.
func (im *ServerInputMethod) SendActivate() error {
	// IM event 0 = activate.
	return im.d.Send(im.id, 0, nil, nil)
}

// SendDeactivate emits the deactivate event — focus lost. The
// downstream IME should reset its per-session state.
func (im *ServerInputMethod) SendDeactivate() error {
	// IM event 1 = deactivate.
	return im.d.Send(im.id, 1, nil, nil)
}

// SendSurroundingText emits the surrounding_text event. The bridge
// calls this for IMEs that need cursor context (the ibus protocol
// has an equivalent that the bridge translates).
func (im *ServerInputMethod) SendSurroundingText(text string, cursor, anchor uint32) error {
	body := wlwire.NewEncoder()
	body.PutString(text)
	body.PutUint(cursor)
	body.PutUint(anchor)
	// IM event 2 = surrounding_text.
	return im.d.Send(im.id, 2, body.Bytes(), nil)
}

// SendTextChangeCause emits the text_change_cause event. cause
// matches the zwp_text_input_v3 enum (0=input_method, 1=other).
func (im *ServerInputMethod) SendTextChangeCause(cause uint32) error {
	body := wlwire.NewEncoder()
	body.PutUint(cause)
	// IM event 3 = text_change_cause.
	return im.d.Send(im.id, 3, body.Bytes(), nil)
}

// SendContentType emits the content_type event. hint and purpose
// are bitmasks/enums the application provided to flag e.g.
// "this is a password field" so the IME can adjust behaviour.
func (im *ServerInputMethod) SendContentType(hint, purpose uint32) error {
	body := wlwire.NewEncoder()
	body.PutUint(hint)
	body.PutUint(purpose)
	// IM event 4 = content_type.
	return im.d.Send(im.id, 4, body.Bytes(), nil)
}

// SendDone closes a logical batch of state events. The downstream
// IME applies surrounding_text + content_type + … atomically as
// of this done event rather than per-event.
func (im *ServerInputMethod) SendDone() error {
	// IM event 5 = done.
	return im.d.Send(im.id, 5, nil, nil)
}

// sendUnavailable emits the unavailable event — the server has
// withdrawn this input_method (typically because another connection
// took over the seat). One-shot per IM.
func (im *ServerInputMethod) sendUnavailable() {
	// IM event 6 = unavailable.
	if err := im.d.Send(im.id, 6, nil, nil); err != nil {
		im.log.Debug("send unavailable failed", "err", err)
	}
}

// --- keyboard_grab ---

// ServerKeyboardGrab is the server side of
// zwp_input_method_keyboard_grab_v2. The bridge calls SendKey /
// SendModifiers per keystroke to push events to the downstream
// IME; the IME sends back commit_string/etc on the parent
// input_method.
type ServerKeyboardGrab struct {
	d  *wlserver.Dispatcher
	id uint32
	im *ServerInputMethod

	mu       sync.Mutex
	released bool
}

// HandleRequest services the grab's single request: release.
func (g *ServerKeyboardGrab) HandleRequest(opcode uint16, _ []byte, _ []int) error {
	switch opcode {
	case 0: // release
		g.mu.Lock()
		g.released = true
		g.mu.Unlock()
		g.d.Unregister(g.id)
		if g.im != nil {
			g.im.mu.Lock()
			if g.im.grab == g {
				g.im.grab = nil
			}
			g.im.mu.Unlock()
		}
	default:
		// Forward-compat — ignore unknown opcodes.
	}
	return nil
}

// Released returns true once the downstream IME has issued the
// release request. The bridge checks this to avoid sending key
// events on a torn-down grab.
func (g *ServerKeyboardGrab) Released() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.released
}

// SendKeymap emits the keymap event with format/size in the body
// and the fd via SCM_RIGHTS. fd ownership transfers to the wire
// layer — after a successful return the caller MUST NOT close it
// (the kernel duplicates and we close the local side downstream).
// Same wire shape as wl_keyboard::keymap.
//
// The protocol XML reserves an "fd" arg slot between format and
// size; on the wire we still emit a placeholder uint32=0 since the
// actual fd rides in the ancillary data. This matches what
// libwayland-server does — the wire arg is a "spec marker" rather
// than the fd itself.
func (g *ServerKeyboardGrab) SendKeymap(format uint32, fd int, size uint32) error {
	body := wlwire.NewEncoder()
	body.PutUint(format)
	body.PutUint(0) // fd placeholder per protocol XML
	body.PutUint(size)
	// Grab event 0 = keymap.
	return g.d.Send(g.id, 0, body.Bytes(), []int{fd})
}

// SendKey emits one key event. serial is monotonic per grab;
// time is a millisecond timestamp; key is the linux evdev scancode;
// state is 0=released, 1=pressed.
func (g *ServerKeyboardGrab) SendKey(serial, time, key, state uint32) error {
	body := wlwire.NewEncoder()
	body.PutUint(serial)
	body.PutUint(time)
	body.PutUint(key)
	body.PutUint(state)
	// Grab event 1 = key.
	return g.d.Send(g.id, 1, body.Bytes(), nil)
}

// SendModifiers emits the modifiers event. The four bitmasks
// match xkb's depressed/latched/locked/group quartet.
func (g *ServerKeyboardGrab) SendModifiers(serial, depressed, latched, locked, group uint32) error {
	body := wlwire.NewEncoder()
	body.PutUint(serial)
	body.PutUint(depressed)
	body.PutUint(latched)
	body.PutUint(locked)
	body.PutUint(group)
	// Grab event 2 = modifiers.
	return g.d.Send(g.id, 2, body.Bytes(), nil)
}

// SendRepeatInfo emits the repeat_info event with the compositor's
// auto-repeat settings (rate in chars/sec, delay in ms).
func (g *ServerKeyboardGrab) SendRepeatInfo(rate, delay int32) error {
	body := wlwire.NewEncoder()
	body.PutInt(rate)
	body.PutInt(delay)
	// Grab event 3 = repeat_info.
	return g.d.Send(g.id, 3, body.Bytes(), nil)
}
