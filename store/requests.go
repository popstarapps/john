package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// AccessRequest is a developer asking for a network policy — the only thing a
// member can request.
type AccessRequest struct {
	ID            string
	Ref           string
	Title         string
	RequesterID   string
	TeamID        string
	Status        string // pending | approved | rejected | needs-info
	Justification string
	CreatedAt     time.Time
	DecidedAt     *time.Time
	DecidedByID   *string
	DecisionNote  *string
	GroupID       *string // proposed group; nil means a new group
	GroupName     string
	Spec          json.RawMessage // NetworkSpec
	EvidenceIDs   []string        // blocked network_requests that prompted this
}

// CreateAccessRequest files a request and allocates its REQ-n reference.
func (s *Store) CreateAccessRequest(requesterID, teamID, title, justification, groupName string, groupID *string, spec json.RawMessage, evidenceIDs []string) (*AccessRequest, error) {
	var maxRef int
	if err := s.db.QueryRow(`SELECT COALESCE(MAX(CAST(substr(ref, 5) AS INTEGER)), 99) FROM access_requests`).Scan(&maxRef); err != nil {
		return nil, err
	}
	ev, _ := json.Marshal(evidenceIDs)
	if evidenceIDs == nil {
		ev = []byte("[]")
	}
	id := NewID("r")
	_, err := s.db.Exec(`INSERT INTO access_requests
		(id, ref, title, requester_id, team_id, status, justification, created_at,
		 proposed_group_id, proposed_group_name, proposed_spec, evidence)
		VALUES (?, ?, ?, ?, ?, 'pending', ?, ?, ?, ?, ?, ?)`,
		id, fmt.Sprintf("REQ-%d", maxRef+1), title, requesterID, teamID, justification,
		time.Now().UTC().Format(time.RFC3339), nullStr(groupID), groupName, string(spec), string(ev))
	if err != nil {
		return nil, err
	}
	return s.AccessRequestByIDOrRef(id)
}

// ExtendAccessRequest widens a request that is still open: another blocked
// request for the same host, from the same person, joins the one already
// waiting rather than becoming a request of its own. A request that has been
// decided is left alone.
func (s *Store) ExtendAccessRequest(id string, spec json.RawMessage, justification string) (*AccessRequest, error) {
	res, err := s.db.Exec(`UPDATE access_requests SET proposed_spec = ?, justification = ?
		WHERE id = ? AND status IN ('pending', 'needs-info')`, string(spec), justification, id)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, ErrNotFound
	}
	return s.AccessRequestByIDOrRef(id)
}

// AccessRequestByIDOrRef finds a request by id or by its REQ-n reference.
func (s *Store) AccessRequestByIDOrRef(idOrRef string) (*AccessRequest, error) {
	return scanAccessRequest(s.db.QueryRow(`SELECT `+accessRequestColumns+` FROM access_requests WHERE id = ? OR ref = ?`,
		idOrRef, idOrRef))
}

// AccessRequests lists requests, newest first, optionally filtered. status
// "open" means pending or needs-info.
func (s *Store) AccessRequests(status, requesterID, teamID, search string) ([]*AccessRequest, error) {
	q := `SELECT ` + accessRequestColumns + ` FROM access_requests WHERE 1=1`
	var args []any
	switch status {
	case "":
	case "open":
		q += ` AND status IN ('pending', 'needs-info')`
	default:
		q += ` AND status = ?`
		args = append(args, status)
	}
	if requesterID != "" {
		q += ` AND requester_id = ?`
		args = append(args, requesterID)
	}
	if teamID != "" {
		q += ` AND team_id = ?`
		args = append(args, teamID)
	}
	if search != "" {
		q += ` AND lower(title || ref || justification) LIKE ?`
		args = append(args, "%"+strings.ToLower(search)+"%")
	}
	// rowid and not id, for the reason at NewID: id is random, so it is not a
	// tiebreaker, it is a shuffle.
	q += ` ORDER BY created_at DESC, rowid DESC`
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*AccessRequest
	for rows.Next() {
		r, err := scanAccessRequest(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// DecideAccessRequest approves or rejects a pending request, optionally with
// the reviewer's edited spec and target group. It updates the request only;
// writing the approved policy is the caller's next step, so the change
// history can attribute both.
func (s *Store) DecideAccessRequest(id, status, deciderID string, editedSpec json.RawMessage, editedGroupID *string, groupName string) (*AccessRequest, error) {
	if status != "approved" && status != "rejected" {
		return nil, fmt.Errorf("store: bad decision %q", status)
	}
	r, err := s.AccessRequestByIDOrRef(id)
	if err != nil {
		return nil, err
	}
	if r.Status != "pending" && r.Status != "needs-info" {
		return nil, fmt.Errorf("store: request %s is already %s", r.Ref, r.Status)
	}
	spec := r.Spec
	if editedSpec != nil {
		spec = editedSpec
	}
	groupID := r.GroupID
	if editedGroupID != nil {
		groupID = editedGroupID
	}
	if groupName == "" {
		groupName = r.GroupName
	}
	_, err = s.db.Exec(`UPDATE access_requests SET status = ?, decided_at = ?, decided_by = ?,
		proposed_spec = ?, proposed_group_id = ?, proposed_group_name = ? WHERE id = ?`,
		status, time.Now().UTC().Format(time.RFC3339), deciderID,
		string(spec), nullStr(groupID), groupName, r.ID)
	if err != nil {
		return nil, err
	}
	return s.AccessRequestByIDOrRef(r.ID)
}

// SetAccessRequestOpenStatus moves an open request between pending and
// needs-info. It is deliberately not DecideAccessRequest: asking a question
// is not a decision, so nothing here writes decided_at or decided_by, and a
// request that has already been approved or rejected cannot be dragged back
// open by it.
func (s *Store) SetAccessRequestOpenStatus(id, status string) (*AccessRequest, error) {
	if status != "pending" && status != "needs-info" {
		return nil, fmt.Errorf("store: %q is not an open status", status)
	}
	r, err := s.AccessRequestByIDOrRef(id)
	if err != nil {
		return nil, err
	}
	if r.Status != "pending" && r.Status != "needs-info" {
		return nil, fmt.Errorf("store: request %s is already %s", r.Ref, r.Status)
	}
	if r.Status == status {
		return r, nil
	}
	if _, err := s.db.Exec(`UPDATE access_requests SET status = ? WHERE id = ?`, status, r.ID); err != nil {
		return nil, err
	}
	return s.AccessRequestByIDOrRef(r.ID)
}

// --- the conversation on a request ---

// RequestComment is one thing somebody said about an access request.
type RequestComment struct {
	ID        string
	RequestID string
	AuthorID  string
	At        time.Time
	Body      string
}

// AddRequestComment appends a comment. Comments are append-only, like the
// change history: a reviewer's question and the answer to it are the reason a
// request was decided the way it was, and editing that away would leave a
// decision with no argument behind it.
func (s *Store) AddRequestComment(requestID, authorID, body string) (*RequestComment, error) {
	body = strings.TrimSpace(body)
	if body == "" {
		return nil, fmt.Errorf("store: empty comment")
	}
	r, err := s.AccessRequestByIDOrRef(requestID)
	if err != nil {
		return nil, err
	}
	c := &RequestComment{
		ID:        NewID("rc"),
		RequestID: r.ID,
		AuthorID:  authorID,
		At:        time.Now().UTC(),
		Body:      body,
	}
	_, err = s.db.Exec(`INSERT INTO request_comments (id, request_id, author_id, at, body)
		VALUES (?, ?, ?, ?, ?)`,
		c.ID, c.RequestID, c.AuthorID, c.At.Format(time.RFC3339Nano), c.Body)
	if err != nil {
		return nil, err
	}
	return c, nil
}

// RequestComments lists a request's conversation, oldest first.
func (s *Store) RequestComments(requestID string) ([]*RequestComment, error) {
	rows, err := s.db.Query(`SELECT id, request_id, author_id, at, body FROM request_comments
		WHERE request_id = ? ORDER BY at, rowid`, requestID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*RequestComment{}
	for rows.Next() {
		var c RequestComment
		var at string
		if err := rows.Scan(&c.ID, &c.RequestID, &c.AuthorID, &at, &c.Body); err != nil {
			return nil, err
		}
		c.At, _ = time.Parse(time.RFC3339Nano, at)
		out = append(out, &c)
	}
	return out, rows.Err()
}

const accessRequestColumns = `id, ref, title, requester_id, team_id, status, justification, created_at,
	decided_at, decided_by, decision_note, proposed_group_id, proposed_group_name, proposed_spec, evidence`

func scanAccessRequest(row interface{ Scan(...any) error }) (*AccessRequest, error) {
	var r AccessRequest
	var created string
	var decidedAt, decidedBy, note, groupID sql.NullString
	var spec, evidence string
	err := row.Scan(&r.ID, &r.Ref, &r.Title, &r.RequesterID, &r.TeamID, &r.Status, &r.Justification, &created,
		&decidedAt, &decidedBy, &note, &groupID, &r.GroupName, &spec, &evidence)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	r.CreatedAt, _ = time.Parse(time.RFC3339, created)
	if decidedAt.Valid {
		t, _ := time.Parse(time.RFC3339, decidedAt.String)
		r.DecidedAt = &t
	}
	if decidedBy.Valid {
		r.DecidedByID = &decidedBy.String
	}
	if note.Valid {
		r.DecisionNote = &note.String
	}
	if groupID.Valid && groupID.String != "" {
		r.GroupID = &groupID.String
	}
	r.Spec = json.RawMessage(spec)
	json.Unmarshal([]byte(evidence), &r.EvidenceIDs)
	if r.EvidenceIDs == nil {
		r.EvidenceIDs = []string{}
	}
	return &r, nil
}

// --- proxy traffic ---

// NetworkRequest is one request the proxy saw and what became of it.
type NetworkRequest struct {
	ID             string
	At             time.Time
	UserID         string
	SessionID      string
	Method         string
	Scheme         string
	Host           string
	Port           int
	Path           string
	Query          string
	Status         string // allowed | unknown | denied | awaiting-approval | approved | rejected
	PolicyID       *string
	Reason         string
	BytesOut       int64
	BytesIn        int64
	DurationMS     int64
	ResponseStatus *int
	Headers        [][2]string
	BodyPreview    *string
	ContentType    *string
}

// InsertNetworkRequest records one proxy decision.
func (s *Store) InsertNetworkRequest(n *NetworkRequest) error {
	if n.ID == "" {
		n.ID = NewID("n")
	}
	if n.At.IsZero() {
		n.At = time.Now().UTC()
	}
	headers, _ := json.Marshal(n.Headers)
	if n.Headers == nil {
		headers = []byte("[]")
	}
	var policyID, preview, ctype sql.NullString
	if n.PolicyID != nil {
		policyID = sql.NullString{String: *n.PolicyID, Valid: true}
	}
	if n.BodyPreview != nil {
		preview = sql.NullString{String: *n.BodyPreview, Valid: true}
	}
	if n.ContentType != nil {
		ctype = sql.NullString{String: *n.ContentType, Valid: true}
	}
	var respStatus sql.NullInt64
	if n.ResponseStatus != nil {
		respStatus = sql.NullInt64{Int64: int64(*n.ResponseStatus), Valid: true}
	}
	_, err := s.db.Exec(`INSERT INTO network_requests
		(id, at, user_id, session_id, method, scheme, host, port, path, query, status,
		 policy_id, reason, bytes_out, bytes_in, duration_ms, response_status, headers, body_preview, content_type)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		n.ID, n.At.UTC().Format(time.RFC3339Nano), n.UserID, n.SessionID, n.Method, n.Scheme, n.Host, n.Port,
		n.Path, n.Query, n.Status, policyID.String, n.Reason, n.BytesOut, n.BytesIn, n.DurationMS,
		respStatus, string(headers), preview, ctype)
	return err
}

// NetworkRequests lists proxy traffic, newest first. Filters compose; ids
// wins over everything else when set.
func (s *Store) NetworkRequests(ids []string, userID, sessionID, status, search string, limit int) ([]*NetworkRequest, error) {
	q := `SELECT id, at, user_id, session_id, method, scheme, host, port, path, query, status,
		policy_id, reason, bytes_out, bytes_in, duration_ms, response_status, headers, body_preview, content_type
		FROM network_requests WHERE 1=1`
	var args []any
	if len(ids) > 0 {
		q += ` AND id IN (?` + strings.Repeat(",?", len(ids)-1) + `)`
		for _, id := range ids {
			args = append(args, id)
		}
	}
	if userID != "" {
		q += ` AND user_id = ?`
		args = append(args, userID)
	}
	if sessionID != "" {
		q += ` AND session_id = ?`
		args = append(args, sessionID)
	}
	switch status {
	case "":
	case "needs-decision":
		q += ` AND status = 'awaiting-approval'`
	default:
		q += ` AND status = ?`
		args = append(args, status)
	}
	if search != "" {
		q += ` AND lower(host || path || method) LIKE ?`
		args = append(args, "%"+strings.ToLower(search)+"%")
	}
	q += ` ORDER BY at DESC, rowid DESC`
	if limit > 0 {
		q += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*NetworkRequest
	for rows.Next() {
		n, err := scanNetworkRequest(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// NetworkRequestByID looks one up.
func (s *Store) NetworkRequestByID(id string) (*NetworkRequest, error) {
	out, err := s.NetworkRequests([]string{id}, "", "", "", "", 1)
	if err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, ErrNotFound
	}
	return out[0], nil
}

func scanNetworkRequest(row interface{ Scan(...any) error }) (*NetworkRequest, error) {
	var n NetworkRequest
	var at, headers string
	var policyID, preview, ctype sql.NullString
	var respStatus sql.NullInt64
	err := row.Scan(&n.ID, &at, &n.UserID, &n.SessionID, &n.Method, &n.Scheme, &n.Host, &n.Port,
		&n.Path, &n.Query, &n.Status, &policyID, &n.Reason, &n.BytesOut, &n.BytesIn, &n.DurationMS,
		&respStatus, &headers, &preview, &ctype)
	if err != nil {
		return nil, err
	}
	n.At, _ = time.Parse(time.RFC3339Nano, at)
	if policyID.Valid && policyID.String != "" {
		n.PolicyID = &policyID.String
	}
	if preview.Valid {
		n.BodyPreview = &preview.String
	}
	if ctype.Valid {
		n.ContentType = &ctype.String
	}
	if respStatus.Valid {
		v := int(respStatus.Int64)
		n.ResponseStatus = &v
	}
	json.Unmarshal([]byte(headers), &n.Headers)
	return &n, nil
}

func nullStr(s *string) sql.NullString {
	if s == nil || *s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: *s, Valid: true}
}
