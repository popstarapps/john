package backend

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/popstarapps/john/auth"
	"github.com/popstarapps/john/store"
)

// The JSON shapes below mirror console/src/api/types.ts, which is the
// contract: the console was written first and the backend answers in its
// vocabulary.

type userJSON struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	Email        string   `json:"email"`
	Initials     string   `json:"initials"`
	Role         string   `json:"role"`
	Title        string   `json:"title"`
	Status       string   `json:"status"`
	TeamIDs      []string `json:"teamIds"`
	SSOSubject   string   `json:"ssoSubject"`
	CreatedAt    string   `json:"createdAt"`
	LastActiveAt *string  `json:"lastActiveAt"`
}

func toUserJSON(u *store.User, teamIDs []string) userJSON {
	if teamIDs == nil {
		teamIDs = []string{}
	}
	var last *string
	if !u.LastActiveAt.IsZero() {
		v := u.LastActiveAt.UTC().Format(time.RFC3339)
		last = &v
	}
	return userJSON{
		ID: u.ID, Name: u.Name, Email: u.Email, Initials: u.Initials,
		Role: u.Role, Title: u.Title, Status: u.Status, TeamIDs: teamIDs,
		SSOSubject: u.SSOSubject, CreatedAt: u.CreatedAt.UTC().Format(time.RFC3339),
		LastActiveAt: last,
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// requireUser authenticates a request or answers it with the right error.
func (s *Server) requireUser(w http.ResponseWriter, r *http.Request) *store.User {
	if _, err := s.store.Org(); errors.Is(err, store.ErrNoOrg) {
		writeErr(w, http.StatusServiceUnavailable, "setup_required")
		return nil
	}
	u := s.auth.UserFor(r)
	if u == nil {
		writeErr(w, http.StatusUnauthorized, "unauthenticated")
		return nil
	}
	return u
}

func (s *Server) handleViewer(w http.ResponseWriter, r *http.Request) {
	u := s.requireUser(w, r)
	if u == nil {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"user":    toUserJSON(u, nil),
		"isAdmin": u.Role == "admin",
	})
}

func (s *Server) handleCLIStart(w http.ResponseWriter, r *http.Request) {
	if _, err := s.store.Org(); errors.Is(err, store.ErrNoOrg) {
		writeErr(w, http.StatusServiceUnavailable, "setup_required")
		return
	}
	code, url := s.auth.StartCLI(auth.BaseURL(r))
	writeJSON(w, http.StatusOK, map[string]string{"code": code, "url": url})
}

func (s *Server) handleCLIPoll(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Code string `json:"code"`
		Host string `json:"host"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Code == "" {
		writeErr(w, http.StatusBadRequest, "bad request")
		return
	}
	creds, user, err := s.auth.PollCLI(req.Code, req.Host)
	if errors.Is(err, auth.ErrPending) {
		writeJSON(w, http.StatusAccepted, map[string]bool{"pending": true})
		return
	}
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, credentialsJSON(creds, user))
}

// handleRefresh trades a refresh token for a new pair. It is deliberately not
// behind requireUser: the refresh token IS the credential being presented,
// and the access token it renews may well have expired already — requiring a
// live one would make the endpoint useless exactly when it is needed.
func (s *Server) handleRefresh(w http.ResponseWriter, r *http.Request) {
	var req struct {
		RefreshToken string `json:"refreshToken"`
		Host         string `json:"host"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.RefreshToken == "" {
		writeErr(w, http.StatusBadRequest, "bad request")
		return
	}
	creds, user, err := s.auth.Refresh(req.RefreshToken, req.Host)
	switch {
	case errors.Is(err, store.ErrRefreshCapped):
		// Said plainly, because this one is not a failure: the machine did
		// nothing wrong and the fix is simply to sign in again.
		writeErr(w, http.StatusUnauthorized, "sign_in_required")
		return
	case errors.Is(err, store.ErrRefreshReused):
		// The chain is already revoked by now. The answer to the caller is
		// the same either way; which of the two copies this was is not
		// knowable from here.
		writeErr(w, http.StatusUnauthorized, "sign_in_required")
		return
	case err != nil:
		writeErr(w, http.StatusUnauthorized, "sign_in_required")
		return
	}
	writeJSON(w, http.StatusOK, credentialsJSON(creds, user))
}

// credentialsJSON is the one shape a client ever receives credentials in,
// used by both the end of a sign-in and every refresh after it.
func credentialsJSON(c *store.MachineCredentials, user *store.User) map[string]any {
	return map[string]any{
		"token":            c.Access,
		"expiresAt":        c.AccessExpiresAt.UTC().Format(time.RFC3339),
		"refreshToken":     c.Refresh,
		"refreshExpiresAt": c.RefreshExpiresAt.UTC().Format(time.RFC3339),
		"user":             toUserJSON(user, nil),
	}
}
