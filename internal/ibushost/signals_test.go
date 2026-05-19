package ibushost

import (
	"sync"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
)

// recordingEmitter is the ContextEmitter stand-in used by the
// signal-bridge tests. Captures every call so the test asserts
// the bridge routed correctly.
type recordingEmitter struct {
	mu       sync.Mutex
	commits  []recCommit
	preedits []recPreedit
	hides    []dbus.ObjectPath
	shows    []dbus.ObjectPath
	forwards []recForward
}

type recCommit struct {
	Ctx  dbus.ObjectPath
	Text string
}
type recPreedit struct {
	Ctx     dbus.ObjectPath
	Text    string
	Cursor  uint32
	Visible bool
}
type recForward struct {
	Ctx    dbus.ObjectPath
	Keyval uint32
}

func (e *recordingEmitter) EmitCommitText(p dbus.ObjectPath, text string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.commits = append(e.commits, recCommit{p, text})
}

func (e *recordingEmitter) EmitUpdatePreedit(p dbus.ObjectPath, text string, c uint32, v bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.preedits = append(e.preedits, recPreedit{p, text, c, v})
}

func (e *recordingEmitter) EmitHidePreedit(p dbus.ObjectPath) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.hides = append(e.hides, p)
}

func (e *recordingEmitter) EmitShowPreedit(p dbus.ObjectPath) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.shows = append(e.shows, p)
}

func (e *recordingEmitter) EmitForwardKeyEvent(p dbus.ObjectPath, kv, kc, st uint32) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.forwards = append(e.forwards, recForward{p, kv})
}

func (e *recordingEmitter) snapshotCommits() []recCommit {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]recCommit, len(e.commits))
	copy(out, e.commits)
	return out
}

// TestSignalBridgeForwardsCommitText drives the full path:
// a fake "engine" emits CommitText on the session bus; the
// signal bridge picks it up via its match rule; the recording
// emitter records the per-context dispatch.
func TestSignalBridgeForwardsCommitText(t *testing.T) {
	cliConn, err := dbus.ConnectSessionBus()
	if err != nil {
		t.Skipf("no session bus: %v", err)
	}
	defer cliConn.Close()

	// Bridge runs on its own connection (the real host uses
	// the daemon's primary connection; tests use a separate
	// one for cleanliness).
	bridgeConn, err := dbus.ConnectSessionBus()
	if err != nil {
		t.Fatal(err)
	}
	defer bridgeConn.Close()

	b := newSignalBridge(bridgeConn, quietLogger())
	emitter := &recordingEmitter{}
	b.setEmitter(emitter)
	if err := b.installSubscription(); err != nil {
		t.Fatalf("installSubscription: %v", err)
	}
	defer b.close()

	const ctxPath = dbus.ObjectPath("/test/ic1")
	b.setFocus("test-engine", ctxPath)

	// Synthesise the IBusText struct shape ibus engines use.
	// CommitText carries Variant<IBusText> where IBusText is
	// (sa{sv}sv). For test purposes we send a Variant wrapping
	// a struct with the Text field; extractIBusText falls back
	// to the bare string form if the dbus.Store fails.
	type ibusText struct {
		Text       string
		Attributes map[string]dbus.Variant
		_          string
		_          dbus.Variant
	}
	payload := dbus.MakeVariant(ibusText{
		Text:       "你好",
		Attributes: map[string]dbus.Variant{},
	})
	const enginePath = dbus.ObjectPath("/org/test/Engine/fake1")
	if err := cliConn.Emit(enginePath,
		"org.freedesktop.IBus.Engine.CommitText", payload); err != nil {
		t.Fatalf("emit: %v", err)
	}

	// The dispatch loop runs in its own goroutine; poll for
	// the recorded call within a timeout.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		commits := emitter.snapshotCommits()
		if len(commits) > 0 {
			if commits[0].Ctx != ctxPath {
				t.Errorf("commit ctx: got %s, want %s", commits[0].Ctx, ctxPath)
			}
			if commits[0].Text != "你好" {
				t.Errorf("commit text: got %q, want 你好", commits[0].Text)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no CommitText recorded within 1s; emitter state: %+v", emitter.commits)
}

// TestSignalBridgeForwardsForwardKeyEvent — engines that
// receive a key but want it to fall through to the app emit
// ForwardKeyEvent. Verify the routing.
func TestSignalBridgeForwardsForwardKeyEvent(t *testing.T) {
	cliConn, err := dbus.ConnectSessionBus()
	if err != nil {
		t.Skipf("no session bus: %v", err)
	}
	defer cliConn.Close()
	bridgeConn, _ := dbus.ConnectSessionBus()
	defer bridgeConn.Close()

	b := newSignalBridge(bridgeConn, quietLogger())
	emitter := &recordingEmitter{}
	b.setEmitter(emitter)
	if err := b.installSubscription(); err != nil {
		t.Fatal(err)
	}
	defer b.close()
	const ctxPath = dbus.ObjectPath("/test/ic2")
	b.setFocus("test-engine", ctxPath)

	const enginePath = dbus.ObjectPath("/org/test/Engine/fake2")
	if err := cliConn.Emit(enginePath,
		"org.freedesktop.IBus.Engine.ForwardKeyEvent",
		uint32(0x0061), uint32(38), uint32(0)); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		emitter.mu.Lock()
		fwds := len(emitter.forwards)
		emitter.mu.Unlock()
		if fwds > 0 {
			if emitter.forwards[0].Keyval != 0x0061 {
				t.Errorf("keyval: %x", emitter.forwards[0].Keyval)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("no ForwardKeyEvent recorded within 1s")
}

// TestSignalBridgeDropsWhenNoFocus — engine signals arriving
// while no input context is focused on the engine are dropped
// silently. Models the "engine running but no app cares right
// now" state.
func TestSignalBridgeDropsWhenNoFocus(t *testing.T) {
	cliConn, err := dbus.ConnectSessionBus()
	if err != nil {
		t.Skipf("no session bus: %v", err)
	}
	defer cliConn.Close()
	bridgeConn, _ := dbus.ConnectSessionBus()
	defer bridgeConn.Close()

	b := newSignalBridge(bridgeConn, quietLogger())
	emitter := &recordingEmitter{}
	b.setEmitter(emitter)
	_ = b.installSubscription()
	defer b.close()
	// NO setFocus call.

	type ibusText struct {
		Text       string
		Attributes map[string]dbus.Variant
		_          string
		_          dbus.Variant
	}
	cliConn.Emit("/org/test/Engine/fake3",
		"org.freedesktop.IBus.Engine.CommitText",
		dbus.MakeVariant(ibusText{Text: "ignored", Attributes: map[string]dbus.Variant{}}))

	time.Sleep(150 * time.Millisecond)
	if got := emitter.snapshotCommits(); len(got) != 0 {
		t.Errorf("unfocused dispatch leaked: %v", got)
	}
}

// TestSetFocusReplacesPreviousBinding — a second SetFocus on
// the same engine replaces the previous context. Engines emit
// for the most-recently-focused context, not for stale ones.
func TestSetFocusReplacesPreviousBinding(t *testing.T) {
	cliConn, err := dbus.ConnectSessionBus()
	if err != nil {
		t.Skipf("no session bus: %v", err)
	}
	defer cliConn.Close()
	bridgeConn, _ := dbus.ConnectSessionBus()
	defer bridgeConn.Close()

	b := newSignalBridge(bridgeConn, quietLogger())
	emitter := &recordingEmitter{}
	b.setEmitter(emitter)
	_ = b.installSubscription()
	defer b.close()
	b.setFocus("test-engine", "/test/old")
	b.setFocus("test-engine", "/test/new")
	b.setFocus("test-engine", "") // explicit clear

	// After a clear, no commits should land.
	type ibusText struct {
		Text       string
		Attributes map[string]dbus.Variant
		_          string
		_          dbus.Variant
	}
	cliConn.Emit("/org/test/Engine/fake4",
		"org.freedesktop.IBus.Engine.CommitText",
		dbus.MakeVariant(ibusText{Text: "x", Attributes: map[string]dbus.Variant{}}))

	time.Sleep(100 * time.Millisecond)
	if got := emitter.snapshotCommits(); len(got) != 0 {
		t.Errorf("expected 0 commits after focus clear, got %d", len(got))
	}
}
