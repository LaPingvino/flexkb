// Package wlwire is the wire-protocol primitives for talking to a
// Wayland compositor. It deliberately implements only the binary
// framing and argument marshalling defined in the upstream Wayland
// protocol spec (https://wayland.freedesktop.org/docs/html/ch04.html),
// nothing higher level. Per-interface code (wl_registry, wl_display,
// input-method-v2, …) lives in sibling packages and uses this one
// to encode/decode messages.
//
// Design choice: hand-rolled rather than depending on a third-party
// binding generator. The wire format is small, well-specified, and
// hasn't changed in over a decade — owning it ourselves means a
// generator we can regenerate from any future protocol XML, no
// upstream dependency churn, and a codebase a language model can
// fully reason about from the source alone.
//
// References:
//   - Wayland book: https://wayland-book.com/protocol-design/wire-protocol.html
//   - Spec ch.4 "Wire format":
//     https://wayland.freedesktop.org/docs/html/ch04.html
//   - For comparison: libwayland (CGo), rajveermalviya/go-wayland
//     (pure-Go generator). This package is intentionally smaller
//     than either — only what flexkb-imed actually needs.
package wlwire

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
)

// HeaderSize is the byte length of a Wayland message header. Every
// message starts with object-id (u32) + opcode (u16) + size (u16),
// all little-endian.
const HeaderSize = 8

// Header is a parsed Wayland message header. Size is the *total*
// length of the message including this header, so the payload
// length is Size - HeaderSize.
type Header struct {
	ObjectID uint32
	Opcode   uint16
	Size     uint16
}

// EncodeHeader serialises h into the first HeaderSize bytes of buf.
// Panics if buf is too small — caller's responsibility to size it.
func EncodeHeader(buf []byte, h Header) {
	binary.LittleEndian.PutUint32(buf[0:4], h.ObjectID)
	// Wayland packs opcode and size into one u32: high 16 bits are
	// size, low 16 bits are opcode. Spec ch.4: "the upper 16 bits
	// are the message size in bytes... the lower 16 bits are the
	// request/event opcode."
	binary.LittleEndian.PutUint32(buf[4:8], uint32(h.Size)<<16|uint32(h.Opcode))
}

// DecodeHeader parses a header from the first HeaderSize bytes of
// buf. Returns the header — no error case; malformed input shows
// up later when Size doesn't match the actual stream.
func DecodeHeader(buf []byte) Header {
	id := binary.LittleEndian.Uint32(buf[0:4])
	packed := binary.LittleEndian.Uint32(buf[4:8])
	return Header{
		ObjectID: id,
		Opcode:   uint16(packed & 0xFFFF),
		Size:     uint16(packed >> 16),
	}
}

// Encoder builds the body of a Wayland message. Arguments are
// written in declaration order matching the protocol XML; each
// Put* method appends one argument with the right padding.
type Encoder struct {
	buf []byte
}

// NewEncoder returns an Encoder backed by a fresh buffer.
func NewEncoder() *Encoder { return &Encoder{} }

// Bytes returns the encoded body. Caller should prepend a header
// with Size = HeaderSize + len(Bytes()).
func (e *Encoder) Bytes() []byte { return e.buf }

// Len returns the current body length (without the header).
func (e *Encoder) Len() int { return len(e.buf) }

// PutInt appends a signed 32-bit integer argument.
func (e *Encoder) PutInt(v int32) {
	var b [4]byte
	binary.LittleEndian.PutUint32(b[:], uint32(v))
	e.buf = append(e.buf, b[:]...)
}

// PutUint appends an unsigned 32-bit integer argument. Used for
// object IDs, new-IDs (typed form), and enum values.
func (e *Encoder) PutUint(v uint32) {
	var b [4]byte
	binary.LittleEndian.PutUint32(b[:], v)
	e.buf = append(e.buf, b[:]...)
}

// PutFixed appends a 24.8 fixed-point argument. The wire format is
// signed 24.8 in a 32-bit container (the integer part in the high
// 24 bits, the fractional part in the low 8). Pointer coordinates
// from the compositor use this; we'll need it for cursor-position
// messages even though the IM tier itself doesn't.
func (e *Encoder) PutFixed(v float64) {
	scaled := int32(math.Round(v * 256))
	e.PutInt(scaled)
}

// PutString appends a string argument: a u32 length (in bytes,
// INCLUDING the trailing null), the bytes themselves, a null
// terminator, and padding to the next 32-bit boundary. An empty
// string is encoded as length=0.
//
// The protocol spec's edge case: length=0 means the string is
// absent (NULL in C-API terms); length=1 means empty string
// (just the null terminator). We treat empty Go strings as
// length=0 (NULL) since flexkb-imed's senders shouldn't need to
// distinguish; receivers should accept both. Callers that need
// the present-but-empty form can append a zero-byte string via
// PutBytes if it ever matters.
func (e *Encoder) PutString(s string) {
	if s == "" {
		e.PutUint(0)
		return
	}
	totalLen := uint32(len(s) + 1) // bytes + null terminator
	e.PutUint(totalLen)
	e.buf = append(e.buf, s...)
	e.buf = append(e.buf, 0) // null terminator
	// Pad to 32-bit boundary.
	for (len(e.buf) % 4) != 0 {
		e.buf = append(e.buf, 0)
	}
}

// PutArray appends a generic byte-array argument: a u32 length
// (bytes), the bytes, padding to 32-bit alignment. No null
// terminator (unlike PutString — arrays carry arbitrary binary
// data).
func (e *Encoder) PutArray(data []byte) {
	e.PutUint(uint32(len(data)))
	e.buf = append(e.buf, data...)
	for (len(e.buf) % 4) != 0 {
		e.buf = append(e.buf, 0)
	}
}

// Decoder reads typed arguments out of a message body. Returns
// the zero value of the requested type on under-read; callers
// should check via Err() at the end if they need to distinguish
// "truncated message" from "valid zero".
type Decoder struct {
	buf []byte
	pos int
	err error
}

// NewDecoder wraps body bytes for sequential read.
func NewDecoder(body []byte) *Decoder { return &Decoder{buf: body} }

// Err returns the first error encountered during a chain of reads,
// or nil if the entire decoded sequence was well-formed.
func (d *Decoder) Err() error { return d.err }

// Remaining returns how many body bytes are still unread.
func (d *Decoder) Remaining() int { return len(d.buf) - d.pos }

// Int reads a signed 32-bit integer argument.
func (d *Decoder) Int() int32 {
	if d.err != nil || d.pos+4 > len(d.buf) {
		d.fail("int")
		return 0
	}
	v := int32(binary.LittleEndian.Uint32(d.buf[d.pos:]))
	d.pos += 4
	return v
}

// Uint reads an unsigned 32-bit integer argument (object IDs,
// typed new-IDs, enum values).
func (d *Decoder) Uint() uint32 {
	if d.err != nil || d.pos+4 > len(d.buf) {
		d.fail("uint")
		return 0
	}
	v := binary.LittleEndian.Uint32(d.buf[d.pos:])
	d.pos += 4
	return v
}

// Fixed reads a 24.8 fixed-point argument as a float64.
func (d *Decoder) Fixed() float64 {
	v := d.Int()
	return float64(v) / 256.0
}

// String reads a string argument. Returns "" for the NULL form
// (length=0); a non-NULL empty string (length=1, just the null
// terminator) also returns "". Callers needing the distinction
// should use the lower-level Bytes/Uint primitives.
func (d *Decoder) String() string {
	totalLen := d.Uint()
	if d.err != nil || totalLen == 0 {
		return ""
	}
	if d.pos+int(totalLen) > len(d.buf) {
		d.fail("string body")
		return ""
	}
	// totalLen includes the null terminator.
	s := string(d.buf[d.pos : d.pos+int(totalLen)-1])
	d.pos += int(totalLen)
	// Skip padding to 32-bit boundary.
	for (d.pos % 4) != 0 {
		d.pos++
	}
	return s
}

// Bytes reads a generic byte-array argument (the array primitive,
// not String — no null terminator). Returns a fresh copy of the
// bytes so the caller can hold onto them across further reads.
func (d *Decoder) Bytes() []byte {
	totalLen := d.Uint()
	if d.err != nil || totalLen == 0 {
		return nil
	}
	if d.pos+int(totalLen) > len(d.buf) {
		d.fail("bytes body")
		return nil
	}
	out := make([]byte, totalLen)
	copy(out, d.buf[d.pos:d.pos+int(totalLen)])
	d.pos += int(totalLen)
	for (d.pos % 4) != 0 {
		d.pos++
	}
	return out
}

func (d *Decoder) fail(what string) {
	if d.err == nil {
		d.err = fmt.Errorf("wlwire decode: short read on %s at offset %d (buf=%d)", what, d.pos, len(d.buf))
	}
}

// ReadMessage reads one complete Wayland message from r. Returns
// the parsed header and the body bytes (header excluded). The
// caller wraps the body in NewDecoder to extract arguments.
//
// io.EOF surfaces unchanged so callers can detect peer-close.
// io.ErrUnexpectedEOF means the stream ended mid-message — that's
// a protocol error and the connection should be torn down.
func ReadMessage(r io.Reader) (Header, []byte, error) {
	var hdrBuf [HeaderSize]byte
	if _, err := io.ReadFull(r, hdrBuf[:]); err != nil {
		return Header{}, nil, err
	}
	h := DecodeHeader(hdrBuf[:])
	if h.Size < HeaderSize {
		return Header{}, nil, fmt.Errorf("wlwire: header size %d shorter than header itself", h.Size)
	}
	bodyLen := int(h.Size) - HeaderSize
	body := make([]byte, bodyLen)
	if _, err := io.ReadFull(r, body); err != nil {
		if errors.Is(err, io.EOF) {
			return Header{}, nil, io.ErrUnexpectedEOF
		}
		return Header{}, nil, err
	}
	return h, body, nil
}

// WriteMessage emits a complete Wayland message to w: header
// followed by body. Used by clients to send requests and by
// tests to simulate server events.
func WriteMessage(w io.Writer, objectID uint32, opcode uint16, body []byte) error {
	totalSize := HeaderSize + len(body)
	if totalSize > 0xFFFF {
		return fmt.Errorf("wlwire: message size %d exceeds u16 header limit", totalSize)
	}
	var hdrBuf [HeaderSize]byte
	EncodeHeader(hdrBuf[:], Header{
		ObjectID: objectID,
		Opcode:   opcode,
		Size:     uint16(totalSize),
	})
	if _, err := w.Write(hdrBuf[:]); err != nil {
		return err
	}
	if len(body) == 0 {
		return nil
	}
	_, err := w.Write(body)
	return err
}
