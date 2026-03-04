package server

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/yourusername/gobarrier/internal/protocol"
	"github.com/yourusername/gobarrier/pkg/config"
)

// Server is the primary KVM switch.  It accepts connections from secondary
// screens, tracks which screen the cursor is currently on, and routes input
// events appropriately.
type Server struct {
	cfg    *config.Config
	layout *Layout

	mu      sync.RWMutex
	clients map[string]*Client // keyed by screen name (lowercase)

	// Active screen state.
	activeName string      // "" means primary (this machine)
	activeClient *Client   // nil means primary

	// Keep-alive ticker.
	keepAliveTicker *time.Ticker

	// Clipboard state.
	clipboardSeq  uint32
	clipboardOwner string
	clipboardData string

	// Primary screen size (set by the platform layer).
	primaryW, primaryH int16

	// callbacks from the platform input layer
	OnMouseMove    func(x, y int16)
	OnMouseDown    func(btn uint8)
	OnMouseUp      func(btn uint8)
	OnMouseWheel   func(xd, yd int16)
	OnKeyDown      func(keyID, mods, btn uint16)
	OnKeyRepeat    func(keyID, mods, count, btn uint16)
	OnKeyUp        func(keyID, mods, btn uint16)
}

// NewServer creates a Server from config.
func NewServer(cfg *config.Config) (*Server, error) {
	layout, err := NewLayout(cfg)
	if err != nil {
		return nil, err
	}
	return &Server{
		cfg:    cfg,
		layout: layout,
		clients: make(map[string]*Client),
	}, nil
}

// SetPrimarySize tells the server the resolution of the primary screen.
func (s *Server) SetPrimarySize(w, h int16) {
	s.primaryW = w
	s.primaryH = h
}

// Listen starts accepting connections.
func (s *Server) Listen(ctx context.Context) error {
	addr := fmt.Sprintf("%s:%d", s.cfg.Server.Host, s.cfg.Server.Port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	slog.Info("gobarrier server listening", "addr", addr)

	go s.keepAliveLoop(ctx)

	go func() {
		<-ctx.Done()
		ln.Close()
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return nil
			default:
				slog.Error("accept error", "err", err)
				continue
			}
		}
		go s.handleConn(conn)
	}
}

func (s *Server) handleConn(conn net.Conn) {
	c := newClient(conn, s)
	defer func() {
		conn.Close()
		s.removeClient(c)
		slog.Info("client disconnected", "screen", c.name)
	}()

	if err := c.performHandshake(); err != nil {
		slog.Warn("handshake failed", "err", err)
		return
	}

	lname := strings.ToLower(c.name)
	if _, ok := s.layout.screens[lname]; !ok {
		c.send(protocol.MsgEUnknown, nil)
		slog.Warn("unknown screen connected", "name", c.name)
		return
	}

	s.mu.Lock()
	if existing, ok := s.clients[lname]; ok {
		existing.conn.Close()
	}
	s.clients[lname] = c
	s.mu.Unlock()

	// Query for screen info immediately.
	c.send(protocol.MsgQInfo, nil)

	// Start read loop then dispatch messages.
	go c.readLoop()
	s.dispatch(c)
}

func (s *Server) dispatch(c *Client) {
	for {
		select {
		case msg, ok := <-c.incoming:
			if !ok {
				return
			}
			if err := s.handleClientMessage(c, msg); err != nil {
				slog.Warn("message handling error", "screen", c.name, "err", err)
			}
		case <-c.done:
			return
		}
	}
}

func (s *Server) handleClientMessage(c *Client, payload []byte) error {
	code := protocol.MessageCode(payload)
	switch code {
	case protocol.MsgDInfo:
		info, err := protocol.ParseInfo(payload)
		if err != nil {
			return err
		}
		c.mu.Lock()
		c.info = ScreenInfo{X: info.X, Y: info.Y, W: info.W, H: info.H,
			MouseX: info.MouseX, MouseY: info.MouseY}
		c.mu.Unlock()
		slog.Info("screen info updated", "screen", c.name,
			"size", fmt.Sprintf("%dx%d", info.W, info.H))
		c.SendInfoAck()

	case protocol.MsgCKeepAlive:
		c.SendKeepAlive() // echo back

	case protocol.MsgCClipboard:
		// client is claiming clipboard ownership — ask for data
		slog.Debug("client grabbed clipboard", "screen", c.name)

	case protocol.MsgDClipboard:
		s.handleIncomingClipboard(c, payload)

	case protocol.MsgDFileTransfer:
		s.handleFileTransfer(c, payload)

	case protocol.MsgDDragInfo:
		s.handleDragInfo(c, payload)

	case protocol.MsgCNoop:
		// nothing

	default:
		slog.Debug("unhandled message from client", "screen", c.name, "code", code)
	}
	return nil
}

func (s *Server) removeClient(c *Client) {
	if c.name == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	lname := strings.ToLower(c.name)
	if s.clients[lname] == c {
		delete(s.clients, lname)
	}
	if strings.ToLower(s.activeName) == lname {
		s.activeName = ""
		s.activeClient = nil
	}
}

// --------------------------------------------------------------------------
// Input routing — called from the platform input capture layer
// --------------------------------------------------------------------------

// RouteMouseMove is called with the current absolute cursor position on
// the primary display.  It decides whether to stay on this screen or
// switch to a neighbour.
func (s *Server) RouteMouseMove(x, y int16) {
	s.mu.RLock()
	active := s.activeName
	ac := s.activeClient
	s.mu.RUnlock()

	if active == "" {
		// Currently on primary screen.
		dir := s.edgeDirection(x, y, s.primaryW, s.primaryH)
		if dir != "" {
			s.switchToNeighbour(s.cfg.Server.ScreenName, dir, x, y)
			return
		}
		// Stay on primary — update cursor through the OS (no-op here, handled by platform).
		return
	}

	if ac == nil {
		return
	}

	// On a secondary screen: translate to client coordinates.
	info := ac.Info()
	cx := x // simplified — full implementation maps edges to client geometry
	cy := y
	dir := s.edgeDirection(cx, cy, info.W, info.H)
	if dir != "" {
		s.switchToNeighbour(active, dir, cx, cy)
		return
	}
	ac.SendMouseMove(cx, cy)
}

func (s *Server) edgeDirection(x, y, w, h int16) string {
	margin := int16(1)
	if x <= margin {
		return DirLeft
	}
	if x >= w-margin {
		return DirRight
	}
	if y <= margin {
		return DirTop
	}
	if y >= h-margin {
		return DirBottom
	}
	return ""
}

func (s *Server) switchToNeighbour(from, dir string, x, y int16) {
	nb := s.layout.Neighbour(from, dir)
	if nb == "" {
		return
	}

	s.mu.Lock()
	// Leave current screen.
	if s.activeClient != nil {
		s.activeClient.SendLeave()
	}
	primaryName := strings.ToLower(s.cfg.Server.ScreenName)
	s.activeName = nb
	var nbClient *Client
	if strings.ToLower(nb) != primaryName {
		nbClient = s.clients[strings.ToLower(nb)]
	}
	s.activeClient = nbClient
	s.mu.Unlock()

	if nbClient != nil {
		nbClient.SendEnter(x, y)
		slog.Info("switched to screen", "screen", nb)
	} else {
		slog.Info("returned to primary screen")
	}
}

// RouteMouseDown forwards a button press to the active secondary (or primary).
func (s *Server) RouteMouseDown(btn uint8) {
	s.mu.RLock()
	ac := s.activeClient
	s.mu.RUnlock()
	if ac != nil {
		ac.SendMouseDown(btn)
	} else {
		if s.OnMouseDown != nil {
			s.OnMouseDown(btn)
		}
	}
}

func (s *Server) RouteMouseUp(btn uint8) {
	s.mu.RLock()
	ac := s.activeClient
	s.mu.RUnlock()
	if ac != nil {
		ac.SendMouseUp(btn)
	} else {
		if s.OnMouseUp != nil {
			s.OnMouseUp(btn)
		}
	}
}

func (s *Server) RouteMouseWheel(xd, yd int16) {
	s.mu.RLock()
	ac := s.activeClient
	s.mu.RUnlock()
	if ac != nil {
		ac.SendMouseWheel(xd, yd)
	} else {
		if s.OnMouseWheel != nil {
			s.OnMouseWheel(xd, yd)
		}
	}
}

func (s *Server) RouteKeyDown(keyID, mods, btn uint16) {
	s.mu.RLock()
	ac := s.activeClient
	s.mu.RUnlock()
	if ac != nil {
		ac.SendKeyDown(keyID, mods, btn)
	} else {
		if s.OnKeyDown != nil {
			s.OnKeyDown(keyID, mods, btn)
		}
	}
}

func (s *Server) RouteKeyRepeat(keyID, mods, count, btn uint16) {
	s.mu.RLock()
	ac := s.activeClient
	s.mu.RUnlock()
	if ac != nil {
		ac.SendKeyRepeat(keyID, mods, count, btn)
	}
}

func (s *Server) RouteKeyUp(keyID, mods, btn uint16) {
	s.mu.RLock()
	ac := s.activeClient
	s.mu.RUnlock()
	if ac != nil {
		ac.SendKeyUp(keyID, mods, btn)
	} else {
		if s.OnKeyUp != nil {
			s.OnKeyUp(keyID, mods, btn)
		}
	}
}

// --------------------------------------------------------------------------
// Clipboard sharing
// --------------------------------------------------------------------------

func (s *Server) handleIncomingClipboard(c *Client, payload []byte) {
	r := protocol.PayloadReader(payload)
	id, _ := r.ReadUint8()
	seqNum, _ := r.ReadUint32()
	_ = id
	_ = seqNum
	_, _ = r.ReadUint8() // mark
	data, err := r.ReadString()
	if err != nil {
		return
	}

	slog.Debug("clipboard received", "from", c.name, "len", len(data))
	s.mu.Lock()
	s.clipboardData = data
	s.clipboardOwner = c.name
	s.clipboardSeq++
	seq := s.clipboardSeq
	s.mu.Unlock()

	// Broadcast to all other connected clients.
	s.mu.RLock()
	clients := make([]*Client, 0, len(s.clients))
	for _, cl := range s.clients {
		if cl != c {
			clients = append(clients, cl)
		}
	}
	s.mu.RUnlock()

	for _, cl := range clients {
		cl.SendClipboard(0, seq, data)
	}
	// TODO: also push to primary OS clipboard via platform layer
}

// --------------------------------------------------------------------------
// File transfer & drag-and-drop (new capability vs Barrier)
// --------------------------------------------------------------------------

const fileChunkSize = 64 * 1024 // 64 KB chunks

// SendFileTo streams a file to the named secondary screen using DFTR messages.
// This is the implementation of the drag-and-drop file transfer feature.
func (s *Server) SendFileTo(screenName string, data []byte, filename string) error {
	s.mu.RLock()
	c := s.clients[strings.ToLower(screenName)]
	s.mu.RUnlock()
	if c == nil {
		return fmt.Errorf("screen %q not connected", screenName)
	}

	// Announce filename + size.
	sizeStr := fmt.Sprintf("%d", len(data))
	if err := c.SendFileChunk(protocol.FileMarkSize, []byte(sizeStr)); err != nil {
		return err
	}

	// Stream data in chunks.
	for offset := 0; offset < len(data); offset += fileChunkSize {
		end := offset + fileChunkSize
		if end > len(data) {
			end = len(data)
		}
		if err := c.SendFileChunk(protocol.FileMarkChunk, data[offset:end]); err != nil {
			return err
		}
	}

	// Signal end of transfer.
	return c.SendFileChunk(protocol.FileMarkEnd, nil)
}

func (s *Server) handleFileTransfer(c *Client, payload []byte) {
	r := protocol.PayloadReader(payload)
	mark, err := r.ReadUint8()
	if err != nil {
		return
	}
	data, err := r.ReadBytes()
	if err != nil {
		return
	}
	switch mark {
	case protocol.FileMarkSize:
		slog.Info("incoming file transfer", "from", c.name, "size_str", string(data))
	case protocol.FileMarkChunk:
		slog.Debug("file chunk received", "from", c.name, "bytes", len(data))
		// TODO: accumulate into a temp file
	case protocol.FileMarkEnd:
		slog.Info("file transfer complete", "from", c.name)
		// TODO: move temp file to downloads folder; trigger OS notification
	}
}

func (s *Server) handleDragInfo(c *Client, payload []byte) {
	r := protocol.PayloadReader(payload)
	count, _ := r.ReadUint16()
	paths := make([]string, 0, count)
	for i := 0; i < int(count); i++ {
		p, err := r.ReadString()
		if err != nil {
			break
		}
		paths = append(paths, p)
	}
	slog.Info("drag info received", "from", c.name, "files", paths)
	// TODO: read file bytes from source client and forward via SendFileTo
}

// --------------------------------------------------------------------------
// Keep-alive
// --------------------------------------------------------------------------

func (s *Server) keepAliveLoop(ctx context.Context) {
	ticker := time.NewTicker(protocol.KeepAliveInterval * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			s.mu.RLock()
			for _, c := range s.clients {
				c.SendKeepAlive()
			}
			s.mu.RUnlock()
		case <-ctx.Done():
			return
		}
	}
}

// ConnectedScreens returns names of currently connected secondary screens.
func (s *Server) ConnectedScreens() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	names := make([]string, 0, len(s.clients))
	for n := range s.clients {
		names = append(names, n)
	}
	return names
}
