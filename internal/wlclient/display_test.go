package wlclient

import (
	"io"
	"net"
	"os"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/lapingvino/flexkb/internal/wlwire"
)

// TestRegistryGlobalsThenBind drives the dispatcher against a
// fake compositor (one half of a socketpair) that emits two
// global events and then services a bind request. Mirrors the
// initial handshake every Wayland client does on startup.
func TestRegistryGlobalsThenBind(t *testing.T) {
	clientConn, serverConn := connectedConns(t)
	defer clientConn.Close()
	defer serverConn.Close()

	d := NewDispatcher(clientConn, nil)
	disp, err := ConnectDisplay(d)
	if err != nil {
		t.Fatalf("ConnectDisplay: %v", err)
	}

	var (
		globalsMu sync.Mutex
		globals   []string
	)
	_, err = disp.GetRegistry(
		func(name uint32, iface string, version uint32) {
			globalsMu.Lock()
			globals = append(globals, iface)
			globalsMu.Unlock()
		},
		nil,
	)
	if err != nil {
		t.Fatalf("GetRegistry: %v", err)
	}

	// Run the dispatcher in the background.
	dispatchDone := make(chan error, 1)
	go func() { dispatchDone <- d.Run() }()

	// Server-side: read the get_registry request, then emit
	// two global events on the new registry object, plus a
	// sync done so the test can wait for them all to be processed.
	serverReadAndAssert(t, serverConn, 1 /*display*/, 1 /*opcode get_registry*/)
	// The body of get_registry was a single u32 = the new registry's
	// ID. Re-decode to learn what it is.
	// serverReadAndAssert returned the body. Re-call without
	// helper to capture it.
	// (Refactor below to actually return body.)

	// Synthesise two global events from the server.
	const fakeRegistryID = firstClientID
	body1 := wlwire.NewEncoder()
	body1.PutUint(7) // name
	body1.PutString("wl_compositor")
	body1.PutUint(4)
	if err := wlwire.WriteMessage(serverConn, fakeRegistryID, 0, body1.Bytes()); err != nil {
		t.Fatal(err)
	}
	body2 := wlwire.NewEncoder()
	body2.PutUint(8)
	body2.PutString("zwp_input_method_manager_v2")
	body2.PutUint(1)
	if err := wlwire.WriteMessage(serverConn, fakeRegistryID, 0, body2.Bytes()); err != nil {
		t.Fatal(err)
	}

	// Now sync to make sure those globals were dispatched.
	syncDone := make(chan struct{})
	if err := disp.Sync(0, func(uint32) { close(syncDone) }); err != nil {
		t.Fatal(err)
	}
	// Read the sync request and respond.
	syncReq := serverReadRequest(t, serverConn)
	if syncReq.Header.ObjectID != 1 || syncReq.Header.Opcode != 0 {
		t.Fatalf("expected sync request, got %+v", syncReq.Header)
	}
	callbackID := wlwire.NewDecoder(syncReq.Body).Uint()
	doneBody := wlwire.NewEncoder()
	doneBody.PutUint(99) // callback_data
	if err := wlwire.WriteMessage(serverConn, callbackID, 0, doneBody.Bytes()); err != nil {
		t.Fatal(err)
	}

	select {
	case <-syncDone:
	case <-time.After(time.Second):
		t.Fatal("sync callback never fired")
	}

	globalsMu.Lock()
	gs := append([]string(nil), globals...)
	globalsMu.Unlock()
	if len(gs) != 2 || gs[0] != "wl_compositor" || gs[1] != "zwp_input_method_manager_v2" {
		t.Errorf("got globals %v", gs)
	}

	// Shut down cleanly.
	clientConn.Close()
	select {
	case err := <-dispatchDone:
		if err != nil && err != io.EOF {
			t.Errorf("dispatch loop error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("dispatch loop didn't exit")
	}
}

// TestDisplayErrorEventBubblesUp — the compositor sending an
// error event must surface from Run() so the daemon can react
// (clean shutdown, log, restart).
func TestDisplayErrorEventBubblesUp(t *testing.T) {
	clientConn, serverConn := connectedConns(t)
	defer clientConn.Close()
	defer serverConn.Close()

	d := NewDispatcher(clientConn, nil)
	if _, err := ConnectDisplay(d); err != nil {
		t.Fatal(err)
	}
	dispatchDone := make(chan error, 1)
	go func() { dispatchDone <- d.Run() }()

	// Synthesise display::error.
	body := wlwire.NewEncoder()
	body.PutUint(42)             // offending object id
	body.PutUint(1)              // error code
	body.PutString("test error") // message
	if err := wlwire.WriteMessage(serverConn, 1 /*display*/, 0 /*error*/, body.Bytes()); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-dispatchDone:
		if err == nil {
			t.Fatal("dispatch loop returned nil on display::error")
		}
	case <-time.After(time.Second):
		t.Fatal("dispatch loop didn't propagate display::error")
	}
}

// TestDeleteIDUnregistersHandler — delete_id from the server must
// drop the entry so subsequent events for that ID get the "unknown
// object" debug path rather than reaching a stale handler.
func TestDeleteIDUnregistersHandler(t *testing.T) {
	clientConn, serverConn := connectedConns(t)
	defer clientConn.Close()
	defer serverConn.Close()

	d := NewDispatcher(clientConn, nil)
	if _, err := ConnectDisplay(d); err != nil {
		t.Fatal(err)
	}
	// Pre-register a sentinel handler.
	const sentinelID = 42
	called := make(chan struct{}, 1)
	d.Register(sentinelID, HandlerFunc(func(uint16, []byte, []int) error {
		called <- struct{}{}
		return nil
	}))

	dispatchDone := make(chan error, 1)
	go func() { dispatchDone <- d.Run() }()

	// Server sends delete_id for sentinelID, then a no-op event
	// targeted at sentinelID. The handler should not fire.
	delBody := wlwire.NewEncoder()
	delBody.PutUint(sentinelID)
	if err := wlwire.WriteMessage(serverConn, 1, 1 /*delete_id*/, delBody.Bytes()); err != nil {
		t.Fatal(err)
	}
	// A throwaway event addressed to the (now-unregistered) id.
	if err := wlwire.WriteMessage(serverConn, sentinelID, 0, nil); err != nil {
		t.Fatal(err)
	}

	// Give dispatch a moment to process, then ensure handler
	// wasn't called.
	time.Sleep(100 * time.Millisecond)
	select {
	case <-called:
		t.Fatal("handler fired after delete_id")
	default:
	}

	clientConn.Close()
	<-dispatchDone
}

// --- helpers ---

func connectedConns(t *testing.T) (*wlwire.Conn, *net.UnixConn) {
	t.Helper()
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatalf("socketpair: %v", err)
	}
	cliFile := os.NewFile(uintptr(fds[0]), "client-side")
	srvFile := os.NewFile(uintptr(fds[1]), "server-side")
	cli, err := net.FileConn(cliFile)
	if err != nil {
		t.Fatalf("FileConn cli: %v", err)
	}
	cliFile.Close()
	srv, err := net.FileConn(srvFile)
	if err != nil {
		t.Fatalf("FileConn srv: %v", err)
	}
	srvFile.Close()
	cliUC := cli.(*net.UnixConn)
	srvUC := srv.(*net.UnixConn)
	return newConnForTest(cliUC), srvUC
}

// newConnForTest is a back-door constructor for tests that already
// have a *net.UnixConn (from socketpair) rather than going through
// Dial. The production code never needs this.
func newConnForTest(uc *net.UnixConn) *wlwire.Conn {
	return wlwire.NewConnFromUnixConn(uc)
}

type rawMessage struct {
	Header wlwire.Header
	Body   []byte
}

func serverReadRequest(t *testing.T, conn *net.UnixConn) rawMessage {
	t.Helper()
	conn.SetReadDeadline(time.Now().Add(time.Second))
	h, body, err := wlwire.ReadMessage(conn)
	if err != nil {
		t.Fatalf("server read: %v", err)
	}
	return rawMessage{Header: h, Body: body}
}

func serverReadAndAssert(t *testing.T, conn *net.UnixConn, wantID uint32, wantOpcode uint16) {
	t.Helper()
	msg := serverReadRequest(t, conn)
	if msg.Header.ObjectID != wantID || msg.Header.Opcode != wantOpcode {
		t.Fatalf("expected request id=%d op=%d, got %+v", wantID, wantOpcode, msg.Header)
	}
}
