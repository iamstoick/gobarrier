package server

import (
	"bytes"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/yourusername/gobarrier/internal/protocol"
)

// ClientState tracks the lifecycle of a connected secondary screen.
type ClientState int

const (
	StateHandshake ClientState = iota
	StateConnected
	StateDisconnected
)

// ScreenInfo holds the geometry reported by the client.
type ScreenInfo struct {
	X, Y   int16
	W, H   int16
	MouseX, MouseY int16
}

// Client represents one connected secondary screen.
type Client struct {
	name    string
	conn    net.Conn
	state   ClientState
	info    ScreenInfo
	seqNum  uint32

	mu      sync.Mutex
	writeMu sync.Mutex // serialises writes to conn

	// channels
	incoming chan []byte
	done     chan struct{}
	srv      *Server
}

func newClient(conn net.Conn, srv *Server) *Client {
	return &Client{
		conn:     conn,
		state:    StateHandshake,
		incoming: make(chan []byte, 256),
		done:     make(chan struct{}),
		srv:      srv,
	}
}

// Name returns the screen name.
func (c *Client) Name() string { return c.name }

// Info returns the last-reported screen geometry.
func (c *Client) Info() ScreenInfo {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.info
}

// send serialises a message write to the connection.
func (c *Client) send(code string, body []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return protocol.WriteMessage(c.conn, code, body)
}

// ---- Input injection helpers ----------------------------------------

func (c *Client) SendEnter(x, y int16) error {
	c.mu.Lock()
	c.seqNum++
	seq := c.seqNum
	c.mu.Unlock()
	return c.send(protocol.MsgCEnter, protocol.BuildEnter(x, y, seq, 0))
}

func (c *Client) SendLeave() error {
	return c.send(protocol.MsgCLeave, nil)
}

func (c *Client) SendMouseMove(x, y int16) error {
	return c.send(protocol.MsgDMouseMove, protocol.BuildMouseMove(x, y))
}

func (c *Client) SendMouseRelMove(dx, dy int16) error {
	return c.send(protocol.MsgDMouseRel, protocol.BuildMouseRelMove(dx, dy))
}

func (c *Client) SendMouseDown(btn uint8) error {
	return c.send(protocol.MsgDMouseDown, protocol.BuildMouseButton(btn))
}

func (c *Client) SendMouseUp(btn uint8) error {
	return c.send(protocol.MsgDMouseUp, protocol.BuildMouseButton(btn))
}

func (c *Client) SendMouseWheel(xd, yd int16) error {
	return c.send(protocol.MsgDMouseWheel, protocol.BuildMouseWheel(xd, yd))
}

func (c *Client) SendKeyDown(keyID, mods, btn uint16) error {
	return c.send(protocol.MsgDKeyDown, protocol.BuildKeyDown(keyID, mods, btn))
}

func (c *Client) SendKeyRepeat(keyID, mods, count, btn uint16) error {
	return c.send(protocol.MsgDKeyRepeat, protocol.BuildKeyRepeat(keyID, mods, count, btn))
}

func (c *Client) SendKeyUp(keyID, mods, btn uint16) error {
	return c.send(protocol.MsgDKeyUp, protocol.BuildKeyUp(keyID, mods, btn))
}

func (c *Client) SendKeepAlive() error {
	return c.send(protocol.MsgCKeepAlive, nil)
}

func (c *Client) SendResetOptions() error {
	return c.send(protocol.MsgCResetOpts, nil)
}

func (c *Client) SendInfoAck() error {
	return c.send(protocol.MsgCInfoAck, nil)
}

// SendClipboard streams clipboard data to this client.
func (c *Client) SendClipboard(id uint8, seqNum uint32, data string) error {
	// mark=0 means direct clipboard data (not streaming)
	var buf bytes.Buffer
	bw := protocol.NewWriter(&buf)
	bw.WriteUint8(id)
	bw.WriteUint32(seqNum)
	bw.WriteUint8(0) // mark: direct data
	bw.WriteString(data)
	return c.send(protocol.MsgDClipboard, buf.Bytes())
}

// SendFileChunk sends one chunk of a file transfer.
func (c *Client) SendFileChunk(mark uint8, data []byte) error {
	return c.send(protocol.MsgDFileTransfer, protocol.BuildFileTransferChunk(mark, data))
}

// SendDragInfo announces a drag operation with the given file paths.
func (c *Client) SendDragInfo(paths []string) error {
	return c.send(protocol.MsgDDragInfo, protocol.BuildDragInfo(paths))
}

// ---- Read loop -------------------------------------------------------

// readLoop runs in a goroutine and feeds raw messages into c.incoming.
func (c *Client) readLoop() {
	defer close(c.done)
	for {
		msg, err := protocol.ReadMessage(c.conn)
		if err != nil {
			slog.Info("client read error", "screen", c.name, "err", err)
			return
		}
		select {
		case c.incoming <- msg:
		default:
			slog.Warn("client incoming buffer full, dropping message", "screen", c.name)
		}
	}
}

// ---- Handshake -------------------------------------------------------

// performHandshake sends the server hello and waits for the client response.
// Returns the client's reported screen name.
func (c *Client) performHandshake() error {
	// Send hello.
	hello := protocol.ServerHello(protocol.MajorVersion, protocol.MinorVersion)
	if _, err := c.conn.Write(hello); err != nil {
		return fmt.Errorf("send hello: %w", err)
	}

	// Read hello-back: same 11-byte header + 4-byte length + name bytes.
	// Total minimum: 11 + 4 + 1 = 16 bytes.
	c.conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	defer c.conn.SetReadDeadline(time.Time{})

	header := make([]byte, 11)
	if _, err := readFull(c.conn, header); err != nil {
		return fmt.Errorf("read hello-back header: %w", err)
	}
	major, minor, err := protocol.ParseServerHello(header)
	if err != nil {
		return fmt.Errorf("parse hello-back: %w", err)
	}
	if major != protocol.MajorVersion {
		c.send(protocol.MsgEIncompat, buildIncompatMsg(protocol.MajorVersion, protocol.MinorVersion))
		return fmt.Errorf("incompatible client version %d.%d", major, minor)
	}

	// Read screen name (4-byte length-prefixed string).
	r := protocol.NewReader(c.conn)
	name, err := r.ReadString()
	if err != nil {
		return fmt.Errorf("read screen name: %w", err)
	}
	if name == "" {
		return fmt.Errorf("empty screen name")
	}
	c.name = name
	c.state = StateConnected
	slog.Info("client handshake complete", "screen", name, "version", fmt.Sprintf("%d.%d", major, minor))
	return nil
}

func buildIncompatMsg(major, minor uint16) []byte {
	b := make([]byte, 4)
	b[0] = byte(major >> 8)
	b[1] = byte(major)
	b[2] = byte(minor >> 8)
	b[3] = byte(minor)
	return b
}

func readFull(r net.Conn, buf []byte) (int, error) {
	total := 0
	for total < len(buf) {
		n, err := r.Read(buf[total:])
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}
