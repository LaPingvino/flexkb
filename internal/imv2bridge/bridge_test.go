package imv2bridge

import (
	"net"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"

	"github.com/lapingvino/flexkb/internal/wlim"
	"github.com/lapingvino/flexkb/internal/wlwire"
)

// fakeEmitter records calls back from the bridge into the
// ibus layer. The real ibus.Server satisfies the Emitter
// interface; for tests we only need the recording.
type fakeEmitter struct {
	mu       sync.Mutex
	commits  []emitCommit
	preedits []emitPreedit
	hides    []dbus.ObjectPath
}

type emitCommit struct {
	Path dbus.ObjectPath
	Text string
}
type emitPreedit struct {
	Path    dbus.ObjectPath
	Text    string
	Cursor  uint32
	Visible bool
}

func (e *fakeEmitter) EmitCommitText(p dbus.ObjectPath, text string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.commits = append(e.commits, emitCommit{p, text})
}
func (e *fakeEmitter) EmitUpdatePreedit(p dbus.ObjectPath, text string, cursor uint32, visible bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.preedits = append(e.preedits, emitPreedit{p, text, cursor, visible})
}
func (e *fakeEmitter) EmitHidePreedit(p dbus.ObjectPath) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.hides = append(e.hides, p)
}

// TestBridgeEndToEnd — stand up the bridge listener, connect a
// fake downstream IME, bind the manager, create an input method,
// grab the keyboard. Then drive: focus in, route a key, IME
// commits text. Verify the emitter saw the commit on the right
// path.
func TestBridgeEndToEnd(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "v2.sock")

	em := &fakeEmitter{}
	b := New(em, nil)
	if err := b.Listen(path); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer b.Close()

	// Fake downstream IME connects.
	addr, _ := net.ResolveUnixAddr("unix", path)
	cli, err := net.DialUnix("unix", nil, addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer cli.Close()

	// get_registry → bind manager → get_input_method → grab_keyboard.
	const registryID = uint32(2)
	const managerID = uint32(3)
	const seatID = uint32(4)
	const imID = uint32(5)
	const grabID = uint32(6)

	body := wlwire.NewEncoder()
	body.PutUint(registryID)
	if err := wlwire.WriteMessage(cli, 1, 1, body.Bytes()); err != nil {
		t.Fatal(err)
	}

	// Read the global event.
	cli.SetReadDeadline(time.Now().Add(time.Second))
	h, gbody, err := wlwire.ReadMessage(cli)
	if err != nil || h.ObjectID != registryID {
		t.Fatalf("global event: %+v err=%v", h, err)
	}
	dec := wlwire.NewDecoder(gbody)
	name := dec.Uint()
	iface := dec.String()
	version := dec.Uint()
	if iface != wlim.InterfaceManager {
		t.Fatalf("iface: %q", iface)
	}

	// Bind manager.
	bb := wlwire.NewEncoder()
	bb.PutUint(name)
	bb.PutString(iface)
	bb.PutUint(version)
	bb.PutUint(managerID)
	wlwire.WriteMessage(cli, registryID, 0, bb.Bytes())

	// get_input_method.
	gi := wlwire.NewEncoder()
	gi.PutUint(seatID)
	gi.PutUint(imID)
	wlwire.WriteMessage(cli, managerID, 0, gi.Bytes())

	// grab_keyboard.
	gk := wlwire.NewEncoder()
	gk.PutUint(grabID)
	wlwire.WriteMessage(cli, imID, 5, gk.Bytes())

	// Give the bridge a beat to register everything.
	if !waitFor(func() bool {
		return b.HasActiveGrab()
	}) {
		t.Fatal("HasActiveGrab never became true")
	}

	// Daemon side: focus an input context.
	const ctxPath = dbus.ObjectPath("/test/ic1")
	b.NotifyFocusIn(ctxPath)

	// Bridge should have emitted activate + done on the IM. Drain
	// them.
	cli.SetReadDeadline(time.Now().Add(time.Second))
	for i := 0; i < 2; i++ {
		h, _, err := wlwire.ReadMessage(cli)
		if err != nil {
			t.Fatalf("activate/done read: %v", err)
		}
		if h.ObjectID != imID {
			t.Fatalf("activate/done not addressed to im: id=%d", h.ObjectID)
		}
	}

	// Daemon side: route a key. ibus state form.
	consumed, err := b.RouteKey(ctxPath, 0x61 /*'a'*/, 38 /*xkb code for AC01*/, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !consumed {
		t.Error("RouteKey should consume when grab is active")
	}

	// Drain the modifiers + key events on the client side.
	cli.SetReadDeadline(time.Now().Add(time.Second))
	gotModifiers := false
	gotKey := false
	for !gotModifiers || !gotKey {
		h, body, err := wlwire.ReadMessage(cli)
		if err != nil {
			t.Fatal(err)
		}
		if h.ObjectID != grabID {
			t.Fatalf("grab event addressed to %d, want %d", h.ObjectID, grabID)
		}
		switch h.Opcode {
		case 1: // key
			dec := wlwire.NewDecoder(body)
			_ = dec.Uint() // serial
			_ = dec.Uint() // time
			ev := dec.Uint()
			st := dec.Uint()
			if ev != 30 /* evdev KEY_A = X11 code 38 minus 8 */ {
				t.Errorf("key evcode: got %d, want 30", ev)
			}
			if st != 1 {
				t.Errorf("key state: %d", st)
			}
			gotKey = true
		case 2: // modifiers
			gotModifiers = true
		default:
			t.Errorf("unexpected opcode on grab: %d", h.Opcode)
		}
	}

	// IME side: commit_string + commit. The bridge should record
	// the emitter call on ctxPath.
	cs := wlwire.NewEncoder()
	cs.PutString("a")
	wlwire.WriteMessage(cli, imID, 0 /*commit_string*/, cs.Bytes())

	cm := wlwire.NewEncoder()
	cm.PutUint(1) // serial
	wlwire.WriteMessage(cli, imID, 3 /*commit*/, cm.Bytes())

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		em.mu.Lock()
		n := len(em.commits)
		em.mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	em.mu.Lock()
	defer em.mu.Unlock()
	if len(em.commits) != 1 {
		t.Fatalf("commits recorded: %d, want 1", len(em.commits))
	}
	if em.commits[0].Path != ctxPath || em.commits[0].Text != "a" {
		t.Errorf("commit: %+v", em.commits[0])
	}
}

// TestBridgePreeditFlow — same shape as above but the downstream
// IME sends a preedit instead of a commit. Verify the emitter
// gets UpdatePreedit with the right text.
func TestBridgePreeditFlow(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "v2-preedit.sock")
	em := &fakeEmitter{}
	b := New(em, nil)
	if err := b.Listen(path); err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	cli := dialAndBindIM(t, path)
	defer cli.Close()
	b.NotifyFocusIn("/test/preedit")
	// Drain activate + done.
	cli.SetReadDeadline(time.Now().Add(time.Second))
	wlwire.ReadMessage(cli)
	wlwire.ReadMessage(cli)

	// set_preedit_string("ni", 2, 2) + commit(serial=1).
	const imID = 5
	pb := wlwire.NewEncoder()
	pb.PutString("ni")
	pb.PutInt(2)
	pb.PutInt(2)
	wlwire.WriteMessage(cli, imID, 1, pb.Bytes())
	cm := wlwire.NewEncoder()
	cm.PutUint(1)
	wlwire.WriteMessage(cli, imID, 3, cm.Bytes())

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		em.mu.Lock()
		n := len(em.preedits)
		em.mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	em.mu.Lock()
	defer em.mu.Unlock()
	if len(em.preedits) != 1 || em.preedits[0].Text != "ni" {
		t.Errorf("preedits: %+v", em.preedits)
	}
}

// TestBridgeNoGrabFallsThrough — when no downstream IME has
// grabbed, HasActiveGrab is false and RouteKey returns
// consumed=false. Models the "v2 enabled but nobody connected"
// state which must be a clean pass-through to the next tier.
func TestBridgeNoGrabFallsThrough(t *testing.T) {
	em := &fakeEmitter{}
	b := New(em, nil)

	if b.HasActiveGrab() {
		t.Error("HasActiveGrab true with no clients")
	}
	consumed, err := b.RouteKey("/x", 0x61, 38, 0)
	if err != nil {
		t.Fatal(err)
	}
	if consumed {
		t.Error("RouteKey claimed key with no grab")
	}
}

// --- helpers ---

func waitFor(cond func() bool) bool {
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

// dialAndBindIM is the common preamble: connect to the bridge's
// socket, bind the manager, create an input method, and grab the
// keyboard. Returns the client connection ready for per-IM
// traffic. The caller is responsible for draining the activate +
// done events that follow the first NotifyFocusIn.
func dialAndBindIM(t *testing.T, path string) *net.UnixConn {
	t.Helper()
	addr, _ := net.ResolveUnixAddr("unix", path)
	cli, err := net.DialUnix("unix", nil, addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}

	const registryID = uint32(2)
	const managerID = uint32(3)
	const seatID = uint32(4)
	const imID = uint32(5)
	const grabID = uint32(6)

	body := wlwire.NewEncoder()
	body.PutUint(registryID)
	wlwire.WriteMessage(cli, 1, 1, body.Bytes())

	cli.SetReadDeadline(time.Now().Add(time.Second))
	h, gbody, err := wlwire.ReadMessage(cli)
	if err != nil || h.ObjectID != registryID {
		t.Fatalf("global event: %+v err=%v", h, err)
	}
	dec := wlwire.NewDecoder(gbody)
	name := dec.Uint()
	iface := dec.String()
	version := dec.Uint()

	bb := wlwire.NewEncoder()
	bb.PutUint(name)
	bb.PutString(iface)
	bb.PutUint(version)
	bb.PutUint(managerID)
	wlwire.WriteMessage(cli, registryID, 0, bb.Bytes())

	gi := wlwire.NewEncoder()
	gi.PutUint(seatID)
	gi.PutUint(imID)
	wlwire.WriteMessage(cli, managerID, 0, gi.Bytes())

	gk := wlwire.NewEncoder()
	gk.PutUint(grabID)
	wlwire.WriteMessage(cli, imID, 5, gk.Bytes())

	return cli
}
