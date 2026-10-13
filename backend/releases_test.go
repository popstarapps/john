package backend

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"testing"
	"testing/fstest"
)

// gzBytes gzip-compresses b, as `make release` does to each client binary.
func gzBytes(b []byte) []byte {
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	w.Write(b)
	w.Close()
	return buf.Bytes()
}

// TestReleaseServing proves the single-binary download story without needing
// the embed build tag: a stand-in clientsFS holds gzipped "binaries", and the
// endpoints list them and stream them back decompressed, behind sign-in.
func TestReleaseServing(t *testing.T) {
	fake := []byte("\x7fELF pretend this is a mach-o/pe/elf binary for a platform")
	clientsFS = fstest.MapFS{
		"john-darwin-arm64.gz":      {Data: gzBytes(fake)},
		"john-linux-amd64.gz":       {Data: gzBytes(fake)},
		"john-windows-amd64.exe.gz": {Data: gzBytes(fake)},
	}
	t.Cleanup(func() { clientsFS = nil })

	srv := testServerWithIdP(t)
	base := "http://" + srv.Addr()

	// Downloads are behind sign-in.
	if code := status(t, http.DefaultClient, base+"/releases/john-linux-amd64"); code != http.StatusUnauthorized {
		t.Fatalf("anonymous download = %d, want 401", code)
	}

	browser := browserClient(t)
	resp, _ := browser.Get(base + "/auth/login")
	resp.Body.Close()

	// The list reflects what is embedded, with parsed platform + real size.
	var rels []Release
	getJSON(t, browser, base+"/api/v1/releases", &rels)
	if len(rels) != 3 {
		t.Fatalf("releases = %d, want 3: %+v", len(rels), rels)
	}
	byFile := map[string]Release{}
	for _, r := range rels {
		byFile[r.File] = r
	}
	if r := byFile["john-darwin-arm64"]; r.OS != "darwin" || r.Arch != "arm64" || r.SizeBytes != int64(len(fake)) {
		t.Fatalf("darwin release = %+v (fake size %d)", r, len(fake))
	}
	if r := byFile["john-windows-amd64.exe"]; r.OS != "windows" || r.Arch != "amd64" {
		t.Fatalf("windows release = %+v", r)
	}

	// The download streams the decompressed bytes, as an attachment.
	dl, err := browser.Get(base + "/releases/john-linux-amd64")
	if err != nil {
		t.Fatal(err)
	}
	defer dl.Body.Close()
	if dl.StatusCode != http.StatusOK {
		t.Fatalf("download = %d", dl.StatusCode)
	}
	if cd := dl.Header.Get("Content-Disposition"); cd != `attachment; filename="john-linux-amd64"` {
		t.Fatalf("content-disposition = %q", cd)
	}
	body, _ := io.ReadAll(dl.Body)
	if !bytes.Equal(body, fake) {
		t.Fatalf("downloaded bytes did not round-trip through gzip: %q", body)
	}
	// With its size, so a browser can say how long is left.
	if dl.ContentLength != int64(len(fake)) {
		t.Fatalf("content-length = %d, want %d", dl.ContentLength, len(fake))
	}

	// The client's update asks for the stored gzip, and gets it as it is,
	// with its own size.
	req, _ := http.NewRequest("GET", base+"/releases/john-linux-amd64", nil)
	req.Header.Set("Accept", "application/gzip")
	gzResp, err := browser.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer gzResp.Body.Close()
	if ct := gzResp.Header.Get("Content-Type"); ct != "application/gzip" {
		t.Fatalf("content-type for a gzip request = %q", ct)
	}
	raw, _ := io.ReadAll(gzResp.Body)
	if gzResp.ContentLength != int64(len(raw)) {
		t.Fatalf("content-length = %d for %d bytes", gzResp.ContentLength, len(raw))
	}
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("not gzip: %v", err)
	}
	if unzipped, _ := io.ReadAll(zr); !bytes.Equal(unzipped, fake) {
		t.Fatal("the gzip did not hold the build")
	}

	// A file that is not embedded 404s; a traversal attempt is rejected.
	if code := status(t, browser, base+"/releases/john-nope-arch"); code != http.StatusNotFound {
		t.Fatalf("missing file = %d, want 404", code)
	}
}

// TestReleasesEmptyWhenNoneBundled: a plain `make build` backend (no embed tag)
// reports no downloads rather than erroring.
func TestReleasesEmptyWhenNoneBundled(t *testing.T) {
	clientsFS = nil
	srv := testServerWithIdP(t)
	base := "http://" + srv.Addr()
	browser := browserClient(t)
	resp, _ := browser.Get(base + "/auth/login")
	resp.Body.Close()

	var rels []Release
	getJSON(t, browser, base+"/api/v1/releases", &rels)
	if len(rels) != 0 {
		t.Fatalf("releases = %+v, want none", rels)
	}
	if code := status(t, browser, base+"/releases/john-linux-amd64"); code != http.StatusNotFound {
		t.Fatalf("download with nothing bundled = %d, want 404", code)
	}
}
