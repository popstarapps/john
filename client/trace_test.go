//go:build !windows

package client

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"filippo.io/age"
	"github.com/oliverkofoed/gokit/agentkit/llm/llmtest"

	"github.com/popstarapps/john/client/credstore"
	"github.com/popstarapps/john/trace"
)

// traceFile reads the sealed trace this session wrote.
func (s *e2eSession) traceFile(t *testing.T) []byte {
	t.Helper()
	session := s.app.wire.currentSession()
	if session == "" {
		t.Fatal("no session was opened")
	}
	row, err := s.srv.Store().SessionByID(session)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(s.traceDir, row.StartedAt.UTC().Format("2006"), session+".jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the trace: %v", err)
	}
	return data
}

// TestDeveloperReadsTheirOwnSession is the developer half of the design,
// through the real client: the key generated on this machine at sign-in opens
// the trace the backend wrote, and the backend never held that key.
func TestDeveloperReadsTheirOwnSession(t *testing.T) {
	s := newE2E(t, llmtest.Text("SENTINEL-model-answer"))
	s.type_("remember SENTINEL-developer-prompt\r")
	s.wait("SENTINEL-model-answer")

	data := s.traceFile(t)
	if bytes.Contains(data, []byte("SENTINEL-developer-prompt")) {
		t.Fatal("the prompt reached disk in the clear")
	}

	id, err := credstore.LoadIdentity()
	if err != nil {
		t.Fatalf("the client did not keep a trace identity: %v", err)
	}
	identity, err := age.ParseX25519Identity(id.Private)
	if err != nil {
		t.Fatal(err)
	}
	records, err := trace.Records(bytes.NewReader(data), identity)
	if err != nil {
		t.Fatalf("this machine could not read its own session: %v", err)
	}
	var sawPrompt, sawAnswer bool
	for _, rec := range records {
		body := string(rec.Payload)
		sawPrompt = sawPrompt || strings.Contains(body, "SENTINEL-developer-prompt")
		sawAnswer = sawAnswer || strings.Contains(body, "SENTINEL-model-answer")
	}
	if !sawPrompt || !sawAnswer {
		t.Fatalf("read %d records, prompt=%v answer=%v", len(records), sawPrompt, sawAnswer)
	}

	// And the administrator reads the same file with the organization key.
	if _, err := trace.Records(bytes.NewReader(data), s.orgKey); err != nil {
		t.Fatalf("the organization key could not read the session: %v", err)
	}
}

// TestResumeListsThisWorkspacesSessions checks the command a developer
// actually types. What it prints is metadata the database holds in the clear;
// carrying one on is what needs the key.
func TestResumeListsThisWorkspacesSessions(t *testing.T) {
	s := newE2E(t, llmtest.Text("done"))
	s.type_("do something\r")
	s.wait("done")

	s.type_("/resume\r")
	s.wait("Sessions in this workspace")
	s.wait("current")
}
