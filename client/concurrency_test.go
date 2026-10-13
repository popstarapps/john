//go:build !windows

package client

import (
	"encoding/json"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oliverkofoed/gokit/agentkit/llm"
	"github.com/oliverkofoed/gokit/agentkit/llm/llmtest"
)

// The negotiated tool-call limit is a limit rather than a note.
//
// A turn calls tools in batches — read four files at once — and the loop runs
// a batch of reads concurrently (a batch with a command or an edit in it runs
// in order, one call at a time). Both ends state on open how many may be in
// flight at once and the smaller number governs, and the client is the side
// that has to hold it: it owns the machine the calls run in, and a limit kept
// only by the side asking for the work is not one.
//
// The reads here block until the test lets them go, so what is asserted is
// what was actually running at the same moment and not how fast the machine
// happened to be.
func TestNoMoreToolCallsRunAtOnceThanBothEndsAgreedOn(t *testing.T) {
	want := clientLimits.MaxConcurrentTools
	const calls = 8

	blocks := make([]llm.Block, 0, calls)
	for i := range calls {
		args, err := json.Marshal(map[string]any{"path": fmt.Sprintf("pkg%d/main.go", i)})
		if err != nil {
			t.Fatal(err)
		}
		blocks = append(blocks, llm.Block{Type: llm.BlockToolCall, Name: "read", Args: args})
	}
	s := newE2E(t,
		llmtest.Blocks(llm.StopToolUse, blocks...),
		llmtest.Text("All eight finished."),
	)

	var inflight, peak, started atomic.Int32
	release := make(chan struct{})
	s.sb.ReadFn = func(string) {
		started.Add(1)
		n := inflight.Add(1)
		for {
			high := peak.Load()
			if n <= high || peak.CompareAndSwap(high, n) {
				break
			}
		}
		<-release
		inflight.Add(-1)
	}

	s.type_("read every package's main file\r")

	// The batch fills the slots it is allowed and then stops.
	deadline := time.Now().Add(8 * time.Second * timeScale)
	for inflight.Load() < int32(want) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := inflight.Load(); got != int32(want) {
		t.Fatalf("%d tool calls running, want the negotiated %d", got, want)
	}

	// And it stays stopped while they are held. This is the assertion: the
	// other four calls are on the wire and answered by nothing until one of
	// these comes back.
	time.Sleep(500 * time.Millisecond * timeScale)
	if got := started.Load(); got != int32(want) {
		t.Fatalf("%d tool calls started while %d were still running", got, want)
	}

	close(release)
	s.wait("All eight finished.")
	if got := started.Load(); got != calls {
		t.Fatalf("%d of %d tool calls ran", got, calls)
	}
	if got := peak.Load(); got > int32(want) {
		t.Fatalf("%d tool calls ran at once, over the negotiated %d", got, want)
	}
}
