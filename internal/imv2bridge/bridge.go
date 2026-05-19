// Package imv2bridge wires the wlserver-side v2 IM listener into
// the ibus.InputContext routing path. It exposes a Bridge that
// satisfies ibus.V2Router: when a downstream Wayland v2 IME has
// grabbed flexkb-imed's keyboard, ProcessKeyEvent routes there
// first; commit_string responses flow back via the ibus
// InputContext.Emit* hooks the bridge calls.
//
// Lifetime: one Bridge per running flexkb-imed process. Construct
// once at daemon startup with the ibus.Server (for Emit* hooks),
// then call Listen to open the side socket. Close tears it all
// down on daemon shutdown.
//
// Why a separate package, not a method on ibus.Server? Three
// reasons:
//   - Keeps the wlserver/wlim dependency out of internal/ibus,
//     which is otherwise pure-dbus.
//   - Lets tests exercise the bridge in isolation against a fake
//     ibus.Emitter and a hand-rolled downstream client.
//   - Makes the v2 tier a clean opt-in: flexkb-imed-without-v2 is
//     a flexkb-imed that just doesn't import imv2bridge.
package imv2bridge

import (
	"log/slog"
	"sync"
	"sync/atomic"

	"github.com/godbus/dbus/v5"

	"github.com/lapingvino/flexkb/internal/wlim"
	"github.com/lapingvino/flexkb/internal/wlserver"
)

// Emitter is the narrow surface the bridge needs on the ibus side
// to flow committed text back to the right input context. ibus.Server
// satisfies it via its existing EmitCommitText / EmitUpdatePreedit
// methods.
type Emitter interface {
	EmitCommitText(ctxPath dbus.ObjectPath, text string)
	EmitUpdatePreedit(ctxPath dbus.ObjectPath, text string, cursor uint32, visible bool)
	EmitHidePreedit(ctxPath dbus.ObjectPath)
}

// Bridge holds the wlserver listener and the per-connection state
// needed to satisfy ibus.V2Router. There is at most one currently-
// active downstream IME (the v2 protocol allows only one
// input_method per seat per connection, and the bridge picks the
// most-recently-created across connections as "the active one"
// when there are multiple).
type Bridge struct {
	emitter Emitter
	log     *slog.Logger

	listener *wlserver.Listener

	mu sync.Mutex
	// currentIM points at the most-recently-created downstream
	// input_method object that hasn't been destroyed. The grab,
	// if any, is on this IM.
	currentIM *wlim.ServerInputMethod
	// focusedPath is the ibus input context that currently has
	// focus. Set by NotifyFocusIn / cleared by NotifyFocusOut.
	// Commits / preedits the downstream IME sends route to this
	// path; if it's empty, we drop the commit (the IME got too
	// keen and started typing into nothing).
	focusedPath dbus.ObjectPath
	// commitBuffer holds the accumulated commit_string / preedit
	// state since the last commit(serial) request. The v2
	// protocol batches: per spec, neither commit_string nor
	// set_preedit_string takes effect until commit(serial) is
	// sent. The bridge mirrors that accumulation so it can emit
	// a single ibus CommitText / UpdatePreeditText per batch
	// rather than emitting on every partial.
	pendingCommit  string
	pendingPreedit pendingPreedit
	// keySerial is a per-grab monotonic counter for SendKey events.
	keySerial atomic.Uint32
}

type pendingPreedit struct {
	text        string
	cursorBegin int32
	cursorEnd   int32
	set         bool
}

// New constructs a Bridge attached to the given Emitter (typically
// the ibus.Server). The bridge starts inactive — call Listen to
// open the side socket.
func New(em Emitter, log *slog.Logger) *Bridge {
	if log == nil {
		log = slog.Default()
	}
	return &Bridge{emitter: em, log: log}
}

// Listen opens the side socket at path and starts accepting
// downstream v2 IME connections. Returns once the listener is
// up; the accept loop runs in its own goroutine.
//
// Returns an error if the socket can't be created (path conflict
// with a non-socket file, permissions, etc.). Stale socket files
// from a prior crashed instance are removed automatically.
func (b *Bridge) Listen(path string) error {
	l, err := wlserver.Listen(path, b.log, b.onAccept)
	if err != nil {
		return err
	}
	b.listener = l
	go func() {
		if err := l.Serve(); err != nil {
			b.log.Warn("v2 bridge listener exited", "err", err)
		}
	}()
	return nil
}

// SocketPath returns the path the bridge is listening on, or
// "" if Listen hasn't been called yet (or failed).
func (b *Bridge) SocketPath() string {
	if b.listener == nil {
		return ""
	}
	return b.listener.Path()
}

// Close tears down the listener and any active connections.
func (b *Bridge) Close() error {
	if b.listener == nil {
		return nil
	}
	return b.listener.Close()
}

// onAccept fires for every downstream IME that connects. It
// registers wl_display + wl_registry + the v2 manager global on
// the new connection. The OnInputMethodCreated callback bound to
// the manager is what wires the per-IM commit / preedit forwarding.
func (b *Bridge) onAccept(d *wlserver.Dispatcher) error {
	reg := wlserver.NewRegistry(d)
	if _, err := wlserver.RegisterDisplay(d, reg); err != nil {
		return err
	}
	wlim.AddManagerGlobal(reg, b.log, b)
	return nil
}

// --- wlim.ServerManagerCallback ---

// OnInputMethodCreated fires when a downstream client calls
// get_input_method. The bridge wires up commit / preedit / grab
// forwarding and records this IM as the current active one.
func (b *Bridge) OnInputMethodCreated(im *wlim.ServerInputMethod) {
	im.OnCommitString = func(text string) {
		b.mu.Lock()
		b.pendingCommit += text
		b.mu.Unlock()
	}
	im.OnSetPreeditString = func(text string, cb, ce int32) {
		b.mu.Lock()
		b.pendingPreedit = pendingPreedit{text: text, cursorBegin: cb, cursorEnd: ce, set: true}
		b.mu.Unlock()
	}
	im.OnDeleteSurroundingText = func(before, after uint32) {
		// flexkb-imed's ibus emitter doesn't yet flow
		// delete_surrounding_text to clients (the IBus signal
		// exists but our context.go doesn't wire it). Log and
		// drop for now; surrounding text deletion is rare in
		// the IM-tier hot path.
		b.log.Debug("v2 bridge: delete_surrounding_text dropped",
			"before", before, "after", after)
	}
	im.OnCommit = func(serial uint32) {
		b.flushPending()
	}
	im.OnGrabKeyboard = func(grab *wlim.ServerKeyboardGrab) {
		b.log.Debug("v2 bridge: keyboard grab accepted")
		// Reset per-grab serial.
		b.keySerial.Store(0)
	}
	im.OnDestroy = func() {
		b.mu.Lock()
		if b.currentIM == im {
			b.currentIM = nil
		}
		b.mu.Unlock()
	}

	b.mu.Lock()
	b.currentIM = im
	focused := b.focusedPath
	b.mu.Unlock()

	// If something already had focus when the IME connected, send
	// activate immediately so the IME knows it has a live target.
	if focused != "" {
		b.sendActivate(im)
	}
}

// OnInputMethodDestroyed fires when the downstream IME tears down
// (explicit destroy request, or connection close, or replaced by
// a second get_input_method which forces unavailable on the first).
func (b *Bridge) OnInputMethodDestroyed(im *wlim.ServerInputMethod) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.currentIM == im {
		b.currentIM = nil
	}
}

// flushPending emits the accumulated commit + preedit state to the
// focused input context's ibus path. Called from OnCommit.
func (b *Bridge) flushPending() {
	b.mu.Lock()
	path := b.focusedPath
	commit := b.pendingCommit
	preedit := b.pendingPreedit
	b.pendingCommit = ""
	b.pendingPreedit = pendingPreedit{}
	b.mu.Unlock()
	if path == "" {
		// IME committed into a void — no focused context. Quietly
		// drop. This races with FocusOut; not a bug.
		return
	}
	if commit != "" {
		b.emitter.EmitCommitText(path, commit)
	}
	if preedit.set {
		if preedit.text == "" {
			b.emitter.EmitHidePreedit(path)
		} else {
			b.emitter.EmitUpdatePreedit(path, preedit.text,
				uint32(preedit.cursorEnd), true)
		}
	}
}

// --- ibus.V2Router ---

// HasActiveGrab is true when there is an active downstream IME
// with a keyboard grab. ibus.InputContext.ProcessKeyEvent uses
// this to decide whether to consult the bridge at all.
func (b *Bridge) HasActiveGrab() bool {
	b.mu.Lock()
	im := b.currentIM
	b.mu.Unlock()
	if im == nil {
		return false
	}
	g := im.Grab()
	return g != nil && !g.Released()
}

// RouteKey forwards a keystroke to the downstream IME's keyboard
// grab. ibus's state argument is the X11-style bitmask:
//   Shift=1, Lock=2, Control=4, Mod1=8, Mod5=128 (AltGr).
// The v2 grab expects xkb-style depressed/latched/locked
// quartets, so we'd ideally translate fully — for v1 we map
// Shift/Lock/Ctrl/Mod1 directly into "depressed" and leave
// latched/locked at 0 (most IMEs only care about depressed for
// hot-path key processing).
//
// Returns consumed=true to suppress local processing. The IME's
// response is asynchronous: commit_string and friends arrive on
// other goroutines and the InputContext.Emit* hooks flow them
// back to the right path.
func (b *Bridge) RouteKey(ctxPath dbus.ObjectPath, keyval, keycode, state uint32) (bool, error) {
	b.mu.Lock()
	im := b.currentIM
	b.mu.Unlock()
	if im == nil {
		return false, nil
	}
	grab := im.Grab()
	if grab == nil || grab.Released() {
		return false, nil
	}
	// Map ibus state → xkb-modifiers shape. depressed gets the
	// raw X11 modifier bits; latched/locked stay 0 for v1. Group
	// 0 (the primary group) — multi-group XKB is a phase-3.2
	// concern.
	mods := state & 0xFF // mask out the 0x40000000 release flag etc.
	if err := grab.SendModifiers(b.nextSerial(), mods, 0, 0, 0); err != nil {
		return false, err
	}
	// keycode in ibus is X11 (Linux evdev + 8); wayland uses
	// raw evdev. Subtract 8.
	evcode := keycode
	if evcode >= 8 {
		evcode -= 8
	}
	// Timestamp: we don't have one from ibus, so just zero — the
	// IME's view of time is per-grab monotonic and most engines
	// don't actually use it.
	if err := grab.SendKey(b.nextSerial(), 0, evcode, 1 /*pressed*/); err != nil {
		return false, err
	}
	return true, nil
}

func (b *Bridge) nextSerial() uint32 {
	return b.keySerial.Add(1)
}

// NotifyFocusIn binds the focused input context to the bridge so
// commits route back to it.
func (b *Bridge) NotifyFocusIn(ctxPath dbus.ObjectPath) {
	b.mu.Lock()
	b.focusedPath = ctxPath
	im := b.currentIM
	b.mu.Unlock()
	if im != nil {
		b.sendActivate(im)
	}
}

// NotifyFocusOut clears focus and tells the downstream IME the
// session went away.
func (b *Bridge) NotifyFocusOut(ctxPath dbus.ObjectPath) {
	b.mu.Lock()
	if b.focusedPath == ctxPath {
		b.focusedPath = ""
	}
	im := b.currentIM
	b.mu.Unlock()
	if im != nil {
		_ = im.SendDeactivate()
		_ = im.SendDone()
	}
}

// sendActivate emits the activate + done pair the v2 protocol
// requires before the downstream IME starts processing.
func (b *Bridge) sendActivate(im *wlim.ServerInputMethod) {
	if err := im.SendActivate(); err != nil {
		b.log.Debug("v2 bridge: SendActivate failed", "err", err)
		return
	}
	if err := im.SendDone(); err != nil {
		b.log.Debug("v2 bridge: SendDone failed", "err", err)
	}
}
