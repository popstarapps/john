package backend

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strconv"

	"github.com/popstarapps/john/store"
	"github.com/popstarapps/john/trace"
)

// The endpoints that serve session traces. Nothing here decrypts anything:
// the backend hands out sealed bytes and the reader — a developer's client,
// or an administrator's browser holding the organization key — opens them.
// There is deliberately no endpoint that takes a private key.

// handleListSessions is GET /api/v1/sessions: the caller's own conversations,
// newest first, which is what `/resume` picks from. A developer sees their
// own; nobody sees anybody else's, because the trace, not this list, is where
// oversight happens.
func (s *Server) handleListSessions(w http.ResponseWriter, r *http.Request) {
	user := s.requireUser(w, r)
	if user == nil {
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	rows, err := s.store.SessionsForUser(user.ID, r.URL.Query().Get("workspace_key"), limit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		out = append(out, map[string]any{
			"id": row.ID, "workspace": row.WorkspacePath, "startedAt": ts(row.StartedAt),
			"status": row.Status, "turns": row.Turns, "modelId": row.ModelID,
			"tokensIn": row.TokensIn, "tokensOut": row.TokensOut, "costUsd": row.CostUSD,
			"sealed": trace.IsSealedState(row.State),
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// handleTraceSessions is GET /api/v1/traces: what an administrator can browse,
// one row per session. Everything here is metadata the database already holds
// in the clear — who, when, how long. What was said is in the file, sealed,
// and the console fetches that separately and opens it in the browser.
func (s *Server) handleTraceSessions(w http.ResponseWriter, r *http.Request) {
	if s.requireAdmin(w, r) == nil {
		return
	}
	rows, err := s.store.AllSessions(200)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	users, err := s.store.Users()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	byID := map[string]*store.User{}
	for _, u := range users {
		byID[u.ID] = u
	}

	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		name, email := row.UserID, ""
		if u := byID[row.UserID]; u != nil {
			name, email = u.Name, u.Email
			if name == "" {
				name = u.Email
			}
		}
		out = append(out, map[string]any{
			"id": row.ID, "actorId": row.UserID, "actorName": name, "actorEmail": email,
			"project": row.WorkspacePath, "startedAt": ts(row.StartedAt),
			"status": row.Status, "turns": row.Turns,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// handleTraceFile is GET /api/v1/traces/{session}/file: the sealed trace,
// exactly as it is on disk, line for line. The reader needs a private key the
// backend does not have, which is the point — this endpoint is as useless to
// whoever steals it as the file is.
func (s *Server) handleTraceFile(w http.ResponseWriter, r *http.Request) {
	user := s.requireUser(w, r)
	if user == nil {
		return
	}
	row, err := s.store.SessionByID(r.PathValue("session"))
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "no such session")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	// A developer reads their own; an administrator reads any. Neither can
	// read a word of it without a key held outside this process.
	if row.UserID != user.ID && user.Role != "admin" {
		writeErr(w, http.StatusForbidden, "not your session")
		return
	}

	sess, err := s.traceSession(row)
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	f, err := os.Open(sess.Path())
	if errors.Is(err, os.ErrNotExist) {
		writeErr(w, http.StatusNotFound, "no trace was recorded for this session")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-store")
	io.Copy(w, f)
}

// handleTraceGrant is POST /api/v1/traces/{session}/grant: an administrator
// gives someone access to a session they cannot currently read.
//
// The backend is handed a key already wrapped for the recipient, never a
// plaintext one — the unwrapping and re-wrapping happen in the admin's
// browser, with the organization key, which never comes here. All this does
// is append the result to the file, which is why it cannot be used to read
// anything.
func (s *Server) handleTraceGrant(w http.ResponseWriter, r *http.Request) {
	admin := s.requireAdmin(w, r)
	if admin == nil {
		return
	}
	var body struct {
		UserID  string `json:"userId"`
		Wrapped string `json:"wrapped"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad payload: "+err.Error())
		return
	}
	if body.UserID == "" || body.Wrapped == "" {
		writeErr(w, http.StatusBadRequest, "userId and wrapped are required")
		return
	}
	if _, err := s.store.UserByID(body.UserID); err != nil {
		writeErr(w, http.StatusBadRequest, "no such user")
		return
	}

	row, err := s.store.SessionByID(r.PathValue("session"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "no such session")
		return
	}
	sess, err := s.traceSession(row)
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	if err := sess.Grant(body.UserID, body.Wrapped); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.store.RecordChange(admin.ID, "trace", "granted", row.ID, row.ID,
		"granted access to session "+row.ID+" to "+body.UserID, nil)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleTraceRecipients is GET /api/v1/traces/recipients: the registered
// public keys an administrator can grant to. Public halves only.
func (s *Server) handleTraceRecipients(w http.ResponseWriter, r *http.Request) {
	if s.requireAdmin(w, r) == nil {
		return
	}
	users, err := s.store.Users()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]map[string]any, 0, len(users))
	for _, u := range users {
		out = append(out, map[string]any{
			"id": u.ID, "name": u.Name, "email": u.Email, "recipient": u.TracePubKey,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// traceSession is the file handle for one session, or an error explaining
// that this backend records no traces at all.
func (s *Server) traceSession(row *store.Session) (*trace.Session, error) {
	w := s.traceWriter()
	if w == nil {
		return nil, errors.New("this backend has no trace store configured")
	}
	return w.Session(row.ID, row.StartedAt), nil
}
