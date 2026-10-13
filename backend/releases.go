package backend

import (
	"compress/gzip"
	"encoding/binary"
	"io"
	"io/fs"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// The backend hands out the client for every platform, from inside its own
// single binary. Only the backend needs this — a client never serves other
// clients — so a binary can embed the OTHER platforms' builds without any
// circularity: nothing embeds a copy of itself. The builds are gzip-compressed
// at `make release` and decompressed on the way out.
//
// clientsFS holds them, rooted so its entries are `john-<os>-<arch>[.exe].gz`.
// It is nil unless the binary was built with `-tags embed_clients` (which
// `make release` does); a plain `make build` leaves downloads unbundled and
// the endpoints say so, which keeps day-to-day builds fast.
var clientsFS fs.FS

// Release is one downloadable client build.
type Release struct {
	File      string `json:"file"`      // john-darwin-arm64 (no .gz; what the client saves)
	OS        string `json:"os"`        // darwin | linux | windows
	Arch      string `json:"arch"`      // amd64 | arm64
	SizeBytes int64  `json:"sizeBytes"` // uncompressed
}

// releases lists the embedded client builds.
func releases() []Release {
	if clientsFS == nil {
		return nil
	}
	entries, err := fs.ReadDir(clientsFS, ".")
	if err != nil {
		return nil
	}
	var out []Release
	for _, e := range entries {
		name := e.Name() // john-<os>-<arch>[.exe].gz
		if e.IsDir() || !strings.HasSuffix(name, ".gz") {
			continue
		}
		file := strings.TrimSuffix(name, ".gz")
		os, arch := parsePlatform(file)
		if os == "" {
			continue
		}
		out = append(out, Release{File: file, OS: os, Arch: arch, SizeBytes: uncompressedSize(name)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].File < out[j].File })
	return out
}

// parsePlatform pulls os/arch out of john-<os>-<arch>[.exe].
func parsePlatform(file string) (os, arch string) {
	base := strings.TrimSuffix(strings.TrimPrefix(file, "john-"), ".exe")
	parts := strings.Split(base, "-")
	if len(parts) != 2 {
		return "", ""
	}
	return parts[0], parts[1]
}

// uncompressedSize reads the gzip footer's ISIZE (little-endian, last 4 bytes).
func uncompressedSize(gzName string) int64 {
	data, err := fs.ReadFile(clientsFS, gzName)
	if err != nil || len(data) < 4 {
		return 0
	}
	return int64(binary.LittleEndian.Uint32(data[len(data)-4:]))
}

// handleListReleases is GET /api/v1/releases: what the console's download page
// renders. Requires a signed-in user, because downloads come from the console.
func (s *Server) handleListReleases(w http.ResponseWriter, r *http.Request) {
	if s.requireUser(w, r) == nil {
		return
	}
	rels := releases()
	if rels == nil {
		rels = []Release{}
	}
	writeJSON(w, http.StatusOK, rels)
}

// handleDownloadRelease is GET /releases/{file}: it streams one client build.
// Requires a session so downloads stay behind sign-in.
//
// A browser gets the binary, decompressed. The client's own update asks for
// application/gzip and gets the stored bytes as they are: a sixth of the size,
// which over a slow link is most of the wait. Both carry Content-Length, so
// both can say how far along they are.
func (s *Server) handleDownloadRelease(w http.ResponseWriter, r *http.Request) {
	if s.requireUser(w, r) == nil {
		return
	}
	file := r.PathValue("file")
	if strings.Contains(file, "/") || strings.Contains(file, "..") {
		writeErr(w, http.StatusBadRequest, "bad file")
		return
	}
	if clientsFS == nil {
		writeErr(w, http.StatusNotFound, "no client downloads bundled in this backend build")
		return
	}
	f, err := clientsFS.Open(file + ".gz")
	if err != nil {
		writeErr(w, http.StatusNotFound, "no such client build")
		return
	}
	defer f.Close()

	if strings.Contains(r.Header.Get("Accept"), "application/gzip") {
		if info, err := f.Stat(); err == nil {
			w.Header().Set("Content-Length", strconv.FormatInt(info.Size(), 10))
		}
		w.Header().Set("Content-Type", "application/gzip")
		io.Copy(w, f)
		return
	}

	gz, err := gzip.NewReader(f)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "corrupt client build")
		return
	}
	defer gz.Close()
	if n := uncompressedSize(file + ".gz"); n > 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(n, 10))
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", "attachment; filename=\""+file+"\"")
	io.Copy(w, gz)
}

// releaseFor finds the client build for a platform the client named as
// "darwin/arm64", returning the download path or "" when this backend has no
// build for it. `make build` bundles no clients at all, and a backend that
// cannot hand out a fix has no business refusing a session over the version.
func releaseFor(osArch string) string {
	goos, arch, ok := strings.Cut(osArch, "/")
	if !ok {
		return ""
	}
	for _, r := range releases() {
		if r.OS == goos && r.Arch == arch {
			return "/releases/" + r.File
		}
	}
	return ""
}
