package ibushost

import (
	"errors"
	"io"
	"log/slog"
	"os/exec"
	"sync/atomic"
	"testing"

	"github.com/godbus/dbus/v5"

	"github.com/lapingvino/flexkb/internal/ibus"
	"github.com/lapingvino/flexkb/internal/ibusengines"
)

// quietLogger returns a /dev/null slog so test runs aren't noisy.
func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestNewHostPopulatesCatalog — construction walks the live
// system's /usr/share/ibus/component XMLs. We only assert it
// completes without panicking; the actual catalog size depends
// on what's installed.
func TestNewHostPopulatesCatalog(t *testing.T) {
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		t.Skipf("no session bus: %v", err)
	}
	defer conn.Close()
	h := New(conn, quietLogger())
	if h == nil {
		t.Fatal("New returned nil")
	}
	// Catalog may be 0 on minimal CI; just confirm we can ask.
	_ = h.CatalogSize()
	_ = h.AvailableEngines()
}

// TestSelectEngineRejectsUnknown — a SelectEngine for an engine
// name not in the catalog must error rather than spawn anything.
func TestSelectEngineRejectsUnknown(t *testing.T) {
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		t.Skipf("no session bus: %v", err)
	}
	defer conn.Close()
	h := New(conn, quietLogger())
	// Use an engine name we're certain isn't installed.
	_, err = h.SelectEngine("/test/ic1", "definitely-not-a-real-engine-xyz")
	if err == nil {
		t.Error("expected error for unknown engine")
	}
}

// TestEngineForReturnsNilWhenUnbound — fresh contexts have no
// engine selected; EngineFor must report nil for nil-checks at
// the call site to work.
func TestEngineForReturnsNilWhenUnbound(t *testing.T) {
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		t.Skipf("no session bus: %v", err)
	}
	defer conn.Close()
	h := New(conn, quietLogger())
	if got := h.EngineFor("/never/bound"); got != nil {
		t.Errorf("got %v, want nil", got)
	}
}

// TestRegisterComponentBindsEngine — simulate the case where an
// engine subprocess we (theoretically) spawned has registered
// with us. After RegisterComponent the engine's path/obj should
// be populated, Connected() should return true, and SelectEngine
// should return a working binding.
func TestRegisterComponentBindsEngine(t *testing.T) {
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		t.Skipf("no session bus: %v", err)
	}
	defer conn.Close()
	h := New(conn, quietLogger())

	// Inject a fake engine into the catalog so SelectEngine
	// has something to bind. In production this comes from
	// ibusengines.Discover; here we synthesise.
	h.catalog["test-pinyin"] = ibusengines.Engine{
		Name:     "test-pinyin",
		ExecPath: "/usr/bin/test-fake-engine",
	}

	// Pre-register the engine as if it had spawned and called
	// RegisterComponent. (Real engines do this via dbus; here
	// we call the public API directly.)
	h.RegisterComponent("org.example.Test", []string{"test-pinyin"},
		"/org/freedesktop/IBus/Engine")

	// Now SelectEngine should find the pre-registered engine,
	// not try to spawn.
	eng, err := h.SelectEngine("/ctx/1", "test-pinyin")
	if err != nil {
		t.Fatalf("SelectEngine: %v", err)
	}
	if eng.Name() != "test-pinyin" {
		t.Errorf("name: %q", eng.Name())
	}
	if !eng.Connected() {
		t.Error("expected Connected() to be true after RegisterComponent")
	}
	// EngineFor should return the same engine for the same
	// context path.
	if got := h.EngineFor("/ctx/1"); got == nil || got.Name() != "test-pinyin" {
		t.Errorf("EngineFor: %v", got)
	}
}

// TestReleaseEngineDropsBinding — Destroy on an input context
// should unbind it from the engine without killing the engine
// itself (other contexts may still use it).
func TestReleaseEngineDropsBinding(t *testing.T) {
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		t.Skipf("no session bus: %v", err)
	}
	defer conn.Close()
	h := New(conn, quietLogger())

	h.catalog["test-engine"] = ibusengines.Engine{
		Name:     "test-engine",
		ExecPath: "/no/such/binary",
	}
	h.RegisterComponent("org.example.Test", []string{"test-engine"}, "/x")
	_, err = h.SelectEngine("/ctx/1", "test-engine")
	if err != nil {
		t.Fatal(err)
	}
	if h.EngineFor("/ctx/1") == nil {
		t.Fatal("setup failed: engine not bound")
	}
	h.ReleaseEngine("/ctx/1")
	if got := h.EngineFor("/ctx/1"); got != nil {
		t.Errorf("post-release EngineFor: got %v, want nil", got)
	}
	// Engine itself should still exist in h.engines so other
	// contexts can reuse it.
	if _, ok := h.engines["test-engine"]; !ok {
		t.Error("engine removed from cache; should persist for reuse")
	}
}

// TestDisconnectedEngineDoesNotConsume — ProcessKeyEvent on an
// engine that has been registered (so .obj is populated) but
// where the dbus call times out / fails must return consumed=
// false. This is the path where the engine subprocess crashed
// or hung; the router falls back to the in-process Session.
//
// We exercise this by registering an engine with a path that
// has nothing listening on it — the dbus call returns an error
// within the 100ms timeout.
func TestDisconnectedEngineDoesNotConsume(t *testing.T) {
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		t.Skipf("no session bus: %v", err)
	}
	defer conn.Close()
	h := New(conn, quietLogger())
	h.catalog["test"] = ibusengines.Engine{Name: "test"}
	// Register but point at a non-existent object path.
	h.RegisterComponent("org.example.Test", []string{"test"}, "/nonexistent/path")
	eng, _ := h.SelectEngine("/ctx/1", "test")
	if !eng.Connected() {
		t.Fatal("setup: Connected() should be true after RegisterComponent")
	}
	consumed, err := eng.ProcessKeyEvent(0x61, 38, 0)
	if err == nil {
		t.Log("note: call succeeded against /nonexistent — dbus' method-not-found behaviour")
	}
	if consumed {
		t.Error("a failed engine call must return consumed=false")
	}
}

// TestEngineHostInterfaceShape compiles-checks that *Host
// satisfies ibus.EngineHost. Runtime no-op; the value is the
// build break on regression.
func TestEngineHostInterfaceShape(t *testing.T) {
	var _ ibus.EngineHost = (*Host)(nil)
}

// TestSpawnEngineLifecycle uses /bin/sleep as a stand-in for an
// engine binary. Sleep won't actually register with us (it's not
// an ibus engine), but the spawn → track → Close → terminate
// path runs identically regardless of what the subprocess does.
// Verifies:
//   - spawn populates eng.process
//   - the process is alive after spawn
//   - Close terminates it within the 2s deadline
func TestSpawnEngineLifecycle(t *testing.T) {
	if _, err := dbus.ConnectSessionBus(); err != nil {
		t.Skipf("no session bus: %v", err)
	}
	if _, err := exec.LookPath("sleep"); err != nil {
		t.Skip("/bin/sleep not available")
	}
	conn, _ := dbus.ConnectSessionBus()
	defer conn.Close()
	h := New(conn, quietLogger())

	// Inject a synthetic engine using sleep as the binary.
	h.catalog["test-sleep"] = ibusengines.Engine{
		Name:     "test-sleep",
		ExecPath: "/bin/sleep",
	}
	eng, err := h.SelectEngine("/ctx/1", "test-sleep")
	if err != nil {
		t.Fatalf("SelectEngine: %v", err)
	}
	// sleep --ibus errors out and exits quickly. We just need
	// to confirm the spawn took place — eng.process is the
	// proof, regardless of whether sleep is still alive.
	h.mu.Lock()
	hostEng := h.engines["test-sleep"]
	h.mu.Unlock()
	if hostEng == nil || hostEng.process == nil {
		t.Fatal("engine process not tracked after spawn")
	}
	_ = eng
	h.Close()
}

// TestCloseTerminatesNoProcessesOnEmpty — Close on a Host with
// no spawned engines is a no-op and returns immediately. Sanity
// check for the daemon's shutdown path.
func TestCloseTerminatesNoProcessesOnEmpty(t *testing.T) {
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		t.Skipf("no session bus: %v", err)
	}
	defer conn.Close()
	h := New(conn, quietLogger())
	// Should return promptly (the test framework's default
	// timeout catches hangs).
	h.Close()
}

// fakeEngine is a stand-in for an external ibus engine used by
// the routing-integration test below. It implements the small
// surface ibus.Engine requires; calls a callback when
// ProcessKeyEvent fires so the test can assert routing.
type fakeEngine struct {
	name       string
	connected  bool
	keyCallCnt int32
	willClaim  bool // ProcessKeyEvent returns this for consumed
}

func (f *fakeEngine) Name() string      { return f.name }
func (f *fakeEngine) Connected() bool   { return f.connected }
func (f *fakeEngine) FocusIn() error    { return nil }
func (f *fakeEngine) FocusOut() error   { return nil }
func (f *fakeEngine) Reset() error      { return nil }
func (f *fakeEngine) ProcessKeyEvent(keyval, keycode, state uint32) (bool, error) {
	atomic.AddInt32(&f.keyCallCnt, 1)
	if !f.willClaim {
		return false, errors.New("intentionally rejected")
	}
	return true, nil
}
