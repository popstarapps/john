package protocol

import (
	"context"
	"errors"
	"sync"
	"time"
)

// Transport carries whole frames. The production transport is a WebSocket
// (one JSON message per text frame); tests use an in-memory pipe. The Conn
// assumes frames arrive in the order they were written — both of those
// deliver that — and builds resumability on top.
type Transport interface {
	// ReadFrame returns the next frame, blocking until one arrives, the
	// context ends, or the transport fails.
	ReadFrame(ctx context.Context) ([]byte, error)
	// WriteFrame writes one frame.
	WriteFrame(ctx context.Context, data []byte) error
	// Close releases the transport. Pending reads return an error.
	Close() error
}

var (
	// ErrClosed is returned by Send after Close.
	ErrClosed = errors.New("protocol: connection closed")
	// ErrStale means the replay buffer overflowed while the peer was away;
	// the messages needed to resume are gone and the session must be
	// reopened. This is what session.stale reports.
	ErrStale = errors.New("protocol: replay buffer overflowed, session not resumable")
)

// Options tune a Conn. The zero value gets sane defaults.
type Options struct {
	// MaxBuffered and MaxBufferedBytes bound the replay buffer of sent,
	// unacknowledged frames. Defaults: 1000 frames, 8 MiB.
	MaxBuffered      int
	MaxBufferedBytes int
	// AckThreshold is how many delivered frames may go unacknowledged
	// before a bare ack is pushed even with no outgoing traffic. Default 32.
	AckThreshold int
	// AckInterval is the idle deadline for the same. Default 5s.
	AckInterval time.Duration
	// OnDetach, when set, is called once each time the attached transport
	// fails. The Conn is parked, not closed: Attach resumes it.
	OnDetach func(error)
}

func (o Options) withDefaults() Options {
	if o.MaxBuffered == 0 {
		o.MaxBuffered = 1000
	}
	if o.MaxBufferedBytes == 0 {
		o.MaxBufferedBytes = 8 << 20
	}
	if o.AckThreshold == 0 {
		o.AckThreshold = 32
	}
	if o.AckInterval == 0 {
		o.AckInterval = 5 * time.Second
	}
	return o
}

type sentFrame struct {
	seq  uint64
	data []byte
}

// Conn is one side of a session: it numbers outgoing messages, buffers them
// until acknowledged, deduplicates and orders incoming ones, and survives the
// transport dying by replaying from the peer's last acknowledgement when a
// new transport is attached. Both sides of the wire run the same Conn,
// because either side can be the one that missed something.
//
// Handshake (session.open / session.resume / session.ready / session.stale)
// happens before a Conn is attached, as raw frames via EncodeRaw/DecodeRaw;
// everything after is numbered and resumable.
type Conn struct {
	opts Options

	mu       sync.Mutex
	tr       Transport
	gen      int // attach generation; stale readers see a newer one and stand down
	sendSeq  uint64
	recvSeq  uint64
	peerAck  uint64
	lastAck  uint64 // highest recvSeq we have told the peer about
	buf      []sentFrame
	bufBytes int
	stale    bool
	closed   bool

	in     chan Message
	closec chan struct{}
}

// NewConn creates a detached Conn. Attach a transport to start exchanging.
func NewConn(opts Options) *Conn {
	return &Conn{
		opts:   opts.withDefaults(),
		in:     make(chan Message, 256),
		closec: make(chan struct{}),
	}
}

// In delivers incoming messages in order. The channel is closed by Close.
func (c *Conn) In() <-chan Message { return c.in }

// RecvSeq is the highest contiguous seq received from the peer — the value a
// resume handshake reports as its ack.
func (c *Conn) RecvSeq() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.recvSeq
}

// Stale reports whether the replay buffer overflowed while detached, which
// makes the session unresumable.
func (c *Conn) Stale() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stale
}

// Send numbers, buffers, and transmits one message. It succeeds even while
// detached — the message is buffered for replay on the next Attach. It fails
// only when the Conn is closed or the buffer has overflowed (ErrStale).
func (c *Conn) Send(ctx context.Context, m Message) error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return ErrClosed
	}
	if c.stale {
		c.mu.Unlock()
		return ErrStale
	}
	if len(c.buf) >= c.opts.MaxBuffered || c.bufBytes >= c.opts.MaxBufferedBytes {
		c.stale = true
		c.mu.Unlock()
		return ErrStale
	}
	c.sendSeq++
	seq := c.sendSeq
	ack := c.recvSeq
	data, err := encodeFrame(m, seq, ack)
	if err != nil {
		c.sendSeq--
		c.mu.Unlock()
		return err
	}
	c.buf = append(c.buf, sentFrame{seq: seq, data: data})
	c.bufBytes += len(data)
	c.lastAck = ack
	tr := c.tr
	c.mu.Unlock()

	if tr != nil {
		// A write error is not a Send error: the frame is buffered, the
		// reader will notice the dead transport, and replay covers it.
		_ = tr.WriteFrame(ctx, data)
	}
	return nil
}

// Attach connects a transport and resumes: frames the peer has acknowledged
// (peerAck, from the resume handshake) are dropped, the rest are re-sent in
// order, and reading begins. Attaching to a stale Conn returns ErrStale.
func (c *Conn) Attach(tr Transport, peerAck uint64) error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return ErrClosed
	}
	if c.stale {
		c.mu.Unlock()
		return ErrStale
	}
	if c.tr != nil {
		c.tr.Close()
	}
	c.pruneLocked(peerAck)
	c.tr = tr
	c.gen++
	gen := c.gen
	replay := make([][]byte, len(c.buf))
	for i, f := range c.buf {
		replay[i] = f.data
	}
	c.mu.Unlock()

	ctx := context.Background()
	for _, data := range replay {
		if err := tr.WriteFrame(ctx, data); err != nil {
			c.detach(gen, err)
			return nil // parked again; the next Attach retries
		}
	}

	go c.readLoop(tr, gen)
	go c.ackLoop(tr, gen)
	return nil
}

// Detach parks the Conn deliberately: the transport is closed, state is kept,
// and a later Attach resumes.
func (c *Conn) Detach() {
	c.mu.Lock()
	tr := c.tr
	c.tr = nil
	c.gen++
	c.mu.Unlock()
	if tr != nil {
		tr.Close()
	}
}

// Close ends the Conn for good and closes the In channel.
func (c *Conn) Close() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	tr := c.tr
	c.tr = nil
	c.gen++
	c.mu.Unlock()
	if tr != nil {
		tr.Close()
	}
	close(c.closec)
	close(c.in)
}

func (c *Conn) pruneLocked(ack uint64) {
	if ack > c.peerAck {
		c.peerAck = ack
	}
	i := 0
	for i < len(c.buf) && c.buf[i].seq <= c.peerAck {
		c.bufBytes -= len(c.buf[i].data)
		i++
	}
	c.buf = c.buf[i:]
}

func (c *Conn) detach(gen int, err error) {
	c.mu.Lock()
	if c.closed || c.gen != gen {
		c.mu.Unlock()
		return
	}
	tr := c.tr
	c.tr = nil
	c.gen++
	cb := c.opts.OnDetach
	c.mu.Unlock()
	if tr != nil {
		tr.Close()
	}
	if cb != nil {
		cb(err)
	}
}

func (c *Conn) readLoop(tr Transport, gen int) {
	ctx := context.Background()
	for {
		data, err := tr.ReadFrame(ctx)
		if err != nil {
			c.detach(gen, err)
			return
		}
		h, m, err := decodeFrame(data)
		unknown := false
		if err != nil {
			var u ErrUnknownMessage
			if errors.As(err, &u) {
				// A newer peer can say more than this build understands.
				// The frame still occupies its seq and carries its ack.
				unknown = true
			} else {
				c.detach(gen, err)
				return
			}
		}

		c.mu.Lock()
		if c.closed || c.gen != gen {
			c.mu.Unlock()
			return
		}
		c.pruneLocked(h.Ack)

		deliver := false
		switch {
		case h.Seq == 0:
			// Bare ack or other unnumbered control: ack already handled.
		case h.Seq <= c.recvSeq:
			// Replay overlap after resume; already delivered.
		case h.Seq == c.recvSeq+1:
			c.recvSeq++
			deliver = !unknown
		default:
			// A gap on an ordered transport is a protocol violation.
			c.mu.Unlock()
			c.detach(gen, errors.New("protocol: sequence gap"))
			return
		}
		needAck := c.recvSeq-c.lastAck >= uint64(c.opts.AckThreshold)
		c.mu.Unlock()

		if deliver {
			if _, isAck := m.(*Ack); !isAck {
				select {
				case c.in <- m:
				case <-c.closec:
					return
				}
			}
		}
		if needAck {
			c.sendAck(tr)
		}
	}
}

// ackLoop pushes a bare ack when this direction has been silent while the
// other kept talking; without it a one-sided conversation would never let the
// peer trim its replay buffer.
func (c *Conn) ackLoop(tr Transport, gen int) {
	t := time.NewTicker(c.opts.AckInterval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
		case <-c.closec:
			return
		}
		c.mu.Lock()
		live := !c.closed && c.gen == gen
		pending := c.recvSeq > c.lastAck
		c.mu.Unlock()
		if !live {
			return
		}
		if pending {
			c.sendAck(tr)
		}
	}
}

func (c *Conn) sendAck(tr Transport) {
	c.mu.Lock()
	ack := c.recvSeq
	c.lastAck = ack
	c.mu.Unlock()
	data, err := encodeFrame(Ack{}, 0, ack)
	if err != nil {
		return
	}
	_ = tr.WriteFrame(context.Background(), data)
}

// EncodeRaw encodes an unnumbered frame, for the handshake that happens
// before a Conn is attached.
func EncodeRaw(m Message) ([]byte, error) { return encodeFrame(m, 0, 0) }

// DecodeRaw decodes a frame into its typed message, ignoring numbering.
func DecodeRaw(data []byte) (Message, error) {
	_, m, err := decodeFrame(data)
	return m, err
}
