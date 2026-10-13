package sandbox

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"
)

// The guest link. Two streams run between the client on the host and the
// agent inside the sandbox, each carrying requests in one direction only:
//
//   control  host → guest: read, write, delete, exec, enumerate changes
//   lower    guest → host: stat, readdir, read of the host working directory
//
// One bidirectional multiplexer would do both, but two one-way streams are
// much easier to reason about and to test, and the two directions have
// genuinely different shapes: control calls are few and occasionally very
// long-lived (a build), lower-layer calls are many and short.
//
// Frames carry a small JSON header and any number of raw payload blobs. File
// content never goes through JSON: base64 in a header would cost a third of
// every byte read, and the lower layer serves every read of every file in the
// project.

// frame is one message in either direction. Op is set on requests, Err on
// responses; ID correlates the two.
type frame struct {
	ID    uint64          `json:"id"`
	Op    string          `json:"op,omitempty"`
	Err   string          `json:"err,omitempty"`
	Body  json.RawMessage `json:"body,omitempty"`
	blobs [][]byte
}

// cancelOp withdraws a request still running on the other side. Its body names
// the request's ID, and it gets no answer of its own: the request it stops
// answers, with the error stopping it gave. A side that does not know the op
// answers it with an error under ID 0, which no caller is waiting for.
const cancelOp = "$cancel"

// maxFrame caps a single frame. Reads are chunked well below it; the cap is
// here so that a corrupt or hostile length prefix cannot ask the other side
// to allocate the machine.
const maxFrame = 64 << 20

func writeFrame(w *bufio.Writer, f *frame) error {
	hdr, err := json.Marshal(f)
	if err != nil {
		return err
	}
	if err := binary.Write(w, binary.LittleEndian, uint32(len(hdr))); err != nil {
		return err
	}
	if _, err := w.Write(hdr); err != nil {
		return err
	}
	if err := binary.Write(w, binary.LittleEndian, uint32(len(f.blobs))); err != nil {
		return err
	}
	for _, b := range f.blobs {
		if err := binary.Write(w, binary.LittleEndian, uint32(len(b))); err != nil {
			return err
		}
		if _, err := w.Write(b); err != nil {
			return err
		}
	}
	return w.Flush()
}

func readFrame(r *bufio.Reader) (*frame, error) {
	var n uint32
	if err := binary.Read(r, binary.LittleEndian, &n); err != nil {
		return nil, err
	}
	if n > maxFrame {
		return nil, fmt.Errorf("sandbox: frame header of %d bytes", n)
	}
	hdr := make([]byte, n)
	if _, err := io.ReadFull(r, hdr); err != nil {
		return nil, err
	}
	var f frame
	if err := json.Unmarshal(hdr, &f); err != nil {
		return nil, err
	}
	var count uint32
	if err := binary.Read(r, binary.LittleEndian, &count); err != nil {
		return nil, err
	}
	if count > 64 {
		return nil, fmt.Errorf("sandbox: frame with %d blobs", count)
	}
	total := 0
	for range count {
		var sz uint32
		if err := binary.Read(r, binary.LittleEndian, &sz); err != nil {
			return nil, err
		}
		if total += int(sz); sz > maxFrame || total > maxFrame {
			return nil, fmt.Errorf("sandbox: frame payload of %d bytes", total)
		}
		b := make([]byte, sz)
		if _, err := io.ReadFull(r, b); err != nil {
			return nil, err
		}
		f.blobs = append(f.blobs, b)
	}
	return &f, nil
}

// rpcClient issues requests over one stream and matches the answers back to
// their callers by ID — the same correlation pattern the wire uses, for the
// same reason: calls overlap, and a build that runs for four minutes must not
// hold up the read behind it.
type rpcClient struct {
	conn net.Conn

	// Writes take wmu and nothing else. Holding the state lock across the
	// write would deadlock the link under load: a large frame can block in
	// the socket until the peer drains it, the peer only drains while its own
	// writer is not blocked, and a reader here that cannot take the state
	// lock to deliver a response is a reader that has stopped draining.
	wmu sync.Mutex
	w   *bufio.Writer

	mu      sync.Mutex
	next    uint64
	waiting map[uint64]chan *frame
	closed  error

	// probeOp, when set, is an op the other side answers at once. A call
	// whose own deadline passes sends it, and a link that does not answer
	// that either is failed with ErrStoppedAnswering — see deadlineMissed.
	probeOp string
}

// ErrStoppedAnswering is a sandbox that is no longer there to run anything:
// its link closed, or it stopped answering while the link stayed open. Every
// call after it fails at once with this error, and what fixes it is starting
// the sandbox again.
var ErrStoppedAnswering = errors.New("sandbox: the sandbox stopped answering")

// probeWait is how long a stuck link gets to answer the probe.
var probeWait = 10 * time.Second

func newRPCClient(conn net.Conn) *rpcClient {
	c := &rpcClient{conn: conn, w: bufio.NewWriter(conn), waiting: map[uint64]chan *frame{}}
	go c.read()
	return c
}

func (c *rpcClient) read() {
	r := bufio.NewReader(c.conn)
	for {
		f, err := readFrame(r)
		if err != nil {
			c.fail(err)
			return
		}
		c.mu.Lock()
		ch := c.waiting[f.ID]
		delete(c.waiting, f.ID)
		c.mu.Unlock()
		if ch != nil {
			ch <- f
		}
	}
}

// fail wakes every caller still waiting. A guest that died mid-call must
// produce an error on the host rather than a goroutine parked forever.
func (c *rpcClient) fail(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed == nil {
		// Said in words: a bare "EOF" is all the model and the developer
		// were told the last time this happened, and neither could tell that
		// the sandbox was gone rather than the command being odd.
		if errors.Is(err, ErrStoppedAnswering) {
			c.closed = err
		} else {
			c.closed = fmt.Errorf("%w (its link closed: %v)", ErrStoppedAnswering, err)
		}
	}
	for id, ch := range c.waiting {
		delete(c.waiting, id)
		close(ch)
	}
}

// call sends one request and waits for its answer. body is marshalled into
// the header; blobs ride alongside it untouched.
func (c *rpcClient) call(ctx context.Context, op string, body any, blobs [][]byte) (*frame, error) {
	f, err := c.do(ctx, op, body, blobs)
	if err != nil && errors.Is(err, context.DeadlineExceeded) && c.probeOp != "" && op != c.probeOp {
		if dead := c.deadlineMissed(); dead != nil {
			return nil, dead
		}
	}
	return f, err
}

// deadlineMissed tells a slow answer from no answer. A call past its deadline
// is usually a command that ran long; the probe is answered at once by a
// sandbox that is working, and one that does not answer it in probeWait is
// not working at all. Then the link is failed: this call and every one
// waiting say so now, rather than each running out its own deadline — a
// wedged sandbox otherwise answered every tool call for half an hour with
// "context deadline exceeded", and the agent kept trying.
func (c *rpcClient) deadlineMissed() error {
	ctx, cancel := context.WithTimeout(context.Background(), probeWait)
	defer cancel()
	if _, err := c.do(ctx, c.probeOp, struct{}{}, nil); err == nil || !errors.Is(err, context.DeadlineExceeded) {
		return nil // it answered, or failed for a reason of its own
	}
	c.fail(fmt.Errorf("%w (it did not answer a ping in %s)", ErrStoppedAnswering, probeWait))
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

func (c *rpcClient) do(ctx context.Context, op string, body any, blobs [][]byte) (*frame, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	ch := make(chan *frame, 1)

	c.mu.Lock()
	if c.closed != nil {
		err := c.closed
		c.mu.Unlock()
		return nil, err
	}
	c.next++
	id := c.next
	c.waiting[id] = ch
	c.mu.Unlock()

	// The ID is claimed before the frame goes out, so an answer that arrives
	// while this goroutine is still in the write has somewhere to land.
	c.wmu.Lock()
	err = writeFrame(c.w, &frame{ID: id, Op: op, Body: raw, blobs: blobs})
	c.wmu.Unlock()
	if err != nil {
		c.fail(err)
		return nil, err
	}

	select {
	case <-ctx.Done():
		// The caller has given up. The ID stays claimed until the answer
		// arrives or the link dies, so a late answer is discarded rather
		// than delivered to whoever gets that ID next.
		c.mu.Lock()
		delete(c.waiting, id)
		c.mu.Unlock()
		// And the other side is told, so what was asked for stops there too
		// rather than running to its own deadline — a command interrupted
		// with Esc kept running in the sandbox. Sent from a goroutine of its
		// own: a write can block until the peer drains, and the caller has
		// already been kept waiting long enough.
		go func() {
			raw, _ := json.Marshal(struct {
				ID uint64 `json:"id"`
			}{id})
			c.wmu.Lock()
			writeFrame(c.w, &frame{Op: cancelOp, Body: raw})
			c.wmu.Unlock()
		}()
		return nil, ctx.Err()
	case f, ok := <-ch:
		if !ok {
			c.mu.Lock()
			err := c.closed
			c.mu.Unlock()
			if err == nil {
				err = errors.New("sandbox: guest link closed")
			}
			return nil, err
		}
		if f.Err != "" {
			return nil, decodeRemoteError(f.Err)
		}
		return f, nil
	}
}

// callJSON is call for the common case: a request and a decoded answer.
func (c *rpcClient) callJSON(ctx context.Context, op string, body, out any) error {
	f, err := c.call(ctx, op, body, nil)
	if err != nil {
		return err
	}
	if out == nil || len(f.Body) == 0 {
		return nil
	}
	return json.Unmarshal(f.Body, out)
}

func (c *rpcClient) Close() error { return c.conn.Close() }

// rpcHandler answers requests arriving on a stream. It returns the response
// body and any payload blobs; an error becomes the response's Err.
type rpcHandler func(ctx context.Context, op string, body json.RawMessage, blobs [][]byte) (any, [][]byte, error)

// serveRPC reads requests from conn and answers them, each in its own
// goroutine so a long call does not block the ones behind it. Writes are
// serialized; a response may therefore arrive out of order, which is what the
// ID is for.
func serveRPC(ctx context.Context, conn net.Conn, h rpcHandler) error {
	defer conn.Close()
	var wmu sync.Mutex
	w := bufio.NewWriter(conn)
	r := bufio.NewReader(conn)

	var wg sync.WaitGroup
	defer wg.Wait()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Each request runs under a context of its own, so a caller that gives up
	// can stop it here too; see cancelOp.
	var cmu sync.Mutex
	running := map[uint64]context.CancelFunc{}

	for {
		req, err := readFrame(r)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		if req.Op == cancelOp {
			var c struct {
				ID uint64 `json:"id"`
			}
			if json.Unmarshal(req.Body, &c) == nil {
				cmu.Lock()
				if stop := running[c.ID]; stop != nil {
					stop()
				}
				cmu.Unlock()
			}
			continue // answered by the cancelled call's own response
		}
		reqCtx, stop := context.WithCancel(ctx)
		cmu.Lock()
		running[req.ID] = stop
		cmu.Unlock()
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() {
				cmu.Lock()
				delete(running, req.ID)
				cmu.Unlock()
				stop()
			}()
			body, blobs, err := h(reqCtx, req.Op, req.Body, req.blobs)
			resp := &frame{ID: req.ID, blobs: blobs}
			if err != nil {
				resp.Err = encodeRemoteError(err)
			} else if body != nil {
				if raw, merr := json.Marshal(body); merr == nil {
					resp.Body = raw
				} else {
					resp.Err = encodeRemoteError(merr)
					resp.blobs = nil
				}
			}
			wmu.Lock()
			writeFrame(w, resp)
			wmu.Unlock()
		}()
	}
}

// jsonMarshal is json.Marshal, named so the guest configuration can be handed
// to a subprocess without importing encoding/json at every call site.
func jsonMarshal(v any) ([]byte, error) { return json.Marshal(v) }

// jsonUnmarshal is json.Unmarshal, for the same reason as jsonMarshal above.
func jsonUnmarshal(data []byte, v any) error { return json.Unmarshal(data, v) }
