//go:build embed_clients

package backend

import (
	"embed"
	"io/fs"
)

// embedded_clients/ is populated by `make release` with one gzip-compressed
// client build per platform (john-<os>-<arch>[.exe].gz). This file, and the
// embed, are compiled in only under the embed_clients build tag, so a plain
// `go build` never needs the directory to exist.

//go:embed all:embedded_clients
var embeddedClients embed.FS

func init() {
	if sub, err := fs.Sub(embeddedClients, "embedded_clients"); err == nil {
		clientsFS = sub
	}
}
