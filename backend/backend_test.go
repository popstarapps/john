package backend

import (
	"context"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/popstarapps/john/console"
)

// requireConsole skips a test that needs the built web console. console/dist
// is a build artifact (`make console`); a clone that has never run node embeds
// only the .gitkeep marker, and such a test would fail for the wrong reason.
func requireConsole(t *testing.T) {
	t.Helper()
	f, err := console.FS().Open("index.html")
	if err != nil {
		t.Skip("console not built — run `make console`")
	}
	f.Close()
}

func testServer(t *testing.T) *Server {
	t.Helper()
	srv, err := New(Config{
		DBPath:  filepath.Join(t.TempDir(), "john.db"),
		Listen:  "127.0.0.1:0",
		Version: "test",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.Serve(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
		srv.Close()
	})
	return srv
}

func get(t *testing.T, url string) (int, string, http.Header) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body), resp.Header
}

func TestBackendBootsAndCreatesDatabase(t *testing.T) {
	srv := testServer(t)
	if srv.Addr() == "" {
		t.Fatal("no bound address")
	}
	if v, err := srv.Store().SchemaVersion(); err != nil || v < 1 {
		t.Fatalf("schema version %d, err %v", v, err)
	}
}

func TestConsoleIsServedWithSPAFallback(t *testing.T) {
	requireConsole(t)
	srv := testServer(t)
	base := "http://" + srv.Addr()

	status, body, _ := get(t, base+"/")
	if status != http.StatusOK || !strings.Contains(body, "<html") {
		t.Fatalf("GET / = %d, body %.60q", status, body)
	}

	// A deep link the router resolves client-side must serve the shell, not 404.
	status, deep, _ := get(t, base+"/admin/teams/eng-platform")
	if status != http.StatusOK || deep != body {
		t.Fatalf("deep link = %d, shell mismatch %v", status, deep != body)
	}

	// The API namespace must never fall back to HTML: a typo'd endpoint
	// answering 200 text/html is how a client quietly parses garbage.
	status, apiBody, _ := get(t, base+"/api/v1/nope")
	if status == http.StatusOK || strings.Contains(apiBody, "<html") {
		t.Fatalf("GET /api/v1/nope = %d, body %.60q — API paths must not serve the SPA", status, apiBody)
	}
}

func TestViewerBeforeSetupSaysSetupRequired(t *testing.T) {
	srv := testServer(t)
	status, body, _ := get(t, "http://"+srv.Addr()+"/api/v1/viewer")
	if status != http.StatusServiceUnavailable || !strings.Contains(body, "setup_required") {
		t.Fatalf("GET /api/v1/viewer = %d %q, want 503 setup_required", status, body)
	}
}
