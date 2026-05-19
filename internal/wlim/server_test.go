package wlim

import (
	"net"
	"os"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/lapingvino/flexkb/internal/wlserver"
	"github.com/lapingvino/flexkb/internal/wlwire"
)

// TestServerManagerCreatesInputMethod — the most important end-to-
// end check: a downstream client binds the manager, sends
// get_input_method, and the server registers it via the callback.
func TestServerManagerCreatesInputMethod(t *testing.T) {
	srv, cli := serverConns(t)
	defer cli.Close()

	d := wlserver.NewDispatcher(srv, nil)
	reg := wlserver.NewRegistry(d)
	if _, err := wlserver.RegisterDisplay(d, reg); err != nil {
		t.Fatal(err)
	}

	var (
		createdMu sync.Mutex
		created   []*ServerInputMethod
	)
	AddManagerGlobal(reg, nil, &testManagerCallback{
		onCreated: func(im *ServerInputMethod) {
			createdMu.Lock()
			created = append(created, im)
			createdMu.Unlock()
		},
	})

	go d.Run()

	// Client: get_registry, bind manager, get_input_method.
	const registryID uint32 = 2
	const managerID uint32 = 3
	const seatID uint32 = 4
	const imID uint32 = 5

	bGetReg := wlwire.NewEncoder()
	bGetReg.PutUint(registryID)
	if err := wlwire.WriteMessage(cli, 1, 1, bGetReg.Bytes()); err != nil {
		t.Fatal(err)
	}

	// Read the global event the server emits.
	cli.SetReadDeadline(time.Now().Add(time.Second))
	h, body, err := wlwire.ReadMessage(cli)
	if err != nil {
		t.Fatal(err)
	}
	if h.ObjectID != registryID {
		t.Fatalf("global event id: got %d, want %d", h.ObjectID, registryID)
	}
	dec := wlwire.NewDecoder(body)
	name := dec.Uint()
	iface := dec.String()
	version := dec.Uint()
	if iface != InterfaceManager {
		t.Fatalf("global iface: %q", iface)
	}

	// Bind the manager.
	bBind := wlwire.NewEncoder()
	bBind.PutUint(name)
	bBind.PutString(iface)
	bBind.PutUint(version)
	bBind.PutUint(managerID)
	if err := wlwire.WriteMessage(cli, registryID, 0, bBind.Bytes()); err != nil {
		t.Fatal(err)
	}

	// get_input_method(seat, im_id).
	bGetIM := wlwire.NewEncoder()
	bGetIM.PutUint(seatID)
	bGetIM.PutUint(imID)
	if err := wlwire.WriteMessage(cli, managerID, 0, bGetIM.Bytes()); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		createdMu.Lock()
		n := len(created)
		createdMu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	createdMu.Lock()
	defer createdMu.Unlock()
	if len(created) != 1 {
		t.Fatalf("created %d input methods, want 1", len(created))
	}
	if created[0].ID() != imID {
		t.Errorf("im id: got %d, want %d", created[0].ID(), imID)
	}
}

// TestServerInputMethodCommitsFlowToCallback — once an IM is
// created, the downstream client sends commit_string and
// set_preedit_string; the bridge-style callback receives them.
func TestServerInputMethodCommitsFlowToCallback(t *testing.T) {
	srv, cli, im := bringUpIM(t)
	defer cli.Close()
	_ = srv

	var (
		mu       sync.Mutex
		commits  []string
		preedits []string
		commitN  uint32
	)
	im.OnCommitString = func(text string) {
		mu.Lock()
		commits = append(commits, text)
		mu.Unlock()
	}
	im.OnSetPreeditString = func(text string, _, _ int32) {
		mu.Lock()
		preedits = append(preedits, text)
		mu.Unlock()
	}
	im.OnCommit = func(serial uint32) {
		mu.Lock()
		commitN = serial
		mu.Unlock()
	}

	// commit_string("你")
	b := wlwire.NewEncoder()
	b.PutString("你")
	wlwire.WriteMessage(cli, im.ID(), 0, b.Bytes())
	// set_preedit_string("h", 1, 1)
	b = wlwire.NewEncoder()
	b.PutString("h")
	b.PutInt(1)
	b.PutInt(1)
	wlwire.WriteMessage(cli, im.ID(), 1, b.Bytes())
	// commit(serial=42)
	b = wlwire.NewEncoder()
	b.PutUint(42)
	wlwire.WriteMessage(cli, im.ID(), 3, b.Bytes())

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		got := commitN
		mu.Unlock()
		if got == 42 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(commits) != 1 || commits[0] != "你" {
		t.Errorf("commits: %v", commits)
	}
	if len(preedits) != 1 || preedits[0] != "h" {
		t.Errorf("preedits: %v", preedits)
	}
	if commitN != 42 {
		t.Errorf("commit serial: %d", commitN)
	}
}

// TestServerInputMethodGrabKeyboardThenSendKey — the downstream
// IME grabs, the bridge sends a key event, the IME receives it.
func TestServerInputMethodGrabKeyboardThenSendKey(t *testing.T) {
	_, cli, im := bringUpIM(t)
	defer cli.Close()

	grabReady := make(chan *ServerKeyboardGrab, 1)
	im.OnGrabKeyboard = func(g *ServerKeyboardGrab) { grabReady <- g }

	const grabID uint32 = 100
	b := wlwire.NewEncoder()
	b.PutUint(grabID)
	wlwire.WriteMessage(cli, im.ID(), 5, b.Bytes())

	var grab *ServerKeyboardGrab
	select {
	case grab = <-grabReady:
	case <-time.After(time.Second):
		t.Fatal("OnGrabKeyboard never fired")
	}

	if grab.Released() {
		t.Fatal("grab released before any traffic")
	}

	// Bridge side: send a key event.
	if err := grab.SendKey(1, 1234, 30 /*KEY_A*/, 1 /*pressed*/); err != nil {
		t.Fatal(err)
	}

	// Client side: read it back.
	cli.SetReadDeadline(time.Now().Add(time.Second))
	h, body, err := wlwire.ReadMessage(cli)
	if err != nil {
		t.Fatal(err)
	}
	if h.ObjectID != grabID || h.Opcode != 1 {
		t.Fatalf("key event header: %+v", h)
	}
	dec := wlwire.NewDecoder(body)
	serial := dec.Uint()
	tval := dec.Uint()
	key := dec.Uint()
	state := dec.Uint()
	if serial != 1 || tval != 1234 || key != 30 || state != 1 {
		t.Errorf("key body: serial=%d time=%d key=%d state=%d", serial, tval, key, state)
	}

	// Client releases the grab.
	wlwire.WriteMessage(cli, grabID, 0, nil)
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if grab.Released() {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !grab.Released() {
		t.Error("grab not released after release request")
	}
}

// TestServerSendActivateRoundTrip — the bridge sends activate +
// done; the wire bytes look right.
func TestServerSendActivateRoundTrip(t *testing.T) {
	_, cli, im := bringUpIM(t)
	defer cli.Close()

	if err := im.SendActivate(); err != nil {
		t.Fatal(err)
	}
	if err := im.SendDone(); err != nil {
		t.Fatal(err)
	}

	cli.SetReadDeadline(time.Now().Add(time.Second))
	h, _, err := wlwire.ReadMessage(cli)
	if err != nil || h.ObjectID != im.ID() || h.Opcode != 0 {
		t.Fatalf("activate frame: %+v err=%v", h, err)
	}
	h, _, err = wlwire.ReadMessage(cli)
	if err != nil || h.ObjectID != im.ID() || h.Opcode != 5 {
		t.Fatalf("done frame: %+v err=%v", h, err)
	}
}

// TestServerSecondInputMethodMakesPriorUnavailable — protocol rule:
// at most one input_method per seat.
func TestServerSecondInputMethodMakesPriorUnavailable(t *testing.T) {
	srv, cli := serverConns(t)
	defer cli.Close()

	d := wlserver.NewDispatcher(srv, nil)
	reg := wlserver.NewRegistry(d)
	wlserver.RegisterDisplay(d, reg)

	created := make(chan *ServerInputMethod, 4)
	destroyed := make(chan *ServerInputMethod, 4)
	AddManagerGlobal(reg, nil, &testManagerCallback{
		onCreated:   func(im *ServerInputMethod) { created <- im },
		onDestroyed: func(im *ServerInputMethod) { destroyed <- im },
	})
	go d.Run()

	// Skip ahead: bind, then ask for two input_methods.
	bindManager(t, cli, 2 /*registry*/, 3 /*manager*/)
	const seatID = 4
	const imA = 5
	const imB = 6

	getIM := func(id uint32) {
		b := wlwire.NewEncoder()
		b.PutUint(seatID)
		b.PutUint(id)
		wlwire.WriteMessage(cli, 3, 0, b.Bytes())
	}
	getIM(imA)
	getIM(imB)

	imAObj := <-created
	imBObj := <-created
	if imAObj.ID() != imA || imBObj.ID() != imB {
		t.Errorf("ids: %d / %d", imAObj.ID(), imBObj.ID())
	}
	// imA should have been destroyed when imB was created.
	select {
	case dropped := <-destroyed:
		if dropped.ID() != imA {
			t.Errorf("destroyed id %d, want %d", dropped.ID(), imA)
		}
	case <-time.After(time.Second):
		t.Fatal("imA was not torn down on imB creation")
	}
	// imA should have received the unavailable event.
	cli.SetReadDeadline(time.Now().Add(time.Second))
	// First message after the global emit was the unavailable on imA.
	// Drain frames until we see (imA, opcode 6).
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		h, _, err := wlwire.ReadMessage(cli)
		if err != nil {
			t.Fatal(err)
		}
		if h.ObjectID == imA && h.Opcode == 6 {
			return
		}
	}
	t.Fatal("never saw unavailable on imA")
}

// --- helpers ---

func serverConns(t *testing.T) (*wlwire.Conn, *net.UnixConn) {
	t.Helper()
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	srvFile := os.NewFile(uintptr(fds[0]), "srv")
	cliFile := os.NewFile(uintptr(fds[1]), "cli")
	srv, _ := net.FileConn(srvFile)
	srvFile.Close()
	cli, _ := net.FileConn(cliFile)
	cliFile.Close()
	return wlwire.NewConnFromUnixConn(srv.(*net.UnixConn)), cli.(*net.UnixConn)
}

// bringUpIM stands up a dispatcher with the manager global,
// performs the (get_registry → bind → get_input_method) sequence
// from the client side, and returns the resulting ServerInputMethod
// once the callback fires. Used by every test that exercises
// per-IM behaviour.
func bringUpIM(t *testing.T) (*wlwire.Conn, *net.UnixConn, *ServerInputMethod) {
	t.Helper()
	srv, cli := serverConns(t)

	d := wlserver.NewDispatcher(srv, nil)
	reg := wlserver.NewRegistry(d)
	wlserver.RegisterDisplay(d, reg)

	created := make(chan *ServerInputMethod, 1)
	AddManagerGlobal(reg, nil, &testManagerCallback{
		onCreated: func(im *ServerInputMethod) { created <- im },
	})
	go d.Run()

	bindManager(t, cli, 2, 3)
	const seatID = 4
	const imID = 5
	b := wlwire.NewEncoder()
	b.PutUint(seatID)
	b.PutUint(imID)
	wlwire.WriteMessage(cli, 3, 0, b.Bytes())

	select {
	case im := <-created:
		return srv, cli, im
	case <-time.After(time.Second):
		t.Fatal("input method never created")
		return nil, nil, nil
	}
}

// bindManager runs the get_registry + bind pair the test client
// needs before it can call get_input_method. Drains the global
// event so subsequent reads start fresh.
func bindManager(t *testing.T, cli *net.UnixConn, registryID, managerID uint32) {
	t.Helper()
	b := wlwire.NewEncoder()
	b.PutUint(registryID)
	wlwire.WriteMessage(cli, 1, 1, b.Bytes())

	cli.SetReadDeadline(time.Now().Add(time.Second))
	h, body, err := wlwire.ReadMessage(cli)
	if err != nil || h.ObjectID != registryID {
		t.Fatalf("global event read: %+v err=%v", h, err)
	}
	dec := wlwire.NewDecoder(body)
	name := dec.Uint()
	iface := dec.String()
	version := dec.Uint()
	if iface != InterfaceManager {
		t.Fatalf("global iface: %q", iface)
	}

	bb := wlwire.NewEncoder()
	bb.PutUint(name)
	bb.PutString(iface)
	bb.PutUint(version)
	bb.PutUint(managerID)
	wlwire.WriteMessage(cli, registryID, 0, bb.Bytes())
}

type testManagerCallback struct {
	onCreated   func(*ServerInputMethod)
	onDestroyed func(*ServerInputMethod)
}

func (c *testManagerCallback) OnInputMethodCreated(im *ServerInputMethod) {
	if c.onCreated != nil {
		c.onCreated(im)
	}
}
func (c *testManagerCallback) OnInputMethodDestroyed(im *ServerInputMethod) {
	if c.onDestroyed != nil {
		c.onDestroyed(im)
	}
}
