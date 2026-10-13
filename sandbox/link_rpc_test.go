package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"
)

// linkPair returns a client and a served handler over a real socket pair, so
// the framing, the buffering and the concurrency are the ones the sandbox
// runs.
func linkPair(t *testing.T, h rpcHandler) *rpcClient {
	t.Helper()
	a, b := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go serveRPC(ctx, b, h)
	c := newRPCClient(a)
	t.Cleanup(func() { c.Close() })
	return c
}

func TestTheGuestLinkCarriesHeadersAndPayloadsIntact(t *testing.T) {
	big := make([]byte, 3<<20)
	for i := range big {
		big[i] = byte(i)
	}
	c := linkPair(t, func(ctx context.Context, op string, body json.RawMessage, blobs [][]byte) (any, [][]byte, error) {
		return map[string]any{"op": op, "blobs": len(blobs)}, blobs, nil
	})

	f, err := c.call(context.Background(), "echo", map[string]string{"hello": "world"}, [][]byte{big, nil, []byte("x")})
	if err != nil {
		t.Fatal(err)
	}
	if len(f.blobs) != 3 {
		t.Fatalf("got %d blobs back", len(f.blobs))
	}
	if len(f.blobs[0]) != len(big) {
		t.Fatalf("payload came back %d bytes, sent %d", len(f.blobs[0]), len(big))
	}
	for i, b := range f.blobs[0] {
		if b != big[i] {
			t.Fatalf("payload differs at byte %d", i)
		}
	}
	if len(f.blobs[1]) != 0 || string(f.blobs[2]) != "x" {
		t.Fatalf("blobs came back as %q %q", f.blobs[1], f.blobs[2])
	}
}

// Calls overlap: a build runs for minutes while reads go past it, and the
// mount issues many small reads at once. The answers have to find their own
// callers, and a large frame in flight must not stall the reader that
// delivers them — which it would if writing and delivering took one lock.
func TestOverlappingCallsEachGetTheirOwnAnswer(t *testing.T) {
	c := linkPair(t, func(ctx context.Context, op string, body json.RawMessage, blobs [][]byte) (any, [][]byte, error) {
		var req struct {
			N int `json:"n"`
		}
		json.Unmarshal(body, &req)
		if req.N%7 == 0 {
			// A slow answer in the middle of the others.
			time.Sleep(20 * time.Millisecond)
		}
		return nil, [][]byte{make([]byte, req.N*1024)}, nil
	})

	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for i := 1; i <= 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			f, err := c.call(ctx, "sized", map[string]int{"n": i}, nil)
			if err != nil {
				errs <- err
				return
			}
			if len(f.blobs) != 1 || len(f.blobs[0]) != i*1024 {
				errs <- fmt.Errorf("call %d got %d bytes", i, len(f.blobs[0]))
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

// A guest that dies mid-call must produce an error on the host, not a
// goroutine parked forever.
func TestACallOutlivedByTheLinkFails(t *testing.T) {
	a, b := net.Pipe()
	c := newRPCClient(a)
	done := make(chan error, 1)
	go func() {
		_, err := c.call(context.Background(), "never-answered", nil, nil)
		done <- err
	}()
	time.Sleep(20 * time.Millisecond)
	b.Close()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a call over a dead link succeeded")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a call over a dead link never returned")
	}
}

// A workspace-protection refusal is a decision, not a failure. Crossing the
// link turns errors into text, and one that arrived on the host as a plain
// string would be reported to the model as a broken sandbox instead of a rule
// it should stop retrying.
func TestARefusalSurvivesTheLinkAsARefusal(t *testing.T) {
	c2 := linkPair(t, func(ctx context.Context, op string, body json.RawMessage, blobs [][]byte) (any, [][]byte, error) {
		snap := testProtections(map[string][]string{"read-only": {".git"}})
		return nil, nil, snap.CheckWrite(".git/config")
	})
	_, err := c2.call(context.Background(), "write", nil, nil)
	wantProtected(t, "a refusal over the link", err, "read-only")

	// And an ordinary error stays an ordinary error rather than becoming one.
	c3 := linkPair(t, func(ctx context.Context, op string, body json.RawMessage, blobs [][]byte) (any, [][]byte, error) {
		return nil, nil, errors.New("something went wrong")
	})
	_, err = c3.call(context.Background(), "boom", nil, nil)
	if err == nil || err.Error() != "something went wrong" {
		t.Fatalf("plain error came back as %v", err)
	}
}

// A call past its deadline tells a slow sandbox from one that has stopped:
// it pings, and a link whose other end answers nothing — not even that — is
// failed with ErrStoppedAnswering, for this call and every one after it.
// A wedged sandbox otherwise answered each tool call for half an hour with
// "context deadline exceeded", and the agent kept trying.
func TestALinkThatStopsAnsweringSaysSo(t *testing.T) {
	probeWait = 200 * time.Millisecond
	t.Cleanup(func() { probeWait = 10 * time.Second })

	// Answers nothing at all.
	block := make(chan struct{})
	t.Cleanup(func() { close(block) })
	dead := linkPair(t, func(ctx context.Context, op string, body json.RawMessage, blobs [][]byte) (any, [][]byte, error) {
		<-block
		return nil, nil, nil
	})
	dead.probeOp = "ping"
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := dead.call(ctx, "exec", struct{}{}, nil); !errors.Is(err, ErrStoppedAnswering) {
		t.Fatalf("a call to a sandbox that answers nothing: %v", err)
	}
	start := time.Now()
	if _, err := dead.call(context.Background(), "read", struct{}{}, nil); !errors.Is(err, ErrStoppedAnswering) || time.Since(start) > time.Second {
		t.Fatalf("the next call did not fail at once: %v after %s", err, time.Since(start))
	}

	// Slow at the one thing, answering the ping: not stopped, just slow.
	slow := linkPair(t, func(ctx context.Context, op string, body json.RawMessage, blobs [][]byte) (any, [][]byte, error) {
		if op == "exec" {
			<-block
		}
		return struct{}{}, nil, nil
	})
	slow.probeOp = "ping"
	ctx2, cancel2 := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel2()
	if _, err := slow.call(ctx2, "exec", struct{}{}, nil); !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrStoppedAnswering) {
		t.Fatalf("a slow call to a working sandbox: %v", err)
	}
	if _, err := slow.call(context.Background(), "read", struct{}{}, nil); err != nil {
		t.Fatalf("a working sandbox was failed: %v", err)
	}
}

// A caller that gives up stops the call on the other side too. It used to
// stop only waiting: Esc on a command reported it cancelled while the guest
// ran it to its own deadline.
func TestGivingUpOnACallStopsItOnTheOtherSide(t *testing.T) {
	stopped := make(chan struct{})
	c := linkPair(t, func(ctx context.Context, op string, body json.RawMessage, blobs [][]byte) (any, [][]byte, error) {
		if op == "ping" {
			return nil, nil, nil
		}
		<-ctx.Done()
		close(stopped)
		return nil, nil, ctx.Err()
	})
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	if _, err := c.call(ctx, "exec", struct{}{}, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("call = %v, want cancelled", err)
	}
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("the other side kept running the call after its caller gave up")
	}
	// And the link carries on.
	done := make(chan error, 1)
	go func() { _, err := c.call(context.Background(), "ping", struct{}{}, nil); done <- err }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the link stopped answering after a cancel")
	}
}
