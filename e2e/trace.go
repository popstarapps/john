package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"filippo.io/age"

	"github.com/popstarapps/john/trace"
)

// traces reads the sessions the standalone backend recorded under this run's
// JOHN_HOME. It is how the harness knows what happened rather than what was
// drawn: a turn is over when its turn.end is written, a command succeeded when
// its tool.result says so. The keys to open them are in the same home — the
// backend's and the developer's both — because the run made them.
type traces struct {
	home       string
	identities []age.Identity
}

func openTraces(home string) *traces {
	t := &traces{home: home}
	for _, p := range []string{
		filepath.Join(home, "standalone", "trace-identity.txt"),
		filepath.Join(home, "trace-identity.txt"),
	} {
		f, err := os.Open(p)
		if err != nil {
			continue
		}
		ids, err := age.ParseIdentities(f)
		f.Close()
		if err == nil {
			t.identities = append(t.identities, ids...)
		}
	}
	return t
}

// all returns every record of every session, oldest session first. The run
// owns the store, so every session in it is one of the run's.
func (t *traces) all() []trace.Record {
	files, _ := filepath.Glob(filepath.Join(t.home, "standalone", "traces", "*", "*.jsonl"))
	sort.Slice(files, func(i, j int) bool {
		a, _ := os.Stat(files[i])
		b, _ := os.Stat(files[j])
		return a != nil && b != nil && a.ModTime().Before(b.ModTime())
	})
	var out []trace.Record
	for _, path := range files {
		for _, id := range t.identities {
			f, err := os.Open(path)
			if err != nil {
				break
			}
			recs, err := trace.Records(f, id)
			f.Close()
			if err == nil {
				out = append(out, recs...)
				break
			}
		}
	}
	return out
}

func count(recs []trace.Record, kind string) int {
	n := 0
	for _, r := range recs {
		if r.Kind == kind {
			n++
		}
	}
	return n
}

// toolRun is one tool call with its result.
type toolRun struct {
	Tool   string
	Cmd    string // run's command, or the path for a file tool
	Exit   int
	Status string
	Stdout string
}

func toolRuns(recs []trace.Record) []toolRun {
	calls := map[string]*toolRun{}
	var order []*toolRun
	for _, r := range recs {
		switch r.Kind {
		case "tool.call":
			var p struct {
				ID   string `json:"id"`
				Tool string `json:"tool"`
				Args struct {
					Cmd  string `json:"cmd"`
					Path string `json:"path"`
				} `json:"args"`
			}
			if json.Unmarshal(r.Payload, &p) != nil {
				continue
			}
			tr := &toolRun{Tool: p.Tool, Cmd: p.Args.Cmd, Status: "no result"}
			if tr.Cmd == "" {
				tr.Cmd = p.Args.Path
			}
			calls[p.ID] = tr
			order = append(order, tr)
		case "tool.result":
			var p struct {
				ID     string `json:"id"`
				Exit   int    `json:"exit"`
				Status string `json:"status"`
				Stdout string `json:"stdout"`
			}
			if json.Unmarshal(r.Payload, &p) != nil {
				continue
			}
			if tr := calls[p.ID]; tr != nil {
				tr.Exit, tr.Status, tr.Stdout = p.Exit, p.Status, p.Stdout
			}
		}
	}
	out := make([]toolRun, len(order))
	for i, tr := range order {
		out[i] = *tr
	}
	return out
}

// ranOK reports whether any run command containing one of needles succeeded.
func ranOK(runs []toolRun, needles ...string) bool {
	for _, r := range runs {
		if r.Tool != "run" || r.Exit != 0 || r.Status == "failed" || r.Status == "no result" {
			continue
		}
		for _, n := range needles {
			if strings.Contains(r.Cmd, n) {
				return true
			}
		}
	}
	return false
}

// writeDecoded writes every record as one line of JSON, for the reviewer and
// for whoever opens the run directory next.
func writeDecoded(recs []trace.Record, path string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	for _, r := range recs {
		if err := enc.Encode(r); err != nil {
			return err
		}
	}
	return nil
}

// promptRec is a prompt as the backend received it: which session, which
// turn, and the exact text.
type promptRec struct {
	Session, Turn, Text string
}

func prompts(recs []trace.Record) []promptRec {
	var out []promptRec
	for _, r := range recs {
		if r.Kind != "prompt" {
			continue
		}
		var p struct{ Turn, Text string }
		if json.Unmarshal(r.Payload, &p) == nil {
			out = append(out, promptRec{Session: r.Session, Turn: p.Turn, Text: p.Text})
		}
	}
	return out
}

// turnEnd is how a turn finished: completed, interrupted, failed, lost.
type turnEnd struct {
	Session, Turn, Reason string
}

func turnEnds(recs []trace.Record) []turnEnd {
	var out []turnEnd
	for _, r := range recs {
		if r.Kind != "turn.end" {
			continue
		}
		var p struct{ Turn, Reason string }
		if json.Unmarshal(r.Payload, &p) == nil {
			out = append(out, turnEnd{Session: r.Session, Turn: p.Turn, Reason: p.Reason})
		}
	}
	return out
}

// turnNumber is the n of a turn id "tn", or 0.
func turnNumber(turn string) int {
	n := 0
	fmt.Sscanf(turn, "t%d", &n)
	return n
}

// replies is everything the model said in records from index from on.
func replies(recs []trace.Record, from int) string {
	var b strings.Builder
	for _, r := range recs[min(from, len(recs)):] {
		if r.Kind != "assistant.message" {
			continue
		}
		var p struct{ Text string }
		if json.Unmarshal(r.Payload, &p) == nil {
			b.WriteString(p.Text + "\n")
		}
	}
	return b.String()
}

// proxyRec is one decision the proxy reported.
type proxyRec struct {
	URL     string `json:"url"`
	Verdict string `json:"verdict"`
	Policy  string `json:"policy"`
	Allowed bool   `json:"allowed"`
	Error   string `json:"error"`
}

func proxyDecisions(recs []trace.Record) []proxyRec {
	var out []proxyRec
	for _, r := range recs {
		if r.Kind != "proxy.decision" {
			continue
		}
		var p proxyRec
		if json.Unmarshal(r.Payload, &p) == nil {
			out = append(out, p)
		}
	}
	return out
}

// payloadsOf is the payload of every record of one kind, for the records a
// step only looks for by name: model.narrow, model.choice.
func payloadsOf(recs []trace.Record, kind string) []map[string]any {
	var out []map[string]any
	for _, r := range recs {
		if r.Kind != kind {
			continue
		}
		var p map[string]any
		if json.Unmarshal(r.Payload, &p) == nil {
			out = append(out, p)
		}
	}
	return out
}
