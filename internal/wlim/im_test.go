package wlim

import (
	"io"
	"net"
	"os"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/lapingvino/flexkb/internal/wlclient"
	"github.com/lapingvino/flexkb/internal/wlwire"
)

// TestInputMethodEventDispatch — emit all seven IM events from a
// fake compositor and assert each lands in its callback with the
// expected arguments. Probes the body decoding and the opcode
// routing together.
func TestInputMethodEventDispatch(t *testing.T) {
	clientConn, serverConn := pair(t)
	defer clientConn.Close()
	defer serverConn.Close()

	d := wlclient.NewDispatcher(clientConn, nil)
	go d.Run()

	im := NewInputMethod(nil)
	im.d = d
	const imID = 5
	im.id = imID
	d.Register(imID, im)

	var (
		mu               sync.Mutex
		gotActivate      bool
		gotDeactivate    bool
		gotDone          bool
		gotUnavail       bool
		gotSurr          string
		gotSurrCursor    uint32
		gotCause         uint32
		gotHint, gotPurp uint32
	)
	im.OnActivate = func() { mu.Lock(); gotActivate = true; mu.Unlock() }
	im.OnDeactivate = func() { mu.Lock(); gotDeactivate = true; mu.Unlock() }
	im.OnSurroundingText = func(text string, cursor, anchor uint32) {
		mu.Lock()
		gotSurr = text
		gotSurrCursor = cursor
		mu.Unlock()
	}
	im.OnTextChangeCause = func(cause uint32) { mu.Lock(); gotCause = cause; mu.Unlock() }
	im.OnContentType = func(hint, purpose uint32) {
		mu.Lock()
		gotHint, gotPurp = hint, purpose
		mu.Unlock()
	}
	im.OnDone = func() { mu.Lock(); gotDone = true; mu.Unlock() }
	im.OnUnavailable = func() { mu.Lock(); gotUnavail = true; mu.Unlock() }

	// Emit each event from the server side.
	emit := func(opcode uint16, body []byte) {
		if err := wlwire.WriteMessage(serverConn, imID, opcode, body); err != nil {
			t.Fatal(err)
		}
	}
	emit(0, nil) // activate
	st := wlwire.NewEncoder()
	st.PutString("hello world")
	st.PutUint(5) // cursor
	st.PutUint(5) // anchor
	emit(2, st.Bytes())
	cc := wlwire.NewEncoder()
	cc.PutUint(1)
	emit(3, cc.Bytes())
	ct := wlwire.NewEncoder()
	ct.PutUint(7) // hint
	ct.PutUint(3) // purpose
	emit(4, ct.Bytes())
	emit(5, nil) // done
	emit(6, nil) // unavailable
	emit(1, nil) // deactivate

	// Use a sync via a separate dispatcher object to flush.
	time.Sleep(80 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if !gotActivate || !gotDeactivate || !gotDone || !gotUnavail {
		t.Errorf("flags: activate=%v deactivate=%v done=%v unavail=%v",
			gotActivate, gotDeactivate, gotDone, gotUnavail)
	}
	if gotSurr != "hello world" || gotSurrCursor != 5 {
		t.Errorf("surrounding_text: text=%q cursor=%d", gotSurr, gotSurrCursor)
	}
	if gotCause != 1 {
		t.Errorf("cause: got %d, want 1", gotCause)
	}
	if gotHint != 7 || gotPurp != 3 {
		t.Errorf("content_type: hint=%d purpose=%d", gotHint, gotPurp)
	}
}

// TestInputMethodRequestsOnTheWire — exercise commit_string,
// set_preedit_string, delete_surrounding_text, and commit. Each
// must produce the bytes the wlroots protocol XML defines.
func TestInputMethodRequestsOnTheWire(t *testing.T) {
	clientConn, serverConn := pair(t)
	defer clientConn.Close()
	defer serverConn.Close()

	d := wlclient.NewDispatcher(clientConn, nil)
	go d.Run()

	im := NewInputMethod(nil)
	im.d = d
	const imID = 5
	im.id = imID

	// commit_string("ω")
	if err := im.CommitString("ω"); err != nil {
		t.Fatal(err)
	}
	h, body, _, err := readMsg(serverConn)
	if err != nil {
		t.Fatal(err)
	}
	if h.ObjectID != imID || h.Opcode != 0 {
		t.Errorf("commit_string header: %+v", h)
	}
	if wlwire.NewDecoder(body).String() != "ω" {
		t.Errorf("commit_string body decode wrong")
	}

	// set_preedit_string("ni", 2, 2)
	if err := im.SetPreeditString("ni", 2, 2); err != nil {
		t.Fatal(err)
	}
	h, body, _, _ = readMsg(serverConn)
	if h.Opcode != 1 {
		t.Errorf("set_preedit_string opcode: got %d", h.Opcode)
	}
	dec := wlwire.NewDecoder(body)
	if dec.String() != "ni" || dec.Int() != 2 || dec.Int() != 2 {
		t.Errorf("set_preedit body decode wrong")
	}

	// delete_surrounding_text(3, 0)
	if err := im.DeleteSurroundingText(3, 0); err != nil {
		t.Fatal(err)
	}
	h, body, _, _ = readMsg(serverConn)
	if h.Opcode != 2 {
		t.Errorf("delete_surrounding_text opcode: got %d", h.Opcode)
	}
	dec = wlwire.NewDecoder(body)
	if dec.Uint() != 3 || dec.Uint() != 0 {
		t.Errorf("delete_surrounding body decode wrong")
	}

	// commit(serial=42)
	if err := im.Commit(42); err != nil {
		t.Fatal(err)
	}
	h, body, _, _ = readMsg(serverConn)
	if h.Opcode != 3 {
		t.Errorf("commit opcode: got %d", h.Opcode)
	}
	if wlwire.NewDecoder(body).Uint() != 42 {
		t.Errorf("commit serial decode wrong")
	}
}

// TestKeyboardGrabKeyEvents — exercise the key/modifiers events,
// the most important grab events for the daemon's hot path. The
// keymap event with fd-passing is exercised in a separate test
// because it requires SCM_RIGHTS.
func TestKeyboardGrabKeyEvents(t *testing.T) {
	clientConn, serverConn := pair(t)
	defer clientConn.Close()
	defer serverConn.Close()

	d := wlclient.NewDispatcher(clientConn, nil)
	go d.Run()

	grab := &KeyboardGrab{d: d}
	const grabID = 7
	grab.id = grabID
	d.Register(grabID, grab)

	var (
		mu       sync.Mutex
		keys     [][4]uint32 // serial, time, key, state
		mods     [][5]uint32
		repeats  [][2]int32
	)
	grab.OnKey = func(serial, time, key, state uint32) {
		mu.Lock()
		keys = append(keys, [4]uint32{serial, time, key, state})
		mu.Unlock()
	}
	grab.OnModifiers = func(serial, dep, lat, lock, grp uint32) {
		mu.Lock()
		mods = append(mods, [5]uint32{serial, dep, lat, lock, grp})
		mu.Unlock()
	}
	grab.OnRepeatInfo = func(rate, delay int32) {
		mu.Lock()
		repeats = append(repeats, [2]int32{rate, delay})
		mu.Unlock()
	}

	// repeat_info(25, 600)
	b := wlwire.NewEncoder()
	b.PutInt(25)
	b.PutInt(600)
	wlwire.WriteMessage(serverConn, grabID, 3, b.Bytes())

	// modifiers(serial=1, dep=1, lat=0, lock=0, grp=0)  — Shift down
	b = wlwire.NewEncoder()
	b.PutUint(1)
	b.PutUint(1)
	b.PutUint(0)
	b.PutUint(0)
	b.PutUint(0)
	wlwire.WriteMessage(serverConn, grabID, 2, b.Bytes())

	// key(serial=2, time=12345, key=30 (Linux AC01='a'), state=1 pressed)
	b = wlwire.NewEncoder()
	b.PutUint(2)
	b.PutUint(12345)
	b.PutUint(30)
	b.PutUint(1)
	wlwire.WriteMessage(serverConn, grabID, 1, b.Bytes())

	time.Sleep(80 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if len(keys) != 1 || keys[0] != [4]uint32{2, 12345, 30, 1} {
		t.Errorf("keys: got %v", keys)
	}
	if len(mods) != 1 || mods[0] != [5]uint32{1, 1, 0, 0, 0} {
		t.Errorf("mods: got %v", mods)
	}
	if len(repeats) != 1 || repeats[0] != [2]int32{25, 600} {
		t.Errorf("repeats: got %v", repeats)
	}
}

// --- helpers ---

func pair(t *testing.T) (*wlwire.Conn, *net.UnixConn) {
	t.Helper()
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	cliFile := os.NewFile(uintptr(fds[0]), "cli")
	srvFile := os.NewFile(uintptr(fds[1]), "srv")
	cli, err := net.FileConn(cliFile)
	if err != nil {
		t.Fatal(err)
	}
	cliFile.Close()
	srv, err := net.FileConn(srvFile)
	if err != nil {
		t.Fatal(err)
	}
	srvFile.Close()
	return wlwire.NewConnFromUnixConn(cli.(*net.UnixConn)), srv.(*net.UnixConn)
}

func readMsg(conn *net.UnixConn) (wlwire.Header, []byte, []int, error) {
	conn.SetReadDeadline(time.Now().Add(time.Second))
	h, body, err := wlwire.ReadMessage(conn)
	if err != nil {
		if err == io.EOF {
			return h, body, nil, err
		}
		return h, body, nil, err
	}
	return h, body, nil, nil
}
