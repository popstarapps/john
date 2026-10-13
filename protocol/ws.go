package protocol

import (
	"context"

	"github.com/coder/websocket"
)

// WSTransport adapts a WebSocket to the Conn's Transport: one protocol
// message per text frame. Both sides of the wire use it.
type WSTransport struct {
	c *websocket.Conn
}

// NewWSTransport wraps an accepted or dialed connection.
func NewWSTransport(c *websocket.Conn) *WSTransport {
	// The replay buffer bounds what a frame can carry; the websocket layer
	// should not be the thing that decides a big tool result is too big.
	c.SetReadLimit(16 << 20)
	return &WSTransport{c: c}
}

func (t *WSTransport) ReadFrame(ctx context.Context) ([]byte, error) {
	_, data, err := t.c.Read(ctx)
	return data, err
}

func (t *WSTransport) WriteFrame(ctx context.Context, data []byte) error {
	return t.c.Write(ctx, websocket.MessageText, data)
}

func (t *WSTransport) Close() error {
	return t.c.Close(websocket.StatusNormalClosure, "")
}

// Ping round-trips a websocket ping. The client uses it as its heartbeat: a
// backend that stops answering is a backend that is gone, and john fails
// closed the moment that is known rather than the moment a write happens to
// fail.
func (t *WSTransport) Ping(ctx context.Context) error {
	return t.c.Ping(ctx)
}
