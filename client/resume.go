package client

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/popstarapps/john/terminalui"
)

// `/resume` picks up an earlier conversation in this workspace.
//
// What travels is a key, never a transcript. The backend holds the history it
// wrote itself, sealed with a key it cannot open; this machine returns that
// key during the handshake and the backend unseals its own record. A client
// that could hand back a conversation instead could rewrite one, and every
// control in the loop reads the history as true.

// pastSession is one row of GET /api/v1/sessions.
type pastSession struct {
	ID        string  `json:"id"`
	Workspace string  `json:"workspace"`
	StartedAt string  `json:"startedAt"`
	Status    string  `json:"status"`
	Turns     int     `json:"turns"`
	TokensIn  int     `json:"tokensIn"`
	TokensOut int     `json:"tokensOut"`
	CostUSD   float64 `json:"costUsd"`
	Sealed    bool    `json:"sealed"`
}

// resume runs the /resume command: bare, it lists what this workspace has;
// with an argument, it reopens that conversation.
func (a *app) resume(args string) {
	sessions, err := a.pastSessions()
	if err != nil {
		a.notice(terminalui.NoticeError, "could not list sessions: "+err.Error())
		return
	}
	current := a.wire.currentSession()

	pick := strings.TrimSpace(args)
	if pick == "" {
		a.ui.Send(terminalui.AssistantMessage{Text: renderSessions(sessions, current)})
		return
	}

	target := ""
	if n, err := strconv.Atoi(pick); err == nil && n >= 1 && n <= len(sessions) {
		target = sessions[n-1].ID
	} else {
		for _, s := range sessions {
			if s.ID == pick {
				target = s.ID
				break
			}
		}
	}
	if target == "" {
		a.notice(terminalui.NoticeError, "no session "+pick+" in this workspace — /resume lists them")
		return
	}
	if target == current {
		a.notice(terminalui.NoticeInfo, "already in that session")
		return
	}
	a.notice(terminalui.NoticeInfo, "resuming "+target+"…")
	if err := a.wire.resumeSession(target); err != nil {
		a.notice(terminalui.NoticeError, "could not resume: "+err.Error())
	}
}

// renderSessions writes the list the developer picks from. Only what the
// database holds in the clear is here — when, how long, how much — because
// what was said is in the trace and stays sealed until something with a key
// opens it.
func renderSessions(sessions []pastSession, current string) string {
	if len(sessions) == 0 {
		return "**No earlier sessions** in this workspace yet."
	}
	var b strings.Builder
	b.WriteString("**Sessions in this workspace**\n\n")
	for i, s := range sessions {
		mark := ""
		if s.ID == current {
			mark = " · current"
		}
		when := s.StartedAt
		if t, err := time.Parse(time.RFC3339, s.StartedAt); err == nil {
			when = t.Local().Format("Mon 2 Jan 15:04")
		}
		b.WriteString(fmt.Sprintf("%d. `%s` — %s · %d turns%s\n", i+1, s.ID, when, s.Turns, mark))
	}
	b.WriteString("\n`/resume <number>` carries one on. The conversation is decrypted with the key on this machine.")
	return b.String()
}

// pastSessions asks the backend for this workspace's conversations.
func (a *app) pastSessions() ([]pastSession, error) {
	q := url.Values{}
	q.Set("workspace_key", a.wire.wsKey)
	q.Set("limit", "20")
	req, err := http.NewRequest("GET", a.wire.url+"/api/v1/sessions?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+a.wire.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("backend returned %d", resp.StatusCode)
	}
	var out []pastSession
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}
