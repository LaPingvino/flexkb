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
