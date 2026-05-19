package wlwire

import (
	"bytes"
	"errors"
	"io"
	"reflect"
	"testing"
)

// TestHeaderRoundTrip — encoding then decoding a header must
// preserve all three fields exactly. Probes both the bit-packing
// (size and opcode share a u32) and the endianness.
func TestHeaderRoundTrip(t *testing.T) {
	cases := []Header{
		{ObjectID: 1, Opcode: 0, Size: 12},
		{ObjectID: 0xCAFEBABE, Opcode: 0xFFFF, Size: 0xFFFF},
		{ObjectID: 7, Opcode: 1, Size: 20},
	}
	for _, want := range cases {
		t.Run("", func(t *testing.T) {
			var buf [HeaderSize]byte
			EncodeHeader(buf[:], want)
			got := DecodeHeader(buf[:])
			if got != want {
				t.Errorf("got %+v, want %+v", got, want)
			}
		})
	}
}

// TestHeaderWireBytes — the encoded header matches the byte
// layout the spec dictates. Without this, a wire-compatible
// implementation could pass roundtrip tests while actually
// emitting bytes a real compositor wouldn't accept.
func TestHeaderWireBytes(t *testing.T) {
	var buf [HeaderSize]byte
	EncodeHeader(buf[:], Header{ObjectID: 1, Opcode: 2, Size: 12})
	want := []byte{
		// object-id = 1, little-endian
		0x01, 0x00, 0x00, 0x00,
		// packed: high u16 = size=12, low u16 = opcode=2
		// → packed = 0x000C0002 → little-endian bytes 02 00 0C 00
		0x02, 0x00, 0x0C, 0x00,
	}
	if !bytes.Equal(buf[:], want) {
		t.Errorf("got %x, want %x", buf[:], want)
	}
}

// TestEncoderArgRoundTrip exercises every supported argument
// type. Encode a sequence, decode in the same order, expect
// the same values back.
func TestEncoderArgRoundTrip(t *testing.T) {
	e := NewEncoder()
	e.PutInt(-42)
	e.PutUint(0xDEADBEEF)
	e.PutFixed(3.5) // exact in 24.8
	e.PutString("hello")
	e.PutString("")
	e.PutString("wayland")
	e.PutArray([]byte{0x01, 0x02, 0x03})

	d := NewDecoder(e.Bytes())
	if got := d.Int(); got != -42 {
		t.Errorf("Int: got %d, want -42", got)
	}
	if got := d.Uint(); got != 0xDEADBEEF {
		t.Errorf("Uint: got %x", got)
	}
	if got := d.Fixed(); got != 3.5 {
		t.Errorf("Fixed: got %v, want 3.5", got)
	}
	if got := d.String(); got != "hello" {
		t.Errorf("String: got %q", got)
	}
	if got := d.String(); got != "" {
		t.Errorf("Empty string: got %q", got)
	}
	if got := d.String(); got != "wayland" {
		t.Errorf("String 2: got %q", got)
	}
	if got := d.Bytes(); !reflect.DeepEqual(got, []byte{0x01, 0x02, 0x03}) {
		t.Errorf("Bytes: got %x", got)
	}
	if err := d.Err(); err != nil {
		t.Errorf("err: %v", err)
	}
	if d.Remaining() != 0 {
		t.Errorf("Remaining bytes after roundtrip: %d", d.Remaining())
	}
}

// TestStringPadding — strings must pad to 32-bit alignment. A
// non-aligned length means the next argument decodes off-by-N.
func TestStringPadding(t *testing.T) {
	cases := []struct {
		s       string
		bodyLen int // expected body length: 4 (u32 length) + bytes + null + padding
	}{
		{"a", 4 + 4},        // "a\0" → 2 bytes + 2 pad = 4 body bytes + 4 header
		{"ab", 4 + 4},       // "ab\0" → 3 bytes + 1 pad
		{"abc", 4 + 4},      // "abc\0" → 4 bytes, no pad
		{"abcd", 4 + 8},     // "abcd\0" → 5 bytes + 3 pad
		{"", 4},             // length=0 only
	}
	for _, c := range cases {
		t.Run(c.s, func(t *testing.T) {
			e := NewEncoder()
			e.PutString(c.s)
			if got := e.Len(); got != c.bodyLen {
				t.Errorf("PutString(%q): body len %d, want %d", c.s, got, c.bodyLen)
			}
			// Decode and verify alignment was actually correct.
			d := NewDecoder(e.Bytes())
			if got := d.String(); got != c.s {
				t.Errorf("roundtrip: got %q, want %q", got, c.s)
			}
		})
	}
}

// TestArrayPadding — same alignment concern as strings, but
// without the null terminator surprise.
func TestArrayPadding(t *testing.T) {
	cases := []struct {
		data    []byte
		bodyLen int
	}{
		{[]byte{1}, 4 + 4},          // 1 byte + 3 pad
		{[]byte{1, 2}, 4 + 4},       // 2 + 2 pad
		{[]byte{1, 2, 3}, 4 + 4},    // 3 + 1 pad
		{[]byte{1, 2, 3, 4}, 4 + 4}, // 4 + 0 pad
		{[]byte{1, 2, 3, 4, 5}, 4 + 8},
		{[]byte{}, 4},
	}
	for _, c := range cases {
		t.Run("", func(t *testing.T) {
			e := NewEncoder()
			e.PutArray(c.data)
			if got := e.Len(); got != c.bodyLen {
				t.Errorf("len %d want %d", got, c.bodyLen)
			}
			d := NewDecoder(e.Bytes())
			got := d.Bytes()
			if !bytes.Equal(got, c.data) && !(len(got) == 0 && len(c.data) == 0) {
				t.Errorf("got %x, want %x", got, c.data)
			}
		})
	}
}

// TestFixedPointConversion — 24.8 fixed must encode/decode to
// the same value, with 1/256 precision.
func TestFixedPointConversion(t *testing.T) {
	cases := []float64{0, 1, -1, 0.5, -0.5, 0.00390625, 100.25, -100.75}
	for _, v := range cases {
		t.Run("", func(t *testing.T) {
			e := NewEncoder()
			e.PutFixed(v)
			d := NewDecoder(e.Bytes())
			if got := d.Fixed(); got != v {
				t.Errorf("got %v, want %v", got, v)
			}
		})
	}
}

// TestReadWriteMessage — end-to-end framing via an in-memory
// pipe. Mirrors what the daemon will do over its real socket:
// send a request, read a server-emitted event, verify both
// directions match the spec.
func TestReadWriteMessage(t *testing.T) {
	var pipe bytes.Buffer
	body := NewEncoder()
	body.PutUint(42)
	body.PutString("wl_compositor")
	if err := WriteMessage(&pipe, 1, 3, body.Bytes()); err != nil {
		t.Fatalf("WriteMessage: %v", err)
	}
	h, gotBody, err := ReadMessage(&pipe)
	if err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}
	if h.ObjectID != 1 || h.Opcode != 3 {
		t.Errorf("header: got %+v", h)
	}
	if int(h.Size) != HeaderSize+len(body.Bytes()) {
		t.Errorf("size: got %d, want %d", h.Size, HeaderSize+len(body.Bytes()))
	}
	d := NewDecoder(gotBody)
	if got := d.Uint(); got != 42 {
		t.Errorf("u32: got %d", got)
	}
	if got := d.String(); got != "wl_compositor" {
		t.Errorf("string: got %q", got)
	}
}

// TestReadMessageEOF — a peer-close at message boundary surfaces
// as io.EOF unchanged so callers can detect clean disconnect.
func TestReadMessageEOF(t *testing.T) {
	_, _, err := ReadMessage(bytes.NewReader(nil))
	if !errors.Is(err, io.EOF) {
		t.Errorf("got %v, want EOF", err)
	}
}

// TestReadMessageTruncated — a partial message in the middle of
// the body is a protocol error, distinct from clean EOF.
func TestReadMessageTruncated(t *testing.T) {
	// Header claims size=16 but only 4 body bytes provided.
	var hdrBuf [HeaderSize]byte
	EncodeHeader(hdrBuf[:], Header{ObjectID: 1, Opcode: 0, Size: 16})
	stream := append(hdrBuf[:], 0x00, 0x00, 0x00, 0x00) // 4 of 8 body bytes
	_, _, err := ReadMessage(bytes.NewReader(stream))
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("got %v, want ErrUnexpectedEOF", err)
	}
}

// TestDecoderShortRead — reading past the body surfaces via Err()
// without panicking. The encoded length is 4 bytes; pulling two
// Uints must error on the second.
func TestDecoderShortRead(t *testing.T) {
	d := NewDecoder([]byte{0x01, 0x00, 0x00, 0x00})
	if got := d.Uint(); got != 1 {
		t.Errorf("first uint: got %d", got)
	}
	_ = d.Uint() // second pull — past end
	if d.Err() == nil {
		t.Error("expected error after short read")
	}
}
