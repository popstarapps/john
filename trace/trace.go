// Package trace writes the backend's session traces: append-only files of
// encrypted records, one per session.
//
// Each session gets its own key, which is what actually encrypts the records
// and the agent state. Copies of that key, wrapped for the organization and
// for the developer the session belongs to, ride in the file's own header —
// so a restored trace store needs a private key and nothing else, not a
// matching database. The backend holds public keys only: it mints a session
// key, uses it while the session is live, and once the key leaves memory it
// can never read the file back.
//
// See readme.md in this directory for the key hierarchy and the file format.
//
// Records are kept forever. Nothing here expires, amends, or deletes one.
package trace

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"filippo.io/age"
)

// Version is the trace file format version.
const Version = 1

// Record is what gets sealed. Payload is free-form JSON per kind.
type Record struct {
	At      time.Time       `json:"at"`
	Session string          `json:"session"`
	Actor   string          `json:"actor"`
	Kind    string          `json:"kind"` // session.open, prompt, tool.call, tool.result, assistant.message, turn.end, proxy.decision, …
	Payload json.RawMessage `json:"payload,omitempty"`
}

// Header is the first line of a trace file: what the session is, and the
// session key wrapped once per recipient.
type Header struct {
	V       int          `json:"v"`
	Session string       `json:"session"`
	Started time.Time    `json:"started"`
	Alg     string       `json:"alg"`
	Keys    []WrappedKey `json:"keys"`
}

// WrappedKey is one recipient's copy of the session key. For is "org" or a
// user id; Wrapped is the base64 age ciphertext.
type WrappedKey struct {
	For     string `json:"for"`
	Wrapped string `json:"wrapped"`
}

// Key returns the copy wrapped for one recipient.
func (h *Header) Key(for_ string) (string, bool) {
	for _, k := range h.Keys {
		if k.For == for_ {
			return k.Wrapped, true
		}
	}
	return "", false
}

// OrgRecipient names the organization's copy in a header.
const OrgRecipient = "org"

// Writer appends encrypted records, one file per session, grouped by the year
// the session started.
//
// One Writer serves the whole backend. Sequence numbers are a line's position
// in its own file, so a reader can see a deleted line as a gap; that only
// holds if every append to a file goes through one counter, which is why the
// backend keeps a single writer rather than one per session.
type Writer struct {
	dir       string
	recipient string // the organization's age recipient

	mu   sync.Mutex
	seqs map[string]int // by file path: the last sequence number written to it
}

// NewWriter checks the org's age recipient (an "age1…" string) and roots the
// store at dir. An empty recipient is an error: traces are never written in
// the clear, so no key means no writer.
func NewWriter(dir, recipient string) (*Writer, error) {
	if _, err := age.ParseX25519Recipient(recipient); err != nil {
		return nil, fmt.Errorf("trace: bad recipient: %w", err)
	}
	return &Writer{dir: dir, recipient: recipient, seqs: map[string]int{}}, nil
}

// Session is a handle to one session's file. The path is fixed by the
// session's start, not by the clock, so a conversation that runs past New
// Year keeps writing to the file that holds its header and its key.
type Session struct {
	w    *Writer
	id   string
	path string
}

// Session opens a handle. It touches no disk.
func (w *Writer) Session(id string, startedAt time.Time) *Session {
	if startedAt.IsZero() {
		startedAt = time.Now()
	}
	return &Session{w: w, id: id,
		path: filepath.Join(w.dir, startedAt.UTC().Format("2006"), id+".jsonl")}
}

// Path is the file this session writes to.
func (s *Session) Path() string { return s.path }

// Header reads the file's first line. It returns nil, nil when the session
// has no file yet.
func (s *Session) Header() (*Header, error) {
	f, err := os.Open(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()

	line, _ := bufio.NewReader(f).ReadBytes('\n')
	if len(line) == 0 {
		return nil, nil // an empty or truncated file has no header
	}
	var h Header
	if err := json.Unmarshal(bytes.TrimSpace(line), &h); err != nil || h.V == 0 {
		return nil, nil
	}
	return &h, nil
}

// Create mints a session key and writes the header that carries it, wrapped
// for the organization and — when the developer has registered a public key —
// for the developer too. It returns the key, which the caller holds in memory
// for as long as the session is live and nowhere else.
//
// A user with no registered key gets a header with only the organization's
// copy: the session works, and until an admin grants them, they cannot read
// it back.
func (s *Session) Create(userID, userRecipient string, startedAt time.Time) ([]byte, *Header, error) {
	key, err := NewKey()
	if err != nil {
		return nil, nil, err
	}
	orgWrapped, err := WrapKey(key, s.w.recipient)
	if err != nil {
		return nil, nil, err
	}
	h := &Header{
		V: Version, Session: s.id, Started: startedAt.UTC(), Alg: "chacha20poly1305",
		Keys: []WrappedKey{{For: OrgRecipient, Wrapped: orgWrapped}},
	}
	if userRecipient != "" && userID != "" {
		userWrapped, err := WrapKey(key, userRecipient)
		if err != nil {
			return nil, nil, err
		}
		h.Keys = append(h.Keys, WrappedKey{For: userID, Wrapped: userWrapped})
	}
	line, err := json.Marshal(h)
	if err != nil {
		return nil, nil, err
	}
	if err := s.appendLine(line, nil); err != nil {
		return nil, nil, err
	}
	return key, h, nil
}

// Append seals one record with the session key and appends it. The record's
// plaintext exists only in memory; what reaches disk is
// {"seq":n,"c":"<base64 nonce and ciphertext>"}.
func (s *Session) Append(key []byte, rec Record) error {
	if len(key) != KeySize {
		return errors.New("trace: refusing to append without a session key")
	}
	if rec.At.IsZero() {
		rec.At = time.Now().UTC()
	}
	plain, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	sealed, err := Seal(key, plain)
	if err != nil {
		return err
	}
	return s.appendLine(nil, map[string]any{"c": sealed})
}

// Grant appends a copy of the session key wrapped for another recipient. It
// takes the wrapped bytes, never the key: the only party that can produce one
// is whoever already holds a private key that opens this session, which is
// never the backend.
func (s *Session) Grant(forID, wrapped string) error {
	if forID == "" || wrapped == "" {
		return errors.New("trace: a grant needs a recipient and a wrapped key")
	}
	return s.appendLine(nil, map[string]any{"grant": forID, "wrapped": wrapped})
}

// appendLine writes one line, numbering it by its position in the file.
// Exactly one of raw and fields is given.
func (s *Session) appendLine(raw []byte, fields map[string]any) error {
	w := s.w
	w.mu.Lock()
	defer w.mu.Unlock()

	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	seq, err := w.nextSeq(s.path)
	if err != nil {
		return err
	}

	var line []byte
	if fields != nil {
		merged := map[string]any{"seq": seq}
		for k, v := range fields {
			merged[k] = v
		}
		if line, err = json.Marshal(merged); err != nil {
			return err
		}
	} else {
		// The header carries its own fields; splice the sequence number in.
		var doc map[string]json.RawMessage
		if err := json.Unmarshal(raw, &doc); err != nil {
			return err
		}
		doc["seq"] = json.RawMessage(fmt.Sprint(seq))
		if line, err = json.Marshal(doc); err != nil {
			return err
		}
	}

	f, err := os.OpenFile(s.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(append(line, '\n')); err != nil {
		return err
	}
	// Only once the line is on disk, so a failed write does not consume a
	// number and leave a gap that reads as a deletion.
	w.seqs[s.path] = seq
	return nil
}

// nextSeq is the number for the next line in path. The first time this writer
// touches a file it counts what is already there rather than starting at one:
// a session reopened after a backend restart continues its file, instead of
// laying a second run of numbers over the first.
//
// Callers hold w.mu.
func (w *Writer) nextSeq(path string) (int, error) {
	if n, ok := w.seqs[path]; ok {
		return n + 1, nil
	}
	n, err := countLines(path)
	if err != nil {
		return 0, err
	}
	return n + 1, nil
}

// countLines counts newlines in a file, treating a missing file as empty.
func countLines(path string) (int, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	defer f.Close()

	n := 0
	buf := make([]byte, 64*1024)
	for {
		read, err := f.Read(buf)
		n += bytes.Count(buf[:read], []byte{'\n'})
		if errors.Is(err, io.EOF) {
			return n, nil
		}
		if err != nil {
			return 0, err
		}
	}
}

// Size walks a trace store and returns what it occupies on disk. The console
// reports it beside the database size; nothing else depends on it.
func Size(dir string) int64 {
	var total int64
	filepath.WalkDir(dir, func(_ string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // an unreadable corner counts as zero, not as a failure
		}
		if !d.IsDir() {
			if info, err := d.Info(); err == nil {
				total += info.Size()
			}
		}
		return nil
	})
	return total
}

// Records reads a whole trace file and returns every record, in file order.
//
// Key material is collected before anything is decrypted, because a grant is
// appended when it is issued and so sits after the records it opens.
func Records(r io.Reader, identity age.Identity) ([]Record, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 16<<20)

	type entry struct {
		V       int          `json:"v"`
		C       string       `json:"c"`
		Grant   string       `json:"grant"`
		Wrapped string       `json:"wrapped"`
		Keys    []WrappedKey `json:"keys"`
	}
	var lines []entry
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var e entry
		if err := json.Unmarshal(line, &e); err != nil {
			return nil, err
		}
		lines = append(lines, e)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(lines) == 0 {
		return nil, nil
	}

	if lines[0].V == 0 {
		return nil, errors.New("trace: file has no header")
	}
	if lines[0].V != Version {
		return nil, fmt.Errorf("trace: unsupported file version %d", lines[0].V)
	}

	// Every copy of the session key this identity might be able to open: the
	// header's, and any granted later.
	var wrapped []string
	for _, k := range lines[0].Keys {
		wrapped = append(wrapped, k.Wrapped)
	}
	for _, e := range lines {
		if e.Grant != "" && e.Wrapped != "" {
			wrapped = append(wrapped, e.Wrapped)
		}
	}
	var key []byte
	for _, candidate := range wrapped {
		if got, err := UnwrapKey(candidate, identity); err == nil {
			key = got
			break
		}
	}

	if key == nil {
		return nil, errors.New("trace: no key in this file opens for this identity")
	}

	var out []Record
	for i, e := range lines {
		if e.C == "" {
			continue
		}
		plain, err := Open(key, e.C)
		if err != nil {
			return nil, fmt.Errorf("trace: line %d: %w", i+1, err)
		}
		var rec Record
		if err := json.Unmarshal(plain, &rec); err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, nil
}
