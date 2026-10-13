package client

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/popstarapps/john/protocol"
)

// backendSaying serves the one endpoint the start-up check reads.
func backendSaying(t *testing.T, version string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/org/public" {
			http.NotFound(w, r)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"name":    "Test Org",
			"backend": map[string]any{"version": version},
		})
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestTheStartUpCheckNoticesADifferentBackend(t *testing.T) {
	base := backendSaying(t, "0.5.2")

	u := checkVersion(base, "0.4.1", "tok")
	if u == nil {
		t.Fatal("a client on the wrong version was not told to update")
	}
	if u.Required != "0.5.2" || u.Current != "0.4.1" {
		t.Fatalf("update = %+v", u)
	}
	if !strings.HasSuffix(u.URL, "/releases/"+releaseFileName()) {
		t.Fatalf("download URL = %q, want this platform's build", u.URL)
	}

	// The matching build has nothing to do.
	if got := checkVersion(base, "0.5.2", "tok"); got != nil {
		t.Fatalf("the right version was told to update: %+v", got)
	}
}

// Neither side being a released build means there is nothing to download,
// so nothing is proposed.
func TestDevelopmentBuildsAreNotSentToUpdate(t *testing.T) {
	if got := checkVersion(backendSaying(t, "0.5.2"), "dev", ""); got != nil {
		t.Fatalf("a dev client was told to update: %+v", got)
	}
	if got := checkVersion(backendSaying(t, "dev"), "0.4.1", ""); got != nil {
		t.Fatalf("a dev backend demanded an update: %+v", got)
	}
}

// A version check that cannot complete is not a reason to refuse to work.
// The handshake still enforces the rule; this one only saves a round trip.
func TestAnUnreachableBackendProposesNothing(t *testing.T) {
	if got := checkVersion("http://127.0.0.1:1", "0.4.1", ""); got != nil {
		t.Fatalf("an unreachable backend produced an update: %+v", got)
	}
}

// The binary is not rewritten without a yes — and the question has to say
// plainly what is about to happen.
func TestTheUpdateIsAlwaysAsked(t *testing.T) {
	u := &update{Required: "0.5.2", Current: "0.4.1"}

	var out bytes.Buffer
	if !u.confirm(strings.NewReader("\n"), &out) {
		t.Fatal("pressing enter did not accept")
	}
	said := out.String()
	for _, want := range []string{"0.5.2", "0.4.1", "replace this binary"} {
		if !strings.Contains(said, want) {
			t.Fatalf("the question never mentioned %q:\n%s", want, said)
		}
	}

	for _, answer := range []string{"n\n", "no\n", ""} {
		out.Reset()
		if u.confirm(strings.NewReader(answer), &out) {
			t.Fatalf("answering %q was taken as a yes", answer)
		}
	}

	out.Reset()
	if !u.confirm(strings.NewReader("maybe\ny\n"), &out) {
		t.Fatal("a yes after a bad answer was not accepted")
	}
	if !strings.Contains(out.String(), "Please answer y or n.") {
		t.Fatalf("a bad answer was not re-asked:\n%s", out.String())
	}
}

// Nobody there to answer means nobody to consent, so it explains itself and
// stops rather than replacing a binary unattended.
func TestWithNoTerminalTheBinaryIsLeftAlone(t *testing.T) {
	u := &update{Required: "0.5.2", Current: "0.4.1"}

	var out bytes.Buffer
	if code := runUpdate(u, strings.NewReader("y\n"), &out, false); code != 1 {
		t.Fatalf("non-interactive update = %d, want 1", code)
	}
	if !strings.Contains(out.String(), "0.5.2") {
		t.Fatalf("did not say which version is needed:\n%s", out.String())
	}
}

func TestDecliningAnUpdateEndsTheRun(t *testing.T) {
	u := &update{Required: "0.5.2", Current: "0.4.1"}

	var out bytes.Buffer
	if code := runUpdate(u, strings.NewReader("n\n"), &out, true); code != 1 {
		t.Fatalf("declined update = %d, want 1", code)
	}
	if !strings.Contains(out.String(), "will not run john 0.4.1") {
		t.Fatalf("declining did not say why the session cannot run:\n%s", out.String())
	}
}

// A download that came back short must never be installed: replacing a
// working binary with a truncated one leaves nothing to run.
func TestATruncatedDownloadIsNotInstalled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("not really a binary"))
	}))
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	u := &update{Required: "0.5.2", Current: "0.4.1", URL: srv.URL}
	if _, err := u.download(filepath.Join(dir, "john"), io.Discard); err == nil {
		t.Fatal("a nineteen-byte download was accepted as a binary")
	}

	// And nothing was left lying around next to the binary it would have
	// replaced: a stray half-download beside a real binary is its own trap.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("left files behind: %v", names)
	}
}

// The two halves have to agree: what the backend sends as a refusal is what
// the client acts on. This drives the client's handshake with the very
// message type backend/version_test.go proves the backend produces.
func TestTheClientActsOnTheBackendsRefusal(t *testing.T) {
	a, b := newFramePair()

	w := &wire{
		app:   &app{version: "0.4.1"},
		url:   "https://john.acme.example",
		token: "machine-token",
	}

	go func() {
		// Read the client's session.open, then refuse it.
		if _, err := b.ReadFrame(context.Background()); err != nil {
			return
		}
		data, _ := protocol.EncodeRaw(&protocol.VersionMismatch{
			Required: "0.5.2",
			Client:   "0.4.1",
			Download: "/releases/john-darwin-arm64",
		})
		b.WriteFrame(context.Background(), data)
	}()

	_, err := w.handshake(context.Background(), a, &protocol.SessionOpen{})

	var ve versionError
	if !errors.As(err, &ve) {
		t.Fatalf("handshake = %v, want a versionError", err)
	}
	if ve.u.Required != "0.5.2" || ve.u.Current != "0.4.1" {
		t.Fatalf("update = %+v", ve.u)
	}
	// The download path the backend named, made absolute against the backend
	// the client is talking to — and carrying the token, because the release
	// endpoint is behind sign-in.
	if ve.u.URL != "https://john.acme.example/releases/john-darwin-arm64" {
		t.Fatalf("download URL = %q", ve.u.URL)
	}
	if ve.u.Token != "machine-token" {
		t.Fatalf("token = %q", ve.u.Token)
	}
	if !strings.Contains(ve.Error(), "0.5.2") || !strings.Contains(ve.Error(), "0.4.1") {
		t.Fatalf("error does not name both versions: %v", ve)
	}
}

// framePair is an in-memory Transport pair, enough to drive one handshake.
type framePair struct {
	in  chan []byte
	out chan []byte
}

func newFramePair() (*framePair, *framePair) {
	ab, ba := make(chan []byte, 4), make(chan []byte, 4)
	return &framePair{in: ba, out: ab}, &framePair{in: ab, out: ba}
}

func (p *framePair) ReadFrame(ctx context.Context) ([]byte, error) {
	select {
	case f := <-p.in:
		return f, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (p *framePair) WriteFrame(ctx context.Context, data []byte) error {
	select {
	case p.out <- data:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (p *framePair) Close() error { return nil }

// The update asks for the build gzipped, installs what is inside, and says
// how big it is while it fetches it.
func TestAnUpdateDownloadsTheGzippedBuild(t *testing.T) {
	build := bytes.Repeat([]byte("john"), 1<<19) // 2 MB, past the truncation floor
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	zw.Write(build)
	zw.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Accept"), "application/gzip") {
			t.Errorf("the update did not ask for gzip: Accept %q", r.Header.Get("Accept"))
		}
		w.Header().Set("Content-Type", "application/gzip")
		w.Header().Set("Content-Length", strconv.Itoa(gz.Len()))
		w.Write(gz.Bytes())
	}))
	t.Cleanup(srv.Close)

	u := &update{Required: "0.5.2", Current: "0.4.1", URL: srv.URL}
	var out bytes.Buffer
	tmp, err := u.download(filepath.Join(t.TempDir(), "john"), &out)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(tmp)
	if !bytes.Equal(got, build) {
		t.Fatalf("installed %d bytes, want the %d inside the gzip", len(got), len(build))
	}
	// Not a terminal, so one line with the size rather than a redraw per tick.
	if want := "Downloading john 0.5.2… (0.0 MB)\n"; out.String() != want {
		t.Fatalf("progress said %q, want %q", out.String(), want)
	}
}

func TestTheProgressLineSaysHowLongIsLeft(t *testing.T) {
	p := &progress{label: "Downloading john 0.5.2…", total: 40 << 20, n: 10 << 20,
		start: time.Now().Add(-5 * time.Second)}
	if got, want := p.line(), "Downloading john 0.5.2… 10.0 of 40.0 MB (25%) · 2.0 MB/s · 15s left"; got != want {
		t.Fatalf("line = %q\n want %q", got, want)
	}
	p.n = p.total
	if got := p.line(); !strings.HasSuffix(got, "(100%) · done in 5s") {
		t.Fatalf("finished line = %q", got)
	}
}
