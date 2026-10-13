//go:build embed_guest

package sandbox

import (
	"embed"
	"io/fs"
)

// embedded_guest/ is populated by `make clients` with the image for the
// platform being built — kernel.gz and initramfs.cpio.gz, and only for that
// one architecture. This file, and the embed, are compiled in only under the
// embed_guest build tag, so a plain `go build` never needs the directory to
// hold anything.

//go:embed all:embedded_guest
var embeddedGuest embed.FS

func init() {
	if sub, err := fs.Sub(embeddedGuest, "embedded_guest"); err == nil {
		guestFS = sub
	}
}
