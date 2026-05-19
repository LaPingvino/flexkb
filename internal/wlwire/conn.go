package wlwire

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"syscall"
)

// Conn is a connected Wayland session — a Unix-domain stream
// socket plus the kernel-level plumbing for passing file
// descriptors via SCM_RIGHTS. Higher-level packages (registry,
// input-method) use it for both directions of message traffic.
//
// Conn is safe for concurrent use by one reader and one writer.
// Multiple writers must serialise externally (the daemon will
// own one writer goroutine).
type Conn struct {
	sock *net.UnixConn
	// rxBuf holds the partial last message read so successive
	// ReadMessage calls don't fragment on socket boundaries.
	// (Unix datagrams would be cleaner, but compositors expose
	// stream sockets per spec.)
	rxBuf   []byte
	rxFDs   []int
	writeMu sync.Mutex
}

// Dial connects to the compositor's Wayland socket. The display
// name resolution matches libwayland-client:
//
//  1. $WAYLAND_DISPLAY (absolute path used as-is; otherwise
//     resolved relative to $XDG_RUNTIME_DIR)
//  2. fallback "wayland-0" in $XDG_RUNTIME_DIR
//
// Returns an error if $XDG_RUNTIME_DIR isn't set (no compositor
// reachable that way) or the socket isn't listening.
func Dial() (*Conn, error) {
	path, err := socketPath()
	if err != nil {
		return nil, err
	}
	addr, err := net.ResolveUnixAddr("unix", path)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", path, err)
	}
	sock, err := net.DialUnix("unix", nil, addr)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", path, err)
	}
	return &Conn{sock: sock}, nil
}

// Close shuts down the connection.
func (c *Conn) Close() error { return c.sock.Close() }

// NewConnFromUnixConn wraps an already-connected *net.UnixConn as
// a Conn. Exposed for tests that want to drive both ends of a
// socketpair without going through Dial; production code uses Dial.
func NewConnFromUnixConn(uc *net.UnixConn) *Conn {
	return &Conn{sock: uc}
}

// SocketPath returns the socket path the connection was opened
// against. Useful for error messages and debug logs.
func SocketPath() (string, error) { return socketPath() }

func socketPath() (string, error) {
	display := os.Getenv("WAYLAND_DISPLAY")
	if display == "" {
		display = "wayland-0"
	}
	if filepath.IsAbs(display) {
		return display, nil
	}
	runtimeDir := os.Getenv("XDG_RUNTIME_DIR")
	if runtimeDir == "" {
		return "", errors.New("wlwire: XDG_RUNTIME_DIR not set; cannot locate Wayland socket")
	}
	return filepath.Join(runtimeDir, display), nil
}

// ReadMessage reads one complete message from the connection,
// returning the header, body, and any file descriptors received
// via SCM_RIGHTS as part of THIS message (or the most recent
// ancillary block — Wayland associates received fds with the
// next message that consumes them).
//
// Callers MUST close the returned fds when done; the Conn does
// not track ownership.
func (c *Conn) ReadMessage() (Header, []byte, []int, error) {
	// Pull bytes (and any incoming fds) until we have at least
	// HeaderSize buffered.
	for len(c.rxBuf) < HeaderSize {
		if err := c.fillRx(); err != nil {
			return Header{}, nil, nil, err
		}
	}
	h := DecodeHeader(c.rxBuf[:HeaderSize])
	if h.Size < HeaderSize {
		return Header{}, nil, nil, fmt.Errorf("wlwire: invalid message size %d", h.Size)
	}
	for len(c.rxBuf) < int(h.Size) {
		if err := c.fillRx(); err != nil {
			return Header{}, nil, nil, err
		}
	}
	body := make([]byte, int(h.Size)-HeaderSize)
	copy(body, c.rxBuf[HeaderSize:h.Size])
	c.rxBuf = c.rxBuf[h.Size:]
	fds := c.rxFDs
	c.rxFDs = nil
	return h, body, fds, nil
}

// fillRx pulls a chunk of bytes and any accompanying fds from
// the socket into the read buffer. One ReadMsgUnix call per
// invocation; ReadMessage loops until it has enough.
func (c *Conn) fillRx() error {
	const chunkSize = 4096
	const oobSize = 256 // room for ~64 fds per recv; more than enough
	chunk := make([]byte, chunkSize)
	oob := make([]byte, oobSize)
	n, oobn, _, _, err := c.sock.ReadMsgUnix(chunk, oob)
	if err != nil {
		return err
	}
	c.rxBuf = append(c.rxBuf, chunk[:n]...)
	if oobn > 0 {
		fds, err := parseSCMRights(oob[:oobn])
		if err != nil {
			return err
		}
		c.rxFDs = append(c.rxFDs, fds...)
	}
	return nil
}

// parseSCMRights walks the ancillary data block and pulls out
// every file descriptor delivered via SCM_RIGHTS.
func parseSCMRights(oob []byte) ([]int, error) {
	msgs, err := syscall.ParseSocketControlMessage(oob)
	if err != nil {
		return nil, fmt.Errorf("parse cmsg: %w", err)
	}
	var out []int
	for _, m := range msgs {
		if m.Header.Level != syscall.SOL_SOCKET || m.Header.Type != syscall.SCM_RIGHTS {
			continue
		}
		fds, err := syscall.ParseUnixRights(&m)
		if err != nil {
			return nil, fmt.Errorf("parse unix rights: %w", err)
		}
		out = append(out, fds...)
	}
	return out, nil
}

// WriteMessage sends one message to the compositor. Pass fds to
// transfer file descriptors via SCM_RIGHTS along with the message
// bytes — the compositor's protocol XML declares which messages
// expect fd arguments; the daemon decides per-message.
//
// Concurrent writers serialise via the Conn's write mutex.
func (c *Conn) WriteMessage(objectID uint32, opcode uint16, body []byte, fds []int) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	totalSize := HeaderSize + len(body)
	if totalSize > 0xFFFF {
		return fmt.Errorf("wlwire: message size %d exceeds u16 header limit", totalSize)
	}
	hdr := make([]byte, HeaderSize)
	EncodeHeader(hdr, Header{
		ObjectID: objectID,
		Opcode:   opcode,
		Size:     uint16(totalSize),
	})
	payload := append(hdr, body...)
	var oob []byte
	if len(fds) > 0 {
		oob = syscall.UnixRights(fds...)
	}
	_, _, err := c.sock.WriteMsgUnix(payload, oob, nil)
	return err
}
