package sandbox

import (
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
)

// The image the sandbox boots: a Linux kernel and an initramfs holding an
// init, a shell and the agent. It travels inside the john binary.
//
// Embedding it rather than fetching it from the backend is what makes a first
// run work with nothing but the binary — no download, no cache to warm, no
// half-fetched image to recover from — and it means `john apply` works on a
// machine that has never reached a backend. It costs nothing in consistency
// either, because the version handshake already refuses a client whose build
// does not match the backend's: there is no state in which a machine could
// usefully hold an image from a different release than its client.
//
// A client embeds the image for the architecture it was built for, and only
// that one. The machines john ships virtualize rather than emulate, so the
// guest's architecture is the host CPU's — an Apple Silicon Mac boots an arm64
// guest, an x86-64 Windows or Linux machine boots an amd64 one. Five client
// builds, two images.
//
// guestFS is nil unless the binary was built with `-tags embed_guest`, which
// `make build` and `make clients` do. A plain `go build` leaves it out, so a
// quick compile does not need an image to have been made first; the sandbox
// then says so rather than failing somewhere further in.
var guestFS fs.FS

// GuestImage is an extracted image on disk, ready for a machine to boot.
type GuestImage struct {
	Kernel    string
	Initramfs string
}

// ErrNoGuestImage means this build carries no image for this machine.
var ErrNoGuestImage = errors.New("sandbox: this build carries no guest image")

// guestImage extracts the embedded image into dir and returns the paths,
// reusing what is already there. Extraction is once per release: the files are
// named by the version they came from, so an upgraded client writes new ones
// beside the old rather than over them, and a machine that is still running on
// the old image keeps the file underneath it.
func guestImage(dir, version string) (GuestImage, error) {
	if guestFS == nil {
		return GuestImage{}, ErrNoGuestImage
	}
	if err := hostSupportsGuestArch(); err != nil {
		return GuestImage{}, err
	}

	out := filepath.Join(dir, "images", version+"-"+runtime.GOARCH)
	img := GuestImage{
		Kernel:    filepath.Join(out, "kernel"),
		Initramfs: filepath.Join(out, "initramfs.cpio.gz"),
	}
	if fileExists(img.Kernel) && fileExists(img.Initramfs) {
		return img, nil
	}
	if err := os.MkdirAll(out, 0o700); err != nil {
		return GuestImage{}, err
	}
	// The kernel is stored compressed and the initramfs already is, so one is
	// decompressed on the way out and the other copied.
	if err := extractGuestFile("kernel.gz", img.Kernel, true); err != nil {
		return GuestImage{}, err
	}
	if err := extractGuestFile("initramfs.cpio.gz", img.Initramfs, false); err != nil {
		return GuestImage{}, err
	}
	return img, nil
}

// extractGuestFile writes one embedded file out, through a temporary name so
// that an interrupted extraction never leaves a half-written image looking
// like a finished one.
func extractGuestFile(name, dst string, decompress bool) error {
	src, err := guestFS.Open(name)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrNoGuestImage, err)
	}
	defer src.Close()

	var r io.Reader = src
	if decompress {
		zr, err := gzip.NewReader(src)
		if err != nil {
			return err
		}
		defer zr.Close()
		r = zr
	}

	tmp, err := os.CreateTemp(filepath.Dir(dst), ".partial-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := io.Copy(tmp, r); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), dst)
}

func fileExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.Mode().IsRegular() && info.Size() > 0
}

// hostSupportsGuestArch refuses when this binary's architecture is not the
// machine's own.
//
// The case that matters is Rosetta: an x86-64 build of john running on Apple
// Silicon reports amd64 for itself, carries the amd64 image, and would ask the
// hypervisor to boot an x86 guest on an ARM processor — which it cannot do.
// Caught here it is one sentence telling the developer to install the right
// build; caught by the hypervisor it is a boot failure with no explanation.
func hostSupportsGuestArch() error {
	host, err := hostArch()
	if err != nil || host == "" || host == runtime.GOARCH {
		// An architecture this build cannot identify is not a reason to
		// refuse: the overwhelmingly likely case is that it matches.
		return nil
	}
	return fmt.Errorf("this is the %s build of john, but the machine is %s — "+
		"the sandbox is a virtual machine and cannot run a guest for a different processor. "+
		"Install the %s build", runtime.GOARCH, host, host)
}
