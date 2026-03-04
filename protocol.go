// Package protocol implements the Barrier/Synergy wire protocol (v1.6).
//
// Wire format overview:
//   - Each message is prefixed with a 4-byte big-endian uint32 length (byte count of the payload).
//   - The payload starts with a 4-byte ASCII message code (e.g. "DMMV").
//   - Format specifiers in message strings:
//       %1i = 1-byte  uint8
//       %2i = 2-byte  big-endian int16
//       %4i = 4-byte  big-endian int32
//       %4I = 4-byte  big-endian uint32 (used in option lists)
//       %s  = 4-byte length prefix + UTF-8 string bytes
//
// Greeting handshake (no length prefix):
//   Server → "Barrier\000\001\000\006"   (magic + major=1 + minor=6)
//   Client → "Barrier\000\001\000\006" + %s(screenName)
package protocol

import (
	"encoding/binary"
	"fmt"
	"io"
)

// Protocol version this implementation speaks.
const (
	MajorVersion uint16 = 1
	MinorVersion uint16 = 6
	DefaultPort         = 24800
)

// Magic bytes that start every hello/hello-back message.
var Magic = []byte("Barrier")

// KeepAliveInterval is how often the server sends CALV messages.
const KeepAliveInterval = 3 // seconds

// Message codes — 4 ASCII bytes, no NUL.
const (
	// Control
	MsgCNoop        = "CNOP" // no-op
	MsgCClose       = "CBYE" // close connection
	MsgCEnter       = "CINN" // enter screen  (x,y,seqNum,modifiers)
	MsgCLeave       = "COUT" // leave screen
	MsgCClipboard   = "CCLP" // clipboard grab (id, seqNum)
	MsgCScreenSaver = "CSEC" // screensaver state (on=1/off=0)
	MsgCResetOpts   = "CROP" // reset options to defaults
	MsgCInfoAck     = "CIAK" // ack for DINF
	MsgCKeepAlive   = "CALV" // keep-alive ping

	// Data – primary → secondary (input injection)
	MsgDKeyDown    = "DKDN" // key press   (keyID, modifiers, button)
	MsgDKeyRepeat  = "DKRP" // key repeat  (keyID, modifiers, count, button)
	MsgDKeyUp      = "DKUP" // key release (keyID, modifiers, button)
	MsgDMouseDown  = "DMDN" // button press   (buttonID)
	MsgDMouseUp    = "DMUP" // button release (buttonID)
	MsgDMouseMove  = "DMMV" // absolute move  (x, y)
	MsgDMouseRel   = "DMRM" // relative move  (dx, dy)
	MsgDMouseWheel = "DMWM" // scroll         (xDelta, yDelta)
	MsgDClipboard  = "DCLP" // clipboard data (id, seqNum, mark, data)
	MsgDInfo       = "DINF" // screen info    (x,y,w,h,unused,mx,my)
	MsgDSetOptions = "DSOP" // set options    (key/value pairs)

	// File transfer & drag (v1.5+)
	MsgDFileTransfer = "DFTR" // file chunk     (mark, data)
	MsgDDragInfo     = "DDRG" // drag info      (numItems, pathList)

	// Query
	MsgQInfo = "QINF" // request screen info from client

	// Errors
	MsgEIncompat = "EICV" // incompatible versions (major, minor)
	MsgEBusy     = "EBSY" // screen name already in use
	MsgEUnknown  = "EUNK" // unknown client screen name
	MsgEBad      = "EBAD" // protocol violation
)

// FileTransfer marks used in DFTR messages.
const (
	FileMarkSize  = 0 // payload is ASCII file size string
	FileMarkChunk = 1 // payload is raw chunk bytes
	FileMarkEnd   = 2 // transfer complete
)

// Direction for screen neighbours.
type Direction uint8

const (
	DirLeft   Direction = 1
	DirRight  Direction = 2
	DirTop    Direction = 3
	DirBottom Direction = 4
)

// OptionID values used in DSOP.
const (
	OptHeartbeat          uint32 = 0x00000001
	OptScreenSaverSync    uint32 = 0x00000002
	OptTwoTapScrollDelay  uint32 = 0x00000003
	OptRelativeMouse      uint32 = 0x00000004
	OptLanguageSync       uint32 = 0x00000005
	OptWin32KeepFgd       uint32 = 0x00000006
	OptSwitchDelay        uint32 = 0x00000010
	OptSwitchTwoTap       uint32 = 0x00000011
	OptSwitchNeedsShift   uint32 = 0x00000012
	OptSwitchNeedsControl uint32 = 0x00000013
	OptSwitchNeedsAlt     uint32 = 0x00000014
	OptScreenScrollDir    uint32 = 0x00000020
)

// --------------------------------------------------------------------------
// Low-level encoder / decoder helpers
// --------------------------------------------------------------------------

// Writer wraps an io.Writer with helpers for the Barrier wire format.
type Writer struct {
	w io.Writer
}

func NewWriter(w io.Writer) *Writer { return &Writer{w: w} }

func (w *Writer) WriteUint8(v uint8) error {
	_, err := w.w.Write([]byte{v})
	return err
}

func (w *Writer) WriteUint16(v uint16) error {
	b := [2]byte{}
	binary.BigEndian.PutUint16(b[:], v)
	_, err := w.w.Write(b[:])
	return err
}

func (w *Writer) WriteInt16(v int16) error {
	return w.WriteUint16(uint16(v))
}

func (w *Writer) WriteUint32(v uint32) error {
	b := [4]byte{}
	binary.BigEndian.PutUint32(b[:], v)
	_, err := w.w.Write(b[:])
	return err
}

func (w *Writer) WriteInt32(v int32) error {
	return w.WriteUint32(uint32(v))
}

// WriteString writes a Barrier-encoded string: 4-byte length + raw bytes.
func (w *Writer) WriteString(s string) error {
	if err := w.WriteUint32(uint32(len(s))); err != nil {
		return err
	}
	_, err := w.w.Write([]byte(s))
	return err
}

// WriteBytes writes a Barrier-encoded byte slice: 4-byte length + raw bytes.
func (w *Writer) WriteBytes(b []byte) error {
	if err := w.WriteUint32(uint32(len(b))); err != nil {
		return err
	}
	_, err := w.w.Write(b)
	return err
}

// WriteCode writes a 4-byte message code (no length prefix).
func (w *Writer) WriteCode(code string) error {
	if len(code) != 4 {
		return fmt.Errorf("message code must be 4 bytes, got %q", code)
	}
	_, err := w.w.Write([]byte(code))
	return err
}

// Reader wraps an io.Reader with helpers for the Barrier wire format.
type Reader struct {
	r io.Reader
}

func NewReader(r io.Reader) *Reader { return &Reader{r: r} }

func (r *Reader) ReadUint8() (uint8, error) {
	b := [1]byte{}
	_, err := io.ReadFull(r.r, b[:])
	return b[0], err
}

func (r *Reader) ReadUint16() (uint16, error) {
	b := [2]byte{}
	_, err := io.ReadFull(r.r, b[:])
	return binary.BigEndian.Uint16(b[:]), err
}

func (r *Reader) ReadInt16() (int16, error) {
	v, err := r.ReadUint16()
	return int16(v), err
}

func (r *Reader) ReadUint32() (uint32, error) {
	b := [4]byte{}
	_, err := io.ReadFull(r.r, b[:])
	return binary.BigEndian.Uint32(b[:]), err
}

func (r *Reader) ReadInt32() (int32, error) {
	v, err := r.ReadUint32()
	return int32(v), err
}

// ReadString reads a Barrier-encoded string (4-byte length prefix + bytes).
func (r *Reader) ReadString() (string, error) {
	length, err := r.ReadUint32()
	if err != nil {
		return "", err
	}
	if length > 4*1024*1024 {
		return "", fmt.Errorf("string too long: %d bytes", length)
	}
	buf := make([]byte, length)
	_, err = io.ReadFull(r.r, buf)
	return string(buf), err
}

// ReadBytes reads a Barrier-encoded byte slice (4-byte length prefix + bytes).
func (r *Reader) ReadBytes() ([]byte, error) {
	length, err := r.ReadUint32()
	if err != nil {
		return nil, err
	}
	if length > 4*1024*1024 {
		return nil, fmt.Errorf("payload too long: %d bytes", length)
	}
	buf := make([]byte, length)
	_, err = io.ReadFull(r.r, buf)
	return buf, err
}

// ReadCode reads exactly 4 bytes and returns them as a string.
func (r *Reader) ReadCode() (string, error) {
	b := [4]byte{}
	_, err := io.ReadFull(r.r, b[:])
	return string(b[:]), err
}

// --------------------------------------------------------------------------
// Framed message I/O (length-prefix protocol used after the handshake)
// --------------------------------------------------------------------------

// ReadMessage reads one length-prefixed message and returns the raw payload
// (which starts with the 4-byte code).
func ReadMessage(r io.Reader) ([]byte, error) {
	rr := NewReader(r)
	length, err := rr.ReadUint32()
	if err != nil {
		return nil, err
	}
	if length == 0 {
		return []byte{}, nil
	}
	if length > 4*1024*1024 {
		return nil, fmt.Errorf("message too large: %d bytes", length)
	}
	buf := make([]byte, length)
	_, err = io.ReadFull(r, buf)
	return buf, err
}

// WriteMessage writes a length-prefixed message whose payload starts with the
// 4-byte code followed by the provided body bytes.
func WriteMessage(w io.Writer, code string, body []byte) error {
	if len(code) != 4 {
		return fmt.Errorf("message code must be 4 bytes, got %q", code)
	}
	total := uint32(4 + len(body))
	ww := NewWriter(w)
	if err := ww.WriteUint32(total); err != nil {
		return err
	}
	if err := ww.WriteCode(code); err != nil {
		return err
	}
	if len(body) > 0 {
		_, err := w.Write(body)
		return err
	}
	return nil
}

// MessageCode returns the first 4 bytes of a raw payload as the message code.
func MessageCode(payload []byte) string {
	if len(payload) < 4 {
		return ""
	}
	return string(payload[:4])
}

// PayloadReader returns a Reader over the body part of a raw payload
// (i.e. skipping the first 4 code bytes).
func PayloadReader(payload []byte) *Reader {
	if len(payload) > 4 {
		return NewReader(readerFromBytes(payload[4:]))
	}
	return NewReader(readerFromBytes(nil))
}

// --------------------------------------------------------------------------
// Greeting helpers
// --------------------------------------------------------------------------

// ServerHello constructs the initial greeting sent by the server.
// Format: "Barrier" + uint16(major) + uint16(minor)   (no length prefix)
func ServerHello(major, minor uint16) []byte {
	buf := make([]byte, 9)
	copy(buf, Magic)
	binary.BigEndian.PutUint16(buf[7:], major)
	buf[8] = 0 // pad to 9 feels off — Barrier uses two separate 2-byte fields
	// Re-do: Magic(7) + major(2) + minor(2) = 11 bytes
	out := make([]byte, 11)
	copy(out, Magic)
	binary.BigEndian.PutUint16(out[7:9], major)
	binary.BigEndian.PutUint16(out[9:11], minor)
	return out
}

// ParseServerHello validates and parses a server hello.
func ParseServerHello(data []byte) (major, minor uint16, err error) {
	if len(data) < 11 {
		return 0, 0, fmt.Errorf("hello too short: %d bytes", len(data))
	}
	if string(data[:7]) != string(Magic) {
		return 0, 0, fmt.Errorf("bad magic: %q", data[:7])
	}
	major = binary.BigEndian.Uint16(data[7:9])
	minor = binary.BigEndian.Uint16(data[9:11])
	return major, minor, nil
}

// ClientHelloBack constructs the client's response to the server hello.
// Format: "Barrier" + uint16(major) + uint16(minor) + string(screenName)
// (no outer length prefix — the string itself has the 4-byte inner length)
func ClientHelloBack(major, minor uint16, screenName string) []byte {
	nameBytes := []byte(screenName)
	out := make([]byte, 11+4+len(nameBytes))
	copy(out, Magic)
	binary.BigEndian.PutUint16(out[7:9], major)
	binary.BigEndian.PutUint16(out[9:11], minor)
	binary.BigEndian.PutUint32(out[11:15], uint32(len(nameBytes)))
	copy(out[15:], nameBytes)
	return out
}

// --------------------------------------------------------------------------
// Typed message builders  (return []byte body, caller passes to WriteMessage)
// --------------------------------------------------------------------------

func BuildEnter(x, y int16, seqNum uint32, modifiers uint16) []byte {
	b := make([]byte, 10)
	binary.BigEndian.PutUint16(b[0:2], uint16(x))
	binary.BigEndian.PutUint16(b[2:4], uint16(y))
	binary.BigEndian.PutUint32(b[4:8], seqNum)
	binary.BigEndian.PutUint16(b[8:10], modifiers)
	return b
}

func BuildMouseMove(x, y int16) []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint16(b[0:2], uint16(x))
	binary.BigEndian.PutUint16(b[2:4], uint16(y))
	return b
}

func BuildMouseRelMove(dx, dy int16) []byte {
	return BuildMouseMove(dx, dy)
}

func BuildMouseWheel(xDelta, yDelta int16) []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint16(b[0:2], uint16(xDelta))
	binary.BigEndian.PutUint16(b[2:4], uint16(yDelta))
	return b
}

func BuildMouseButton(buttonID uint8) []byte {
	return []byte{buttonID}
}

func BuildKeyDown(keyID, modifiers, button uint16) []byte {
	b := make([]byte, 6)
	binary.BigEndian.PutUint16(b[0:2], keyID)
	binary.BigEndian.PutUint16(b[2:4], modifiers)
	binary.BigEndian.PutUint16(b[4:6], button)
	return b
}

func BuildKeyRepeat(keyID, modifiers, count, button uint16) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint16(b[0:2], keyID)
	binary.BigEndian.PutUint16(b[2:4], modifiers)
	binary.BigEndian.PutUint16(b[4:6], count)
	binary.BigEndian.PutUint16(b[6:8], button)
	return b
}

func BuildKeyUp(keyID, modifiers, button uint16) []byte {
	return BuildKeyDown(keyID, modifiers, button)
}

func BuildInfo(x, y, w, h int16, mouseX, mouseY int16) []byte {
	b := make([]byte, 14)
	binary.BigEndian.PutUint16(b[0:2], uint16(x))
	binary.BigEndian.PutUint16(b[2:4], uint16(y))
	binary.BigEndian.PutUint16(b[4:6], uint16(w))
	binary.BigEndian.PutUint16(b[6:8], uint16(h))
	binary.BigEndian.PutUint16(b[8:10], 0) // obsolete warp zone
	binary.BigEndian.PutUint16(b[10:12], uint16(mouseX))
	binary.BigEndian.PutUint16(b[12:14], uint16(mouseY))
	return b
}

func BuildClipboardGrab(id uint8, seqNum uint32) []byte {
	b := make([]byte, 5)
	b[0] = id
	binary.BigEndian.PutUint32(b[1:5], seqNum)
	return b
}

// BuildFileTransferChunk builds a DFTR payload for a data chunk.
func BuildFileTransferChunk(mark uint8, data []byte) []byte {
	// mark(1) + string(4+len)
	out := make([]byte, 1+4+len(data))
	out[0] = mark
	binary.BigEndian.PutUint32(out[1:5], uint32(len(data)))
	copy(out[5:], data)
	return out
}

// BuildDragInfo builds a DDRG payload.
func BuildDragInfo(paths []string) []byte {
	totalLen := 0
	for _, p := range paths {
		totalLen += 4 + len(p)
	}
	out := make([]byte, 2+totalLen)
	binary.BigEndian.PutUint16(out[0:2], uint16(len(paths)))
	offset := 2
	for _, p := range paths {
		binary.BigEndian.PutUint32(out[offset:offset+4], uint32(len(p)))
		copy(out[offset+4:], []byte(p))
		offset += 4 + len(p)
	}
	return out
}

// --------------------------------------------------------------------------
// Parse helpers for incoming messages
// --------------------------------------------------------------------------

type MouseMoveMsg struct{ X, Y int16 }
type KeyMsg struct{ KeyID, Modifiers, Button uint16 }
type KeyRepeatMsg struct{ KeyID, Modifiers, Count, Button uint16 }
type MouseWheelMsg struct{ XDelta, YDelta int16 }
type EnterMsg struct {
	X, Y        int16
	SeqNum      uint32
	Modifiers   uint16
}
type InfoMsg struct {
	X, Y, W, H int16
	MouseX, MouseY int16
}

func ParseMouseMove(payload []byte) (MouseMoveMsg, error) {
	r := PayloadReader(payload)
	x, err := r.ReadInt16()
	if err != nil {
		return MouseMoveMsg{}, err
	}
	y, err := r.ReadInt16()
	return MouseMoveMsg{X: x, Y: y}, err
}

func ParseKeyDown(payload []byte) (KeyMsg, error) {
	r := PayloadReader(payload)
	kid, _ := r.ReadUint16()
	mod, _ := r.ReadUint16()
	btn, err := r.ReadUint16()
	return KeyMsg{KeyID: kid, Modifiers: mod, Button: btn}, err
}

func ParseKeyRepeat(payload []byte) (KeyRepeatMsg, error) {
	r := PayloadReader(payload)
	kid, _ := r.ReadUint16()
	mod, _ := r.ReadUint16()
	cnt, _ := r.ReadUint16()
	btn, err := r.ReadUint16()
	return KeyRepeatMsg{KeyID: kid, Modifiers: mod, Count: cnt, Button: btn}, err
}

func ParseMouseWheel(payload []byte) (MouseWheelMsg, error) {
	r := PayloadReader(payload)
	xd, _ := r.ReadInt16()
	yd, err := r.ReadInt16()
	return MouseWheelMsg{XDelta: xd, YDelta: yd}, err
}

func ParseEnter(payload []byte) (EnterMsg, error) {
	r := PayloadReader(payload)
	x, _ := r.ReadInt16()
	y, _ := r.ReadInt16()
	seq, _ := r.ReadUint32()
	mod, err := r.ReadUint16()
	return EnterMsg{X: x, Y: y, SeqNum: seq, Modifiers: mod}, err
}

func ParseInfo(payload []byte) (InfoMsg, error) {
	r := PayloadReader(payload)
	x, _ := r.ReadInt16()
	y, _ := r.ReadInt16()
	w, _ := r.ReadInt16()
	h, _ := r.ReadInt16()
	r.ReadUint16() // obsolete
	mx, _ := r.ReadInt16()
	my, err := r.ReadInt16()
	return InfoMsg{X: x, Y: y, W: w, H: h, MouseX: mx, MouseY: my}, err
}

// --------------------------------------------------------------------------
// Tiny byte-slice reader (avoids allocating a bytes.Buffer)
// --------------------------------------------------------------------------

type byteSliceReader struct {
	data []byte
	pos  int
}

func readerFromBytes(b []byte) io.Reader {
	return &byteSliceReader{data: b}
}

func (r *byteSliceReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.data) {
		return 0, io.EOF
	}
	n := copy(p, r.data[r.pos:])
	r.pos += n
	return n, nil
}
