package backend

import (
	"io"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// consoleHandler serves the embedded console as a single-page app: real files
// are served as themselves, and every other path falls back to index.html,
// because deep links like /admin/teams/eng-platform are real URLs the router
// resolves in the browser.
func consoleHandler(root fs.FS) http.Handler {
	fileServer := http.FileServerFS(root)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		p := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if p != "" && p != "." {
			if f, err := root.Open(p); err == nil {
				f.Close()
				fileServer.ServeHTTP(w, r)
				return
			}
		}
		serveIndex(w, r, root)
	})
}

func serveIndex(w http.ResponseWriter, r *http.Request, root fs.FS) {
	f, err := root.Open("index.html")
	if err != nil {
		http.Error(w, "console not built — run `make console` and rebuild", http.StatusNotFound)
		return
	}
	defer f.Close()
	// The SPA shell must never be cached: its asset URLs are content-hashed
	// and change on every deploy.
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if r.Method == http.MethodHead {
		return
	}
	io.Copy(w, f)
}
