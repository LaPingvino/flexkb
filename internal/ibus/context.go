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
// keystroke here; we route it through either an external
// engine (if one is bound to this input context) or the
// in-process Session, depending on the engine binding.
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
	if !c.focused {
		c.mu.Unlock()
		return false, nil
	}
	c.mu.Unlock()

	// Three-tier routing, highest-priority first:
	//
	//   1. v2 rebroadcast — a downstream Wayland v2 IME with an
	//      active keyboard grab gets first refusal. Asynchronous:
	//      the IME's commit_string lands later on this context via
	//      the InputContext.Emit* hooks the daemon wires.
	//   2. EngineHost — a bound ibus engine (libpinyin etc.) is
	//      consulted next. Engines have full authority over the
	//      keystroke; consumed=true suppresses local processing.
	//   3. In-process Session — the static-layer + flexkb-native IM
	//      path, the fallback that always works.
	if c.srv.v2 != nil && c.srv.v2.HasActiveGrab() {
		consumed, err := c.srv.v2.RouteKey(c.path, keyval, keycode, state)
		if err != nil {
			c.srv.log.Warn("v2 router failed; falling back",
				"err", err)
		} else if consumed {
			return true, nil
		}
	}

	// Route through an external engine if one is bound. The
	// engine has full authority over the keystroke — if it
	// consumes, we don't double-process via Session. Engines
	// like libpinyin do their own keysym → committed-text
	// translation that doesn't need to go through our resolver.
	if c.srv.host != nil {
		if eng := c.srv.host.EngineFor(c.path); eng != nil && eng.Connected() {
			consumed, err := eng.ProcessKeyEvent(keyval, keycode, state)
			if err != nil {
				c.srv.log.Warn("engine ProcessKeyEvent failed; falling back to in-process session",
					"engine", eng.Name(), "err", err)
			} else if consumed {
				return true, nil
			}
		}
	}

	// In-process Session path — unchanged from 5.2.
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sess.HandleModifiers(state&0xff, 0, 0)
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
			// ibus has no batch-commit equivalent.
		case imsession.PassThrough:
			// false return → client processes the key normally.
		}
	}
	return consumed, nil
}

// FocusIn marks the context as active. Subsequent ProcessKeyEvent
// calls will route through the session. If an engine is bound,
// tells the engine "this context is now active" so its
// per-context state loads, AND notifies the host that this is
// now the focused context for the engine — signals routed back
// will reach the right path.
//
// The v2 router (if attached) also gets a focus notification so
// commit_string responses from a downstream v2 IME route back to
// this context's dbus path.
func (c *InputContext) FocusIn() *dbus.Error {
	c.mu.Lock()
	c.focused = true
	engineName := c.engine
	c.mu.Unlock()
	c.srv.log.Debug("FocusIn", "path", c.path, "client", c.client)
	if c.srv.host != nil && engineName != "" {
		if eng := c.srv.host.EngineFor(c.path); eng != nil && eng.Connected() {
			_ = eng.FocusIn()
		}
		c.srv.host.NotifyFocusIn(engineName, c.path)
	}
	if c.srv.v2 != nil {
		c.srv.v2.NotifyFocusIn(c.path)
	}
	return nil
}

// FocusOut deactivates the context. Pending preedit is cancelled;
// the client should expect no more commits until FocusIn fires
// again. Tells the bound engine (if any) to release its
// per-context state and unhooks signal routing.
func (c *InputContext) FocusOut() *dbus.Error {
	c.mu.Lock()
	c.focused = false
	engineName := c.engine
	c.mu.Unlock()
	if c.srv.host != nil && engineName != "" {
		if eng := c.srv.host.EngineFor(c.path); eng != nil && eng.Connected() {
			_ = eng.FocusOut()
		}
		c.srv.host.NotifyFocusOut(engineName)
	}
	if c.srv.v2 != nil {
		c.srv.v2.NotifyFocusOut(c.path)
	}
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
// Two cases:
//
//   1. The named engine is one flexkb-imed hosts itself (a
//      flexkb-native IM declared in data/inputmethods/). Today
//      we have one Session per context so the named engine is
//      informational; future multi-engine support will swap the
//      loaded IM in c.sess.
//
//   2. The named engine is an external ibus engine (libpinyin
//      etc.) catalogued by ibushost. We tell the host to bind
//      this input context to the engine; subsequent
//      ProcessKeyEvent calls route there first.
//
// On unknown engines we record the name (so the client's
// "what's the active engine?" query gets a consistent answer)
// but don't fail — ibus's protocol semantics tolerate the
// client requesting engines the daemon doesn't know about.
func (c *InputContext) SetEngine(name string) *dbus.Error {
	c.mu.Lock()
	c.engine = name
	c.mu.Unlock()
	c.srv.log.Debug("SetEngine", "path", c.path, "engine", name)
	if c.srv.host != nil && name != "" {
		if _, err := c.srv.host.SelectEngine(c.path, name); err != nil {
			// Bind failure isn't fatal — we just keep using the
			// in-process Session for this context. Log so the
			// user can investigate if the engine they wanted
			// isn't actually available.
			c.srv.log.Info("engine not bound; using in-process session", "engine", name, "err", err)
		} else {
			// Successful bind. Tell the engine the context is
			// (or about to be) focused so it loads its state.
			if eng := c.srv.host.EngineFor(c.path); eng != nil && eng.Connected() {
				_ = eng.FocusIn()
			}
		}
	}
	return nil
}

// Destroy tears down the input context. ibus clients call this
// when the app shuts down or the focus surface disappears.
func (c *InputContext) Destroy() *dbus.Error {
	if c.srv.host != nil {
		c.srv.host.ReleaseEngine(c.path)
	}
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
