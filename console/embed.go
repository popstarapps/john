// Package console embeds the built web console so the john binary is the
// only artifact an operator deploys. dist/ is produced by `make console`
// (vite build) and is otherwise untracked; only dist/.gitkeep is committed, so
// that a clone which has never run node still compiles. Such a binary serves
// no console — backend.serveIndex answers "console not built" instead.
package console

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var dist embed.FS

// FS returns the built console as a filesystem rooted at the site root.
func FS() fs.FS {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		panic(err) // the embed directive guarantees dist exists
	}
	return sub
}
