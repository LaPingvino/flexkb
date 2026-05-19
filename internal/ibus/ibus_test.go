package ibus

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"

	"github.com/lapingvino/flexkb/internal/imsession"
	"github.com/lapingvino/flexkb/internal/model"
	"github.com/lapingvino/flexkb/internal/runtime"
)

// fakeFactory returns a Session against a minimal layout. Reused
// across every test that creates an InputContext; each call to
// factory hands out a fresh Session so per-context state stays
// isolated.
func fakeFactory() SessionFactory {
	return func() (*imsession.Session, error) {
		r := runtime.NewResolver(model.ComposedLayout{
			Symbols: map[string]model.KeySymbols{
				"AC01": {Levels: []string{"a", "A"}},
				"AC02": {Levels: []string{"s", "S"}},
			},
		})
		return imsession.New(r, nil)
	}
}

// quietLogger silences slog output during tests. Real test
// failures show via t.Errorf assertions, not log lines.
func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// withTestServer spins up an ibus.Server on a per-test unique
// dbus name so parallel runs don't collide with each other or
// with a running ibus-daemon. Returns the test server, a client
// conn into the same session bus, and a cleanup func.
//
// Skips the test when no session bus is reachable (CI without
// a dbus-daemon).
func withTestServer(t *testing.T) (*Server, *dbus.Conn, func()) {
	t.Helper()
	if _, err := dbus.ConnectSessionBus(); err != nil {
		t.Skipf("no session bus available: %v", err)
	}
	busName := fmt.Sprintf("org.flexkb.test._%d_%d", os.Getpid(), time.Now().UnixNano())
	srv, err := NewWithName(quietLogger(), fakeFactory(), busName)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := srv.Start(false); err != nil {
		srv.Close()
		t.Fatalf("Start: %v", err)
	}
	cli, err := dbus.ConnectSessionBus()
	if err != nil {
		srv.Close()
		t.Fatalf("client connect: %v", err)
	}
	cleanup := func() {
		cli.Close()
		srv.Close()
	}
	return srv, cli, cleanup
}

// TestCreateInputContextRoundTrip — basic acceptance: a client
// can call CreateInputContext, get a non-empty object path back,
// and the server tracks it.
func TestCreateInputContextRoundTrip(t *testing.T) {
	srv, cli, cleanup := withTestServer(t)
	defer cleanup()

	svcName := srv.BusName()
	obj := cli.Object(svcName, ServicePath)
	var path dbus.ObjectPath
	if err := obj.Call("org.freedesktop.IBus.CreateInputContext", 0, "test-app").Store(&path); err != nil {
		t.Fatalf("CreateInputContext: %v", err)
	}
	if path == "" {
		t.Error("got empty path")
	}
	srv.mu.Lock()
	_, exists := srv.contexts[path]
	srv.mu.Unlock()
	if !exists {
		t.Errorf("path %q not tracked in server contexts", path)
	}
}

// TestProcessKeyEventEmitsCommit — focus the context, send a
// keystroke, expect a CommitText signal. The end-to-end "an app
// gets text via ibus from flexkb-imed" path.
func TestProcessKeyEventEmitsCommit(t *testing.T) {
	srv, cli, cleanup := withTestServer(t)
	defer cleanup()

	svcName := srv.BusName()
	svc := cli.Object(svcName, ServicePath)
	var path dbus.ObjectPath
	if err := svc.Call("org.freedesktop.IBus.CreateInputContext", 0, "test").Store(&path); err != nil {
		t.Fatalf("CreateInputContext: %v", err)
	}

	// Subscribe to signals BEFORE issuing FocusIn / ProcessKeyEvent
	// — otherwise the signal may fire before our match rule is in
	// place and we'd never see it.
	if err := cli.AddMatchSignal(
		dbus.WithMatchInterface("org.freedesktop.IBus.InputContext"),
		dbus.WithMatchMember("CommitText"),
		dbus.WithMatchObjectPath(path),
	); err != nil {
		t.Fatalf("AddMatchSignal: %v", err)
	}
	sigCh := make(chan *dbus.Signal, 4)
	cli.Signal(sigCh)

	ic := cli.Object(svcName, path)
	if err := ic.Call("org.freedesktop.IBus.InputContext.FocusIn", 0).Store(); err != nil {
		t.Fatalf("FocusIn: %v", err)
	}

	// Send keystroke: AC01 ('a') with no modifiers, press.
	// X11 keycode = Linux scancode + 8. AC01 = Linux 30 → X11 38.
	var consumed bool
	if err := ic.Call("org.freedesktop.IBus.InputContext.ProcessKeyEvent", 0,
		uint32(0x61), uint32(38), uint32(0)).Store(&consumed); err != nil {
		t.Fatalf("ProcessKeyEvent: %v", err)
	}
	if !consumed {
		t.Error("ProcessKeyEvent returned false (key not consumed)")
	}

	// Wait for the commit signal.
	select {
	case sig := <-sigCh:
		if sig.Name != "org.freedesktop.IBus.InputContext.CommitText" {
			t.Errorf("got signal %q", sig.Name)
		}
		if len(sig.Body) == 0 {
			t.Error("CommitText signal has empty body")
		}
		// First arg should be the IBusText variant. We just
		// confirm it's structurally a variant; verifying the
		// exact UTF-8 text out of the variant struct is a deeper
		// dbus dance.
	case <-time.After(2 * time.Second):
		t.Fatal("no CommitText signal within 2s")
	}
}

// TestProcessKeyEventBeforeFocusIsIgnored — sending keystrokes
// before FocusIn must NOT emit commits. Some apps stutter at
// startup and we shouldn't drop input into them.
func TestProcessKeyEventBeforeFocusIsIgnored(t *testing.T) {
	srv, cli, cleanup := withTestServer(t)
	defer cleanup()

	svcName := srv.BusName()
	svc := cli.Object(svcName, ServicePath)
	var path dbus.ObjectPath
	if err := svc.Call("org.freedesktop.IBus.CreateInputContext", 0, "test").Store(&path); err != nil {
		t.Fatalf("CreateInputContext: %v", err)
	}

	ic := cli.Object(svcName, path)
	var consumed bool
	if err := ic.Call("org.freedesktop.IBus.InputContext.ProcessKeyEvent", 0,
		uint32(0x61), uint32(38), uint32(0)).Store(&consumed); err != nil {
		t.Fatalf("ProcessKeyEvent: %v", err)
	}
	if consumed {
		t.Error("ProcessKeyEvent before FocusIn was consumed (should be false)")
	}
}

// TestKeyReleaseNotConsumed — bit 0x40000000 in state means key
// release per ibus convention. Releases must return false so
// apps see them.
func TestKeyReleaseNotConsumed(t *testing.T) {
	srv, _, cleanup := withTestServer(t)
	defer cleanup()

	// Direct method call on the InputContext — go through the
	// struct rather than dbus to avoid the round-trip and the
	// signal-subscription dance.
	sess, _ := fakeFactory()()
	ic := &InputContext{
		srv:     srv,
		path:    "/test/ic",
		sess:    sess,
		focused: true,
	}
	consumed, _ := ic.ProcessKeyEvent(0x61, 38, 0x40000000) // release flag
	if consumed {
		t.Error("key release was consumed (should be false)")
	}
}

// TestDestroyRemovesFromContextMap — verifies cleanup. ibus
// clients call Destroy on app shutdown; if we leak we'd grow
// the contexts map indefinitely over a session lifetime.
func TestDestroyRemovesFromContextMap(t *testing.T) {
	srv, cli, cleanup := withTestServer(t)
	defer cleanup()

	svcName := srv.BusName()
	svc := cli.Object(svcName, ServicePath)
	var path dbus.ObjectPath
	if err := svc.Call("org.freedesktop.IBus.CreateInputContext", 0, "test").Store(&path); err != nil {
		t.Fatalf("CreateInputContext: %v", err)
	}

	ic := cli.Object(svcName, path)
	if err := ic.Call("org.freedesktop.IBus.InputContext.Destroy", 0).Store(); err != nil {
		t.Fatalf("Destroy: %v", err)
	}

	// Give the export a moment to take effect.
	time.Sleep(50 * time.Millisecond)
	srv.mu.Lock()
	_, stillThere := srv.contexts[path]
	srv.mu.Unlock()
	if stillThere {
		t.Errorf("path %q still in contexts map after Destroy", path)
	}
}

// TestConcurrentInputContexts — multiple apps focusing
// independently must each get their own context with isolated
// sessions. Probes the per-context Session creation.
func TestConcurrentInputContexts(t *testing.T) {
	srv, cli, cleanup := withTestServer(t)
	defer cleanup()

	svcName := srv.BusName()
	svc := cli.Object(svcName, ServicePath)

	const N = 5
	paths := make([]dbus.ObjectPath, N)
	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			var p dbus.ObjectPath
			if err := svc.Call("org.freedesktop.IBus.CreateInputContext", 0, "test").Store(&p); err != nil {
				t.Errorf("CreateInputContext %d: %v", idx, err)
				return
			}
			paths[idx] = p
		}(i)
	}
	wg.Wait()

	seen := map[dbus.ObjectPath]bool{}
	for _, p := range paths {
		if p == "" {
			t.Error("got empty path")
			continue
		}
		if seen[p] {
			t.Errorf("duplicate path: %s", p)
		}
		seen[p] = true
	}
}

// fakeEngine is the test substitute for an external ibus engine.
// It implements ibus.Engine but doesn't actually speak dbus —
// the test asserts the routing decision, not the wire format
// (which is exercised by the production code path's existing
// session-bus tests).
type fakeEngine struct {
	name      string
	connected bool
	calls     int
	willClaim bool
}

func (f *fakeEngine) Name() string    { return f.name }
func (f *fakeEngine) Connected() bool { return f.connected }
func (f *fakeEngine) ProcessKeyEvent(uint32, uint32, uint32) (bool, error) {
	f.calls++
	return f.willClaim, nil
}
func (f *fakeEngine) FocusIn() error  { return nil }
func (f *fakeEngine) FocusOut() error { return nil }
func (f *fakeEngine) Reset() error    { return nil }

// fakeHost is the test substitute for ibushost.Host. Returns
// one pre-built fakeEngine for any context that asks.
type fakeHost struct {
	engine    *fakeEngine
	released  map[dbus.ObjectPath]bool
	selected  map[dbus.ObjectPath]string
	registered []string
}

func (h *fakeHost) EngineFor(p dbus.ObjectPath) Engine {
	if h.selected[p] != "" {
		return h.engine
	}
	return nil
}
func (h *fakeHost) SelectEngine(p dbus.ObjectPath, name string) (Engine, error) {
	if h.selected == nil {
		h.selected = map[dbus.ObjectPath]string{}
	}
	h.selected[p] = name
	return h.engine, nil
}
func (h *fakeHost) ReleaseEngine(p dbus.ObjectPath) {
	if h.released == nil {
		h.released = map[dbus.ObjectPath]bool{}
	}
	h.released[p] = true
	delete(h.selected, p)
}
func (h *fakeHost) RegisterComponent(name string, engines []string, _ dbus.ObjectPath) {
	h.registered = append(h.registered, name)
}

// TestEngineRoutingWhenBound — SetEngine binds the input context
// to the fake engine; subsequent ProcessKeyEvent calls go to
// the engine first and are consumed when the engine claims them.
func TestEngineRoutingWhenBound(t *testing.T) {
	srv, _, cleanup := withTestServer(t)
	defer cleanup()

	host := &fakeHost{engine: &fakeEngine{name: "fake", connected: true, willClaim: true}}
	srv.SetEngineHost(host)

	sess, _ := fakeFactory()()
	ic := &InputContext{
		srv:     srv,
		path:    "/test/ic",
		sess:    sess,
		focused: true,
	}
	// Bind the engine.
	if err := ic.SetEngine("fake"); err != nil {
		t.Fatalf("SetEngine: %v", err)
	}
	consumed, _ := ic.ProcessKeyEvent(0x61, 38, 0)
	if !consumed {
		t.Error("expected engine-claimed keystroke to be consumed")
	}
	if host.engine.calls != 1 {
		t.Errorf("engine call count: got %d, want 1", host.engine.calls)
	}
}

// TestEngineRoutingFallsBackOnNotConsumed — when the engine
// returns consumed=false, we fall through to the in-process
// Session. Verifies the "engine first, fall back" semantics.
func TestEngineRoutingFallsBackOnNotConsumed(t *testing.T) {
	srv, _, cleanup := withTestServer(t)
	defer cleanup()

	host := &fakeHost{engine: &fakeEngine{name: "fake", connected: true, willClaim: false}}
	srv.SetEngineHost(host)

	sess, _ := fakeFactory()()
	ic := &InputContext{
		srv:     srv,
		path:    "/test/ic",
		sess:    sess,
		focused: true,
	}
	_ = ic.SetEngine("fake")
	consumed, _ := ic.ProcessKeyEvent(0x61, 38, 0)
	// The fakeFactory's Session has no IM loaded — it falls
	// back to "commit the resolved symbol" passthrough. That
	// commits "a" → consumed=true via Session path.
	if !consumed {
		t.Error("Session fallback should have consumed the AC01 keystroke")
	}
	if host.engine.calls != 1 {
		t.Errorf("engine should have been asked first: got %d calls, want 1", host.engine.calls)
	}
}

// TestDestroyReleasesEngineBinding — destroying the input
// context must invoke ReleaseEngine on the host so engines
// aren't left thinking dead contexts are still active.
func TestDestroyReleasesEngineBinding(t *testing.T) {
	srv, _, cleanup := withTestServer(t)
	defer cleanup()

	host := &fakeHost{engine: &fakeEngine{name: "fake", connected: true}}
	srv.SetEngineHost(host)

	sess, _ := fakeFactory()()
	ic := &InputContext{srv: srv, path: "/test/ic", sess: sess}
	srv.mu.Lock()
	srv.contexts["/test/ic"] = ic
	srv.mu.Unlock()
	_ = ic.SetEngine("fake")

	_ = ic.Destroy()
	if !host.released["/test/ic"] {
		t.Error("ReleaseEngine not called on Destroy")
	}
}
