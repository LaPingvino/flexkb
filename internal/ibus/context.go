package ibus

import (
	"sync"

	"github.com/godbus/dbus/v5"

	"github.com/lapingvino/flexkb/internal/imsession"
)

// InputContext is one per-app input context. Created by
// service.CreateInputContext when an app calls in; lives until
// the app destroys it or the connection drops.
type InputContext struct {
	srv    *Server
	path   dbus.ObjectPath
	client string

	mu       sync.Mutex
	sess     *imsession.Session
	focused  bool
	caps     uint32
	cursorX  int32
	cursorY  int32
	cursorW  int32
	cursorH  int32
	engine   string
}

// --- methods (called by clients) ---

// ProcessKeyEvent is the IM hot-path. ibus delivers each
// keystroke here; we feed it through the session and either
// commit/preedit (return true = key consumed) or return false
// (the compositor / app sees the keystroke unchanged).
//
// keyval is an X11 keysym (e.g. 0x0061 = 'a'). keycode is the
// hardware keycode (X11 convention: Linux evdev + 8). state is
// the X11 modifier bitmask (Shift=1, Lock=2, Control=4, Mod1=8,
// Mod5=128 for AltGr, plus 0x40000000 = key release).
func (c *InputContext) ProcessKeyEvent(keyval, keycode, state uint32) (bool, *dbus.Error) {
	// Bit 0x40000000 in state means "key release" per the ibus
	// protocol (legacy of X11 conventions). The IM tier is
	// press-driven; releases pass through.
	if state&0x40000000 != 0 {
		return false, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.focused {
		// No focus → not our keystroke. Defensive: some clients
		// send keys before FocusIn during startup.
		return false, nil
	}
	c.sess.HandleModifiers(state&0xff, 0, 0)
	// Linux evdev scancode = X11 keycode - 8.
	scancode := keycode - 8
	actions := c.sess.HandleKey(scancode, 1)
	consumed := false
	for _, a := range actions {
		switch act := a.(type) {
		case imsession.CommitText:
			c.emitCommitText(act.Text)
			consumed = true
		case imsession.SetPreedit:
			c.emitUpdatePreedit(act.Text, uint32(act.CursorEnd))
			consumed = true
		case imsession.FinishCommit:
			// ibus has no batch-commit equivalent. Each
			// CommitText signal IS the commit. So FinishCommit
			// is a no-op here — Wayland needed it because v2
			// has a separate commit() request, ibus doesn't.
		case imsession.PassThrough:
			// false return → client processes the key normally.
		}
	}
	return consumed, nil
}

// FocusIn marks the context as active. Subsequent ProcessKeyEvent
// calls will route through the session.
func (c *InputContext) FocusIn() *dbus.Error {
	c.mu.Lock()
	c.focused = true
	c.mu.Unlock()
	c.srv.log.Debug("FocusIn", "path", c.path, "client", c.client)
	return nil
}

// FocusOut deactivates the context. Pending preedit is cancelled;
// the client should expect no more commits until FocusIn fires
// again.
func (c *InputContext) FocusOut() *dbus.Error {
	c.mu.Lock()
	c.focused = false
	c.mu.Unlock()
	// Clear preedit on focus-out so a partially-typed sequence
	// doesn't reappear when focus returns elsewhere.
	c.emitHidePreedit()
	c.srv.log.Debug("FocusOut", "path", c.path)
	return nil
}

// Reset clears any in-flight preedit and resets the IM FSM. ibus
// clients call this after major edits (e.g. clipboard paste).
func (c *InputContext) Reset() *dbus.Error {
	// Re-create the session: simplest way to wipe FSM state
	// without exposing internal Reset() on imsession.Session.
	sess, err := c.srv.factory()
	if err != nil {
		return dbus.NewError("org.freedesktop.IBus.Error.NoSession",
			[]interface{}{err.Error()})
	}
	c.mu.Lock()
	c.sess = sess
	c.mu.Unlock()
	c.emitHidePreedit()
	return nil
}

// SetCapabilities records what features the client supports. ibus
// uses bitmask flags (PREEDIT_TEXT=1, AUXILIARY_TEXT=2,
// LOOKUP_TABLE=4, FOCUS=8, …). We record but don't currently
// gate behaviour on them — full feature negotiation is a v2.
func (c *InputContext) SetCapabilities(caps uint32) *dbus.Error {
	c.mu.Lock()
	c.caps = caps
	c.mu.Unlock()
	return nil
}

// SetCursorLocation records the screen-space position of the
// client's text cursor. Used by IMs that show floating candidate
// windows (Pinyin candidate lists, etc.). We just store it for
// future use.
func (c *InputContext) SetCursorLocation(x, y, w, h int32) *dbus.Error {
	c.mu.Lock()
	c.cursorX, c.cursorY, c.cursorW, c.cursorH = x, y, w, h
	c.mu.Unlock()
	return nil
}

// SetEngine selects which IM engine is active for this context.
// flexkb-imed routes everything through one engine per session;
// in a future multi-engine deployment this would swap the IM
// loaded into c.sess. For now we record and acknowledge.
func (c *InputContext) SetEngine(name string) *dbus.Error {
	c.mu.Lock()
	c.engine = name
	c.mu.Unlock()
	c.srv.log.Debug("SetEngine", "path", c.path, "engine", name)
	return nil
}

// Destroy tears down the input context. ibus clients call this
// when the app shuts down or the focus surface disappears.
func (c *InputContext) Destroy() *dbus.Error {
	c.srv.mu.Lock()
	delete(c.srv.contexts, c.path)
	c.srv.mu.Unlock()
	c.srv.conn.Export(nil, c.path, "org.freedesktop.IBus.InputContext")
	c.srv.conn.Export(nil, c.path, "org.freedesktop.IBus.Service")
	c.srv.log.Debug("Destroy", "path", c.path)
	return nil
}

// --- signals (emitted by us) ---

// emitCommitText emits org.freedesktop.IBus.InputContext.CommitText
// with an IBusText carrying the committed string. The IBusText
// dbus signature is "(sa{sv}sv)" — a struct of (text, attributes,
// _, _) where we leave the attribute dict empty.
func (c *InputContext) emitCommitText(text string) {
	t := makeIBusText(text)
	c.srv.conn.Emit(c.path, "org.freedesktop.IBus.InputContext.CommitText", t)
}

// emitUpdatePreedit emits the UpdatePreeditText signal. cursorPos
// is the caret offset in characters; visible=true tells the
// client to show the preedit.
func (c *InputContext) emitUpdatePreedit(text string, cursorPos uint32) {
	t := makeIBusText(text)
	c.srv.conn.Emit(c.path, "org.freedesktop.IBus.InputContext.UpdatePreeditText",
		t, cursorPos, true)
}

// emitHidePreedit clears the preedit display on the client side.
func (c *InputContext) emitHidePreedit() {
	c.srv.conn.Emit(c.path, "org.freedesktop.IBus.InputContext.HidePreeditText")
}

// makeIBusText builds the variant-wrapped IBusText struct. ibus's
// IBusText type is defined as:
//
//	(string text, a{sv} attributes, string _, variant _)
//
// We populate text and leave the others empty — flexkb doesn't
// emit colored / formatted preedit yet.
func makeIBusText(text string) dbus.Variant {
	type ibusText struct {
		Text       string
		Attributes map[string]dbus.Variant
		_          string
		_          dbus.Variant
	}
	return dbus.MakeVariant(ibusText{
		Text:       text,
		Attributes: map[string]dbus.Variant{},
	})
}
