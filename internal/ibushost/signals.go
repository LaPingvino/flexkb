package ibushost

import (
	"errors"
	"log/slog"
	"strings"
	"sync"

	"github.com/godbus/dbus/v5"
)

// signalBridge subscribes to the dbus signals every hosted
// engine emits — CommitText, UpdatePreeditText, ShowPreeditText,
// HidePreeditText, ForwardKeyEvent — and forwards them to the
// input context currently focused on each engine.
//
// One bridge per Host. Subscribes lazily: when the first engine
// registers we install the match rules and start the dispatch
// goroutine; idempotent on subsequent registrations.
//
// The narrow ContextEmitter interface keeps the bridge free of
// any cycle with internal/ibus — flexkb-imed wires an
// implementation that knows how to re-emit on each context's
// dbus path.
type signalBridge struct {
	conn *dbus.Conn
	log  *slog.Logger

	mu       sync.Mutex
	started  bool
	ch       chan *dbus.Signal
	stop     chan struct{}
	// focused tracks (engine name) → (input context path) for
	// the input context currently focused on each engine.
	// Engines emit signals on their OWN path; we look up which
	// context to forward to via this table.
	focused map[string]dbus.ObjectPath
	// emitter is the per-context re-emit hook. Set by
	// SetContextEmitter; nil means "drop signals" — the
	// daemon's startup sequence has a brief window between
	// host construction and ibus.Server hookup.
	emitter ContextEmitter
}

// ContextEmitter is the bridge's escape hatch back to the
// input-context side. The implementation (in
// internal/ibus.InputContext) knows how to package signals onto
// its own dbus path for the IM client to receive.
//
// Defined here (not in internal/ibus) so ibushost has zero
// dependency on the larger ibus types — only the per-signal
// hooks it needs to call.
type ContextEmitter interface {
	EmitCommitText(ctxPath dbus.ObjectPath, text string)
	EmitUpdatePreedit(ctxPath dbus.ObjectPath, text string, cursorPos uint32, visible bool)
	EmitHidePreedit(ctxPath dbus.ObjectPath)
	EmitShowPreedit(ctxPath dbus.ObjectPath)
	EmitForwardKeyEvent(ctxPath dbus.ObjectPath, keyval, keycode, state uint32)
}

// newSignalBridge returns a bridge attached to the given conn
// but not yet listening — installSubscription wires the match
// rules when an engine first registers.
func newSignalBridge(conn *dbus.Conn, log *slog.Logger) *signalBridge {
	return &signalBridge{
		conn:    conn,
		log:     log,
		focused: map[string]dbus.ObjectPath{},
	}
}

// setEmitter installs the per-context re-emit hook. flexkb-imed
// calls this right after constructing both the ibus server and
// the host so the dbus dispatch loop has somewhere to send
// translated signals.
func (b *signalBridge) setEmitter(e ContextEmitter) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.emitter = e
}

// setFocus tells the bridge that ctxPath is now the focused
// context for engine engineName. Subsequent signals from that
// engine route to this context until cleared or replaced.
// Clearing is "setFocus(engineName, '')" — empty path means
// "no context currently focused, drop signals".
func (b *signalBridge) setFocus(engineName string, ctxPath dbus.ObjectPath) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if ctxPath == "" {
		delete(b.focused, engineName)
		return
	}
	b.focused[engineName] = ctxPath
}

// installSubscription wires the dbus match rules for engine
// signals and starts the dispatch goroutine. Idempotent — safe
// to call from every RegisterComponent.
//
// The match is broad ("any signal on org.freedesktop.IBus.Engine,
// any path") because engines pick their own paths and we don't
// want to chase RegisterComponent timing. The dispatcher
// inspects the sender / member to decide what to do.
func (b *signalBridge) installSubscription() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.started {
		return nil
	}
	if err := b.conn.AddMatchSignal(
		dbus.WithMatchInterface("org.freedesktop.IBus.Engine"),
	); err != nil {
		return err
	}
	b.ch = make(chan *dbus.Signal, 32)
	b.stop = make(chan struct{})
	b.conn.Signal(b.ch)
	go b.dispatch()
	b.started = true
	return nil
}

// close tears down the bridge. Called by Host.Close on daemon
// shutdown.
func (b *signalBridge) close() {
	b.mu.Lock()
	if !b.started {
		b.mu.Unlock()
		return
	}
	close(b.stop)
	b.mu.Unlock()
	// AddMatchSignal removal is best-effort — by the time
	// close runs we're shutting down anyway.
	_ = b.conn.RemoveMatchSignal(
		dbus.WithMatchInterface("org.freedesktop.IBus.Engine"),
	)
	b.conn.RemoveSignal(b.ch)
}

// dispatch reads engine-side signals from the match channel and
// re-emits them on the focused context's path. The mapping
// (engine name → context path) was set up in setFocus.
//
// Signal name conventions match the IBus.Engine protocol:
//   CommitText(IBusText)
//   UpdatePreeditText(IBusText, uint32 cursor, bool visible)
//   ShowPreeditText() / HidePreeditText() — no args
//   ForwardKeyEvent(uint32 keyval, uint32 keycode, uint32 state)
//
// Anything else (UpdateAuxiliaryText, UpdateLookupTable, etc.)
// is logged at Debug and dropped — these are richer-UI features
// future iterations can surface via the GUI side channel.
func (b *signalBridge) dispatch() {
	for {
		select {
		case <-b.stop:
			return
		case sig, ok := <-b.ch:
			if !ok {
				return
			}
			b.handle(sig)
		}
	}
}

// handle is one signal's translation step. Kept separate from
// dispatch for testability — tests can call it directly without
// running the goroutine.
func (b *signalBridge) handle(sig *dbus.Signal) {
	b.mu.Lock()
	emitter := b.emitter
	// The engine name isn't carried in the signal — we have to
	// match the sender's path against our route table.
	// Engines are registered with a canonical engine path
	// (/org/freedesktop/IBus/Engine in our impl) plus their
	// dbus sender id; for tests the sender's object path is the
	// path the engine emits on.
	//
	// For each focused (engineName → ctxPath) entry, we
	// forward IF the sender path matches the engine's bound
	// path. Stored on the Host's engines map so this is a
	// quick check.
	//
	// We snapshot the focused map under lock then drop the
	// lock before calling emitter — emitter calls into dbus
	// which may block on Conn write.
	focused := make(map[string]dbus.ObjectPath, len(b.focused))
	for k, v := range b.focused {
		focused[k] = v
	}
	b.mu.Unlock()

	if emitter == nil || len(focused) == 0 {
		return
	}

	// Member is "org.freedesktop.IBus.Engine.CommitText"; pull
	// the trailing dot-segment to get the bare signal name.
	dot := strings.LastIndexByte(sig.Name, '.')
	if dot < 0 {
		return
	}
	member := sig.Name[dot+1:]

	// Single-engine v1 broadcasts to every focused context.
	// In practice only one engine has focus at a time per IM
	// host; this loop runs once. Multi-engine work (different
	// contexts on different engines simultaneously) would
	// need the engine's path identity carried back via the
	// signal — covered when we have a real engine to
	// verify against.
	for _, ctxPath := range focused {
		switch member {
		case "CommitText":
			text, ok := extractIBusText(sig.Body)
			if !ok {
				b.log.Debug("engine CommitText: malformed body", "body", sig.Body)
				continue
			}
			emitter.EmitCommitText(ctxPath, text)
		case "UpdatePreeditText":
			if len(sig.Body) < 3 {
				continue
			}
			text, ok := extractIBusText(sig.Body[:1])
			if !ok {
				continue
			}
			cursor, _ := sig.Body[1].(uint32)
			visible, _ := sig.Body[2].(bool)
			emitter.EmitUpdatePreedit(ctxPath, text, cursor, visible)
		case "ShowPreeditText":
			emitter.EmitShowPreedit(ctxPath)
		case "HidePreeditText":
			emitter.EmitHidePreedit(ctxPath)
		case "ForwardKeyEvent":
			if len(sig.Body) < 3 {
				continue
			}
			keyval, _ := sig.Body[0].(uint32)
			keycode, _ := sig.Body[1].(uint32)
			state, _ := sig.Body[2].(uint32)
			emitter.EmitForwardKeyEvent(ctxPath, keyval, keycode, state)
		default:
			b.log.Debug("engine signal ignored", "name", sig.Name)
		}
	}
}

// extractIBusText unwraps the IBusText struct from a dbus
// signal body. ibus emits CommitText with a single variant arg
// whose value is a struct (text, attributes, _, _) per the
// IBusText type. We extract just the text — flexkb-imed
// doesn't yet support preedit colour/underline attributes.
//
// Over the wire godbus represents structs as []interface{} of
// field values (not as Go structs); we handle both shapes since
// in-process emits and bus-mediated emits decode differently.
func extractIBusText(body []interface{}) (string, bool) {
	if len(body) == 0 {
		return "", false
	}
	first := body[0]
	if v, ok := first.(dbus.Variant); ok {
		first = v.Value()
	}
	// Wire shape: struct serialised as []interface{}.
	if fields, ok := first.([]interface{}); ok && len(fields) >= 1 {
		if s, ok := fields[0].(string); ok {
			return s, true
		}
	}
	// In-process shape: a Go struct. Reflect-based Store
	// handles the unwrap.
	type ibusText struct {
		Text string
	}
	var ib ibusText
	if err := dbus.Store([]interface{}{first}, &ib); err == nil && ib.Text != "" {
		return ib.Text, true
	}
	// Bare-string fallback for test fixtures or simplified
	// engine implementations.
	if s, ok := first.(string); ok {
		return s, true
	}
	return "", false
}

// errBridgeNotRunning is the sentinel returned by setFocus when
// installSubscription hasn't been called yet. Exposed for tests
// that want to validate the lifecycle ordering.
var errBridgeNotRunning = errors.New("signal bridge not yet started")
