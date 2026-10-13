package sandbox

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/popstarapps/john/policy"
)

func TestAProcessorIsRecognisedByEverySpellingOfIt(t *testing.T) {
	cases := map[string]string{
		"aarch64": "arm64", "arm64": "arm64", "ARM64": "arm64", " aarch64 ": "arm64",
		"x86_64": "amd64", "amd64": "amd64", "AMD64": "amd64", "x64": "amd64",
		// Not recognised is not the same as mismatched: a spelling this build
		// does not know reads as "cannot tell", and refusing to start over one
		// would be worse than the problem it guards against.
		"riscv64": "", "i686": "", "": "",
	}
	for name, want := range cases {
		if got := normalizeArch(name); got != want {
			t.Errorf("normalizeArch(%q) = %q, want %q", name, got, want)
		}
	}
}

// This machine runs the build it was given, so the check has to pass here —
// what it exists for is the Rosetta case, where an amd64 build of john runs on
// Apple Silicon and would ask the hypervisor for an x86 guest on an ARM
// processor.
func TestTheArchitectureCheckPassesOnAMatchingMachine(t *testing.T) {
	host, err := hostArch()
	if err != nil {
		t.Fatalf("hostArch: %v", err)
	}
	if host != "" && host != runtime.GOARCH {
		t.Skipf("this build (%s) is not native to this machine (%s)", runtime.GOARCH, host)
	}
	if err := hostSupportsGuestArch(); err != nil {
		t.Fatalf("refused a matching machine: %v", err)
	}
}

// A build with no image says so plainly rather than failing somewhere further
// in. A plain `go build` produces one of these on purpose, so that a quick
// compile does not need an image to have been made first.
func TestABuildWithNoImageSaysSo(t *testing.T) {
	if guestFS != nil {
		t.Skip("this build carries an image")
	}
	_, err := guestImage(t.TempDir(), "v1")
	if !errors.Is(err, ErrNoGuestImage) {
		t.Fatalf("guestImage without an image = %v, want ErrNoGuestImage", err)
	}
}

// The image is extracted once per release and reused, and an upgraded client
// writes a new one beside the old rather than over it — a machine that is
// still running may have the old file open underneath it.
func TestTheImageIsExtractedOncePerReleaseAndKeptApart(t *testing.T) {
	if guestFS == nil {
		t.Skip("build with -tags embed_guest to exercise the embedded image")
	}
	dir := t.TempDir()

	first, err := guestImage(dir, "v1")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{first.Kernel, first.Initramfs} {
		info, err := os.Stat(p)
		if err != nil || info.Size() == 0 {
			t.Fatalf("%s: %v", p, err)
		}
	}
	// The kernel travels compressed and comes out ready to boot.
	if !strings.HasPrefix(filepath.Base(first.Kernel), "kernel") {
		t.Fatalf("kernel extracted as %q", first.Kernel)
	}

	// Asking again reuses what is there rather than writing it out again.
	stamp, err := os.Stat(first.Kernel)
	if err != nil {
		t.Fatal(err)
	}
	again, err := guestImage(dir, "v1")
	if err != nil || again != first {
		t.Fatalf("second call = %+v %v, want the same paths", again, err)
	}
	if s, _ := os.Stat(again.Kernel); !s.ModTime().Equal(stamp.ModTime()) {
		t.Error("the image was extracted a second time")
	}

	// A different release is a different image, kept apart.
	next, err := guestImage(dir, "v2")
	if err != nil {
		t.Fatal(err)
	}
	if next.Kernel == first.Kernel {
		t.Fatal("two releases share one extracted image")
	}
	if _, err := os.Stat(first.Kernel); err != nil {
		t.Fatalf("the older release's image was removed: %v", err)
	}

	// Nothing half-written is left behind by a completed extraction.
	entries, _ := os.ReadDir(filepath.Dir(next.Kernel))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".partial-") {
			t.Errorf("a partial file survived extraction: %s", e.Name())
		}
	}
}

// The whole shipping path: a binary that carries its own image extracts it and
// boots a machine from it, with nothing supplied from outside. This is what a
// developer's first run does, on every machine this host can start one on.
func TestABinaryBootsTheImageItCarries(t *testing.T) {
	if guestFS == nil {
		t.Skip("build with -tags embed_guest to exercise the embedded image")
	}

	// Deliberately no Kernel and no Initramfs on either harness: they have to
	// come out of the binary.
	var machines []realMachine
	if bin := os.Getenv("JOHN_QEMU"); bin != "" {
		q := &qemu{Binary: bin, Console: filepath.Join(t.TempDir(), "console.log")}
		if env := os.Getenv("JOHN_QEMU_ENV"); env != "" {
			q.Env = strings.Split(env, " ")
		}
		machines = append(machines, realMachine{name: q.Name(), hv: q, console: q.Console})
	}
	if hv, console := platformHarnessBare(t); hv != nil {
		machines = append(machines, realMachine{name: hv.Name(), hv: hv, console: console})
	}
	if len(machines) == 0 {
		t.Skip("nothing here boots a real kernel")
	}

	for _, m := range machines {
		t.Run(m.name, func(t *testing.T) {
			vm, workdir := bootRealMachine(t, m,
				hostTree(t, map[string]string{"main.go": "package main\n"}), policy.Protections{})
			if got := strings.TrimSpace(string(run(t, vm, "cat main.go").Stdout)); got != "package main" {
				t.Fatalf("cat main.go = %q", got)
			}
			// The host's own path, in the spelling the sandbox mirrors it
			// under — which is the path itself everywhere but Windows, whose
			// C:\Users\bob\project Linux cannot hold.
			want := GuestMount(workdir)
			if got := strings.TrimSpace(string(run(t, vm, "pwd").Stdout)); got != want {
				t.Fatalf("pwd = %q, want %q", got, want)
			}
		})
	}
}
