package wlserver

import (
	"net"
	"os"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/lapingvino/flexkb/internal/wlwire"
)

// TestRegistryAdvertisesGlobalsOnGetRegistry drives the server
// against a fake client (one half of a socketpair). The client
// sends get_registry, the server advertises two globals; the
// client sends bind; the server invokes the registered onBind.
func TestRegistryAdvertisesGlobalsOnGetRegistry(t *testing.T) {
	srvConn, cli := connectedConns(t)
	defer cli.Close()

	d := NewDispatcher(srvConn, nil)
	reg := NewRegistry(d)
	if _, err := RegisterDisplay(d, reg); err != nil {
		t.Fatalf("RegisterDisplay: %v", err)
	}

	var (
		bindMu     sync.Mutex
		bindCalls  []string
		bindNewIDs []uint32
	)
	reg.AddGlobal("zwp_input_method_manager_v2", 1, func(_ *Dispatcher, newID, version uint32) error {
		bindMu.Lock()
		bindCalls = append(bindCalls, "zwp_input_method_manager_v2")
		bindNewIDs = append(bindNewIDs, newID)
		bindMu.Unlock()
		return nil
	})

	dispatchDone := make(chan error, 1)
	go func() { dispatchDone <- d.Run() }()

	// Client side: send get_registry.
	const registryID uint32 = 2 // first client-allocated ID after wl_display
	body := wlwire.NewEncoder()
	body.PutUint(registryID)
	if err := wlwire.WriteMessage(cli, 1 /*wl_display*/, 1 /*get_registry*/, body.Bytes()); err != nil {
		t.Fatal(err)
	}

	// Server should now emit a global event for the IM manager.
	// Read it.
	cli.SetReadDeadline(time.Now().Add(time.Second))
	h, gbody, err := wlwire.ReadMessage(cli)
	if err != nil {
		t.Fatalf("client read global: %v", err)
	}
	if h.ObjectID != registryID || h.Opcode != 0 {
		t.Fatalf("expected global event on registry, got id=%d op=%d", h.ObjectID, h.Opcode)
	}
	gd := wlwire.NewDecoder(gbody)
	name := gd.Uint()
	iface := gd.String()
	version := gd.Uint()
	if iface != "zwp_input_method_manager_v2" {
		t.Errorf("global iface: %q", iface)
	}
	if version != 1 {
		t.Errorf("global version: %d", version)
	}

	// Client binds it.
	const boundID uint32 = 3
	bBody := wlwire.NewEncoder()
	bBody.PutUint(name)
	bBody.PutString(iface)
	bBody.PutUint(version)
	bBody.PutUint(boundID)
	if err := wlwire.WriteMessage(cli, registryID, 0 /*bind*/, bBody.Bytes()); err != nil {
		t.Fatal(err)
	}

	// Give the dispatcher a beat to process the bind.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		bindMu.Lock()
		got := len(bindCalls)
		bindMu.Unlock()
		if got > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	bindMu.Lock()
	defer bindMu.Unlock()
	if len(bindCalls) != 1 {
		t.Fatalf("bind handler called %d times, want 1", len(bindCalls))
	}
	if bindNewIDs[0] != boundID {
		t.Errorf("bind newID: got %d, want %d", bindNewIDs[0], boundID)
	}

	cli.Close()
	<-dispatchDone
}

// TestSyncEmitsDoneThenDeleteID — the standard round-trip every
// Wayland client uses to wait for prior requests. Server should
// reply with wl_callback::done on the callback id, then
// wl_display::delete_id for the same id.
func TestSyncEmitsDoneThenDeleteID(t *testing.T) {
	srvConn, cli := connectedConns(t)
	defer cli.Close()

	d := NewDispatcher(srvConn, nil)
	reg := NewRegistry(d)
	if _, err := RegisterDisplay(d, reg); err != nil {
		t.Fatal(err)
	}
	dispatchDone := make(chan error, 1)
	go func() { dispatchDone <- d.Run() }()

	const callbackID uint32 = 7
	body := wlwire.NewEncoder()
	body.PutUint(callbackID)
	if err := wlwire.WriteMessage(cli, 1 /*display*/, 0 /*sync*/, body.Bytes()); err != nil {
		t.Fatal(err)
	}

	// Read done.
	cli.SetReadDeadline(time.Now().Add(time.Second))
	h, _, err := wlwire.ReadMessage(cli)
	if err != nil {
		t.Fatalf("client read done: %v", err)
	}
	if h.ObjectID != callbackID || h.Opcode != 0 {
		t.Errorf("done frame: id=%d op=%d (want id=%d op=0)", h.ObjectID, h.Opcode, callbackID)
	}

	// Read delete_id.
	h2, body2, err := wlwire.ReadMessage(cli)
	if err != nil {
		t.Fatalf("client read delete_id: %v", err)
	}
	if h2.ObjectID != 1 || h2.Opcode != 1 {
		t.Errorf("delete_id frame: id=%d op=%d (want id=1 op=1)", h2.ObjectID, h2.Opcode)
	}
	if got := wlwire.NewDecoder(body2).Uint(); got != callbackID {
		t.Errorf("delete_id payload: %d, want %d", got, callbackID)
	}

	cli.Close()
	<-dispatchDone
}

// TestListenerAcceptsConnection — end-to-end with a real Unix
// socket on disk. Verifies the listener accepts, onAccept runs,
// and a client dial round-trips a get_registry.
func TestListenerAcceptsConnection(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/wlserver-test.sock"

	l, err := Listen(path, nil, func(d *Dispatcher) error {
		reg := NewRegistry(d)
		if _, err := RegisterDisplay(d, reg); err != nil {
			return err
		}
		reg.AddGlobal("zwp_input_method_manager_v2", 1,
			func(*Dispatcher, uint32, uint32) error { return nil })
		return nil
	})
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer l.Close()
	go l.Serve()

	addr, _ := net.ResolveUnixAddr("unix", path)
	uc, err := net.DialUnix("unix", nil, addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer uc.Close()

	// Send get_registry.
	const registryID uint32 = 2
	body := wlwire.NewEncoder()
	body.PutUint(registryID)
	if err := wlwire.WriteMessage(uc, 1, 1, body.Bytes()); err != nil {
		t.Fatal(err)
	}
	uc.SetReadDeadline(time.Now().Add(time.Second))
	h, _, err := wlwire.ReadMessage(uc)
	if err != nil {
		t.Fatalf("read global: %v", err)
	}
	if h.ObjectID != registryID || h.Opcode != 0 {
		t.Errorf("global frame: id=%d op=%d", h.ObjectID, h.Opcode)
	}
}

// TestListenerRefusesNonSocketPath — a regular file at the chosen
// path must not be silently unlinked.
func TestListenerRefusesNonSocketPath(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/regular-file"
	if err := os.WriteFile(path, []byte("hi"), 0644); err != nil {
		t.Fatal(err)
	}
	_, err := Listen(path, nil, func(*Dispatcher) error { return nil })
	if err == nil {
		t.Fatal("Listen succeeded against a regular file; should have refused")
	}
}

// --- helpers ---

// connectedConns returns a (wire-wrapped server side, raw client
// side) socket pair. Same idiom as wlclient's tests but with the
// roles swapped — the WIRE-wrapped end is the side the Dispatcher
// owns, and the raw end is what the fake client drives.
func connectedConns(t *testing.T) (*wlwire.Conn, *net.UnixConn) {
	t.Helper()
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatalf("socketpair: %v", err)
	}
	srvFile := os.NewFile(uintptr(fds[0]), "server-side")
	cliFile := os.NewFile(uintptr(fds[1]), "client-side")
	srv, err := net.FileConn(srvFile)
	if err != nil {
		t.Fatal(err)
	}
	srvFile.Close()
	cli, err := net.FileConn(cliFile)
	if err != nil {
		t.Fatal(err)
	}
	cliFile.Close()
	return wlwire.NewConnFromUnixConn(srv.(*net.UnixConn)), cli.(*net.UnixConn)
}
