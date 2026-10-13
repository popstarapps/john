package protocol

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// pipeTransport is an in-memory Transport. A pair shares two buffered
// channels; closing either end fails both, and frames sitting in the buffer
// at close are lost — which is exactly what a dying TCP connection does to
// frames in flight, and what the resume machinery must survive.
type pipeTransport struct {
	in     chan []byte
	out    chan []byte
	closed chan struct{}
	once   *sync.Once // shared: either side's Close kills the pair
}

func newPipePair() (*pipeTransport, *pipeTransport) {
	ab := make(chan []byte, 1024)
	ba := make(chan []byte, 1024)
	closed := make(chan struct{})
	once := &sync.Once{}
	a := &pipeTransport{in: ba, out: ab, closed: closed, once: once}
	b := &pipeTransport{in: ab, out: ba, closed: closed, once: once}
	return a, b
}

var errPipeClosed = errors.New("pipe closed")

func (p *pipeTransport) ReadFrame(ctx context.Context) ([]byte, error) {
	select {
	case data := <-p.in:
		return data, nil
	case <-p.closed:
		return nil, errPipeClosed
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (p *pipeTransport) WriteFrame(ctx context.Context, data []byte) error {
	select {
	case p.out <- data:
		return nil
	case <-p.closed:
		return errPipeClosed
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (p *pipeTransport) Close() error {
	p.once.Do(func() { close(p.closed) })
	return nil
}

// collect drains a Conn's In channel into a slice.
func collect(c *Conn) (get func() []Message) {
	var mu sync.Mutex
	var got []Message
	go func() {
		for m := range c.In() {
			mu.Lock()
			got = append(got, m)
			mu.Unlock()
		}
	}()
	return func() []Message {
		mu.Lock()
		defer mu.Unlock()
		return append([]Message(nil), got...)
	}
}

func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestFrameRoundTrip(t *testing.T) {
	in := &Prompt{Turn: "t1", Text: "fix the retry in billing"}
	data, err := encodeFrame(in, 41, 17)
	if err != nil {
		t.Fatal(err)
	}
	h, m, err := decodeFrame(data)
	if err != nil {
		t.Fatal(err)
	}
	if h.T != "prompt" || h.Seq != 41 || h.Ack != 17 {
		t.Fatalf("header = %+v", h)
	}
	out, ok := m.(*Prompt)
	if !ok {
		t.Fatalf("decoded %T", m)
	}
	if out.Turn != in.Turn || out.Text != in.Text {
		t.Fatalf("round trip lost fields: %+v", out)
	}
}

// Every registered discriminator must decode to a type whose T() returns the
// same discriminator — a copy-paste error in the registry would otherwise
// silently misroute messages.
func TestDecoderRegistryIsConsistent(t *testing.T) {
	for name, mk := range decoders {
		if got := mk().T(); got != name {
			t.Errorf("decoders[%q] constructs a message with T() = %q", name, got)
		}
	}
}

func TestUnknownMessageIsSkippedButCounted(t *testing.T) {
	a, b := newPipePair()
	defer a.Close()

	cb := NewConn(Options{})
	cb.Attach(b, 0)
	defer cb.Close()
	got := collect(cb)

	ctx := context.Background()
	// A frame from a newer peer: unknown discriminator, valid seq.
	if err := a.WriteFrame(ctx, []byte(`{"t":"hologram.begin","seq":1,"ack":0}`)); err != nil {
		t.Fatal(err)
	}
	data, _ := encodeFrame(&Notice{Level: "info", Text: "hello"}, 2, 0)
	if err := a.WriteFrame(ctx, data); err != nil {
		t.Fatal(err)
	}

	waitFor(t, func() bool { return len(got()) == 1 }, "the known message")
	if n, ok := got()[0].(*Notice); !ok || n.Text != "hello" {
		t.Fatalf("got %#v", got()[0])
	}
	if cb.RecvSeq() != 2 {
		t.Fatalf("recvSeq = %d, want 2 (unknown frame still occupies its seq)", cb.RecvSeq())
	}
}

func TestOrderedDeliveryBothDirections(t *testing.T) {
	ta, tb := newPipePair()
	ca := NewConn(Options{})
	cb := NewConn(Options{})
	ca.Attach(ta, 0)
	cb.Attach(tb, 0)
	defer ca.Close()
	defer cb.Close()
	gotA, gotB := collect(ca), collect(cb)

	ctx := context.Background()
	const n = 50
	for i := 0; i < n; i++ {
		if err := ca.Send(ctx, &MessageDelta{ID: "m1", Text: fmt.Sprintf("a%d", i)}); err != nil {
			t.Fatal(err)
		}
		if err := cb.Send(ctx, &ToolResult{ID: fmt.Sprintf("c%d", i), Status: "ok"}); err != nil {
			t.Fatal(err)
		}
	}

	waitFor(t, func() bool { return len(gotB()) == n && len(gotA()) == n }, "all messages")
	for i, m := range gotB() {
		if d := m.(*MessageDelta); d.Text != fmt.Sprintf("a%d", i) {
			t.Fatalf("B[%d] = %q, out of order", i, d.Text)
		}
	}
	for i, m := range gotA() {
		if r := m.(*ToolResult); r.ID != fmt.Sprintf("c%d", i) {
			t.Fatalf("A[%d] = %q, out of order", i, r.ID)
		}
	}
}

func TestAcksPruneReplayBuffer(t *testing.T) {
	ta, tb := newPipePair()
	ca := NewConn(Options{AckInterval: 20 * time.Millisecond})
	cb := NewConn(Options{AckInterval: 20 * time.Millisecond})
	ca.Attach(ta, 0)
	cb.Attach(tb, 0)
	defer ca.Close()
	defer cb.Close()
	collect(ca)
	gotB := collect(cb)

	ctx := context.Background()
	for i := 0; i < 10; i++ {
		if err := ca.Send(ctx, &Notice{Level: "info", Text: "n"}); err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, func() bool { return len(gotB()) == 10 }, "delivery")

	// B says nothing, so only the idle ack can advance A's buffer.
	waitFor(t, func() bool {
		ca.mu.Lock()
		defer ca.mu.Unlock()
		return len(ca.buf) == 0
	}, "replay buffer to drain via bare acks")
}

// The load-bearing test: the transport dies with messages in flight in both
// directions, a new transport attaches with each side's ack from the
// handshake, and everything arrives exactly once, in order.
func TestResumeAfterTransportLossWithInFlightBothDirections(t *testing.T) {
	ta, tb := newPipePair()
	ca := NewConn(Options{AckInterval: time.Hour}) // no idle acks: maximize loss
	cb := NewConn(Options{AckInterval: time.Hour})
	ca.Attach(ta, 0)
	cb.Attach(tb, 0)
	defer ca.Close()
	defer cb.Close()
	gotA, gotB := collect(ca), collect(cb)

	ctx := context.Background()
	// Established traffic both ways.
	for i := 0; i < 5; i++ {
		ca.Send(ctx, &MessageDelta{ID: "m", Text: fmt.Sprintf("pre%d", i)})
		cb.Send(ctx, &ToolResult{ID: fmt.Sprintf("pre%d", i), Status: "ok"})
	}
	waitFor(t, func() bool { return len(gotB()) == 5 && len(gotA()) == 5 }, "pre-loss delivery")

	// Kill the transport, then send into the void from both sides — a
	// backend that issued a tool.call and a client that answered one, both
	// mid-air when the wifi dropped.
	ta.Close()
	ca.Send(ctx, &ToolCall{ID: "c9", Tool: "run"})
	cb.Send(ctx, &ToolResult{ID: "c8", Status: "failed", Exit: 1})
	ca.Send(ctx, &MessageDelta{ID: "m", Text: "post"})

	// Reconnect: the handshake above the Conn exchanges each side's
	// RecvSeq; everything unacknowledged past it replays.
	na, nb := newPipePair()
	if err := ca.Attach(na, cb.RecvSeq()); err != nil {
		t.Fatal(err)
	}
	if err := cb.Attach(nb, ca.RecvSeq()); err != nil {
		t.Fatal(err)
	}

	waitFor(t, func() bool { return len(gotB()) == 7 && len(gotA()) == 6 }, "post-resume delivery")

	msgsB := gotB()
	if c, ok := msgsB[5].(*ToolCall); !ok || c.ID != "c9" {
		t.Fatalf("B[5] = %#v, want the in-flight tool.call", msgsB[5])
	}
	if d, ok := msgsB[6].(*MessageDelta); !ok || d.Text != "post" {
		t.Fatalf("B[6] = %#v", msgsB[6])
	}
	msgsA := gotA()
	if r, ok := msgsA[5].(*ToolResult); !ok || r.ID != "c8" {
		t.Fatalf("A[5] = %#v, want the in-flight tool.result", msgsA[5])
	}

	// Exactly once: nothing delivered twice despite the replay overlap.
	seen := map[string]int{}
	for _, m := range msgsB {
		seen[fmt.Sprintf("%#v", m)]++
	}
	for k, n := range seen {
		if n > 1 {
			t.Fatalf("delivered %d times: %s", n, k)
		}
	}
}

func TestReplayBufferOverflowMakesSessionStale(t *testing.T) {
	c := NewConn(Options{MaxBuffered: 5}) // detached: nothing ever acked
	ctx := context.Background()
	var err error
	for i := 0; i < 10; i++ {
		err = c.Send(ctx, &Notice{Level: "info", Text: "n"})
		if err != nil {
			break
		}
	}
	if !errors.Is(err, ErrStale) {
		t.Fatalf("err = %v, want ErrStale", err)
	}
	if !c.Stale() {
		t.Fatal("conn should be stale")
	}
	ta, _ := newPipePair()
	if err := c.Attach(ta, 0); !errors.Is(err, ErrStale) {
		t.Fatalf("Attach on stale conn = %v, want ErrStale", err)
	}
}

func TestDetachedSendsAreBufferedAndDeliveredOnAttach(t *testing.T) {
	ca := NewConn(Options{})
	cb := NewConn(Options{})
	defer ca.Close()
	defer cb.Close()
	gotB := collect(cb)

	ctx := context.Background()
	// Send while parked — nothing attached yet.
	for i := 0; i < 3; i++ {
		if err := ca.Send(ctx, &Notice{Level: "info", Text: fmt.Sprintf("parked%d", i)}); err != nil {
			t.Fatal(err)
		}
	}
	ta, tb := newPipePair()
	ca.Attach(ta, 0)
	cb.Attach(tb, 0)

	waitFor(t, func() bool { return len(gotB()) == 3 }, "buffered delivery")
	for i, m := range gotB() {
		if n := m.(*Notice); n.Text != fmt.Sprintf("parked%d", i) {
			t.Fatalf("B[%d] = %q", i, n.Text)
		}
	}
}

func TestOnDetachFiresWhenTransportDies(t *testing.T) {
	detached := make(chan error, 1)
	ta, tb := newPipePair()
	ca := NewConn(Options{OnDetach: func(err error) { detached <- err }})
	cb := NewConn(Options{})
	ca.Attach(ta, 0)
	cb.Attach(tb, 0)
	defer ca.Close()
	defer cb.Close()

	tb.Close()
	select {
	case err := <-detached:
		if err == nil {
			t.Fatal("detach error is nil")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("OnDetach never fired")
	}
}
