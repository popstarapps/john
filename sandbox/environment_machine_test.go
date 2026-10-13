package sandbox

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/popstarapps/john/policy"
)

// The environment, built and entered, inside a real machine.
//
// This is the piece that makes the sandbox somewhere a project can be worked
// on rather than an empty kernel: the project's .john/Dockerfile becomes a
// root filesystem on the sandbox's disk, and `run` executes inside it. It
// proves the whole path — the Dockerfile parsed, the base image pulled from a
// registry, its layers unpacked with their permissions, RUN executed in a
// chroot, COPY taken from the build context, and ENV reaching a later command.
//
// The base image is served from a registry in this process and built out of
// the busybox the guest image already carries, so nothing here reaches the
// network and nothing depends on a public image staying where it was.
func TestTheProjectsEnvironmentIsBuiltAndCommandsRunInsideIt(t *testing.T) {
	layer, ok := busyboxLayer(t)
	if !ok {
		t.Skip("no guest image built; see sandbox/guest/mkimage.sh")
	}
	registry := serveImage(t, layer)

	// The environment needs a route into the machine for nothing here — the
	// pull is on the host — but it does need a machine that is Linux inside,
	// which the qemu harness is too, so every real machine will do.
	real := realMachines(t)
	if len(real) == 0 {
		t.Skip("nothing here boots a real kernel")
	}

	files := map[string]string{
		"main.go":         "package main\n",
		".john/setup.txt": "from-the-context\n",
		EnvFile: "FROM " + registry + "/library/base:v1\n" +
			"ENV GREETING=hello-from-env\n" +
			"RUN mkdir -p /opt && echo built-by-run > /opt/marker\n" +
			"COPY setup.txt /opt/copied.txt\n",
	}

	for _, m := range real {
		t.Run(m.name, func(t *testing.T) {
			vm, _ := bootRealMachine(t, m, hostTree(t, files), policy.Protections{})
			needsAShell(t, vm)

			// RUN executed inside the environment, not in the initramfs.
			if got := strings.TrimSpace(string(run(t, vm, "cat /opt/marker").Stdout)); got != "built-by-run" {
				t.Errorf("RUN did not land in the environment: %q", got)
			}
			// COPY took the file from .john/, the build context.
			if got := strings.TrimSpace(string(run(t, vm, "cat /opt/copied.txt").Stdout)); got != "from-the-context" {
				t.Errorf("COPY did not land in the environment: %q", got)
			}
			// ENV from the Dockerfile reaches a command run later.
			if got := strings.TrimSpace(string(run(t, vm, "echo $GREETING").Stdout)); got != "hello-from-env" {
				t.Errorf("ENV did not reach the session: %q", got)
			}
			// And the workspace is still where it belongs, inside the
			// environment's root, at the host's own path.
			if got := strings.TrimSpace(string(run(t, vm, "cat main.go").Stdout)); got != "package main" {
				t.Errorf("the workspace is not readable inside the environment: %q", got)
			}

			// A second Build finds the environment already installed and does
			// not fetch it again — the identity is the build context's
			// content, so an unchanged definition is an unchanged sandbox.
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			before := requests.Load()
			if err := vm.Build(ctx, nil); err != nil {
				t.Fatal(err)
			}
			if got := requests.Load() - before; got != 0 {
				t.Errorf("a rebuild of an unchanged environment made %d registry requests", got)
			}
		})
	}
}

// busyboxLayer makes an image layer out of the guest image's own busybox: a
// shell, the loader it needs, and the symlinks that make `sh` and `echo` work.
func busyboxLayer(t *testing.T) ([]byte, bool) {
	t.Helper()
	arch := "aarch64"
	if runtime.GOARCH == "amd64" {
		arch = "x86_64"
	}
	// mkimage.sh leaves the unpacked initramfs beside the image it built, and
	// the image is what the tests are already pointed at. Falling back to the
	// two obvious relative paths covers `go test ./sandbox` as well as a test
	// binary run from the repository root.
	var roots []string
	if img := os.Getenv("JOHN_GUEST_INITRAMFS"); img != "" {
		roots = append(roots, filepath.Join(filepath.Dir(img), "build", "initramfs"))
	}
	roots = append(roots,
		filepath.Join("build", "guest", arch, "build", "initramfs"),
		filepath.Join("..", "build", "guest", arch, "build", "initramfs"))

	var busybox, loader []byte
	for _, base := range roots {
		b, err := os.ReadFile(filepath.Join(base, "bin", "busybox"))
		if err != nil {
			continue
		}
		l, err := os.ReadFile(filepath.Join(base, "lib", "ld-musl-"+arch+".so.1"))
		if err != nil {
			continue
		}
		busybox, loader = b, l
		break
	}
	if busybox == nil {
		return nil, false
	}

	var raw bytes.Buffer
	zw := gzip.NewWriter(&raw)
	tw := tar.NewWriter(zw)
	dir := func(name string) {
		tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeDir, Mode: 0o755})
	}
	file := func(name string, mode int64, data []byte) {
		tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: mode, Size: int64(len(data))})
		tw.Write(data)
	}
	link := func(name, target string) {
		tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeSymlink, Linkname: target, Mode: 0o777})
	}
	for _, d := range []string{"bin/", "lib/", "usr/", "usr/bin/", "etc/", "opt/", "proc/", "sys/", "dev/", "tmp/", "root/"} {
		dir(d)
	}
	file("bin/busybox", 0o755, busybox)
	file("lib/ld-musl-"+arch+".so.1", 0o755, loader)
	link("lib/libc.musl-"+arch+".so.1", "ld-musl-"+arch+".so.1")
	for _, applet := range []string{"sh", "echo", "cat", "mkdir", "ls", "env", "chmod", "rm", "sed", "grep", "wget", "ip"} {
		link("bin/"+applet, "busybox")
	}
	file("etc/passwd", 0o644, []byte("root:x:0:0:root:/root:/bin/sh\n"))
	tw.Close()
	zw.Close()
	return raw.Bytes(), true
}

// requests counts what the registry was asked for, so a rebuild that should
// have fetched nothing can be shown to have fetched nothing.
var requests atomicCounter

// serveImage runs a registry in this process and returns its host:port.
func serveImage(t *testing.T, layer []byte) string {
	t.Helper()
	sum := func(b []byte) string {
		h := sha256.Sum256(b)
		return "sha256:" + hex.EncodeToString(h[:])
	}
	config := []byte(`{"config":{"Env":["PATH=/bin:/usr/bin","HOME=/root"],"WorkingDir":"/"}}`)
	layerDigest, configDigest := sum(layer), sum(config)
	manifest := []byte(fmt.Sprintf(
		`{"mediaType":%q,"config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":%q,"size":%d},`+
			`"layers":[{"mediaType":"application/vnd.oci.image.layer.v1.tar+gzip","digest":%q,"size":%d}]}`,
		mediaOCIManifest, configDigest, len(config), layerDigest, len(layer)))
	index := []byte(fmt.Sprintf(
		`{"mediaType":%q,"manifests":[{"mediaType":%q,"digest":%q,"size":%d,"platform":{"os":"linux","architecture":%q}}]}`,
		mediaOCIIndex, mediaOCIManifest, sum(manifest), len(manifest), runtime.GOARCH))

	blobs := map[string][]byte{layerDigest: layer, configDigest: config}

	mux := http.NewServeMux()
	mux.HandleFunc("/v2/", func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		name := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		switch {
		case strings.Contains(r.URL.Path, "/manifests/"):
			if name == sum(manifest) {
				w.Header().Set("Content-Type", mediaOCIManifest)
				w.Write(manifest)
				return
			}
			w.Header().Set("Content-Type", mediaOCIIndex)
			w.Write(index)
		case strings.Contains(r.URL.Path, "/blobs/"):
			if b, ok := blobs[name]; ok {
				w.Write(b)
				return
			}
			http.NotFound(w, r)
		default:
			json.NewEncoder(w).Encode(map[string]string{})
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://")
}

type atomicCounter struct{ n atomic.Int64 }

func (c *atomicCounter) Add(d int64) { c.n.Add(d) }
func (c *atomicCounter) Load() int64 { return c.n.Load() }

// Changing the definition rebuilds the environment and keeps the work.
//
// This is the property a container cannot offer. The environment is identified
// by the content of its whole build context, so editing .john/Dockerfile makes
// it a different environment and the next start builds it — and the sandbox's
// own layer, including everything the agent has written and not yet applied,
// is untouched by that. A developer who adds a package to their toolchain does
// not lose an afternoon's work to it.
func TestChangingTheDefinitionRebuildsTheEnvironmentAndKeepsTheWork(t *testing.T) {
	layer, ok := busyboxLayer(t)
	if !ok {
		t.Skip("no guest image built; see sandbox/guest/mkimage.sh")
	}
	registry := serveImage(t, layer)
	machines := realMachines(t)
	if len(machines) == 0 {
		t.Skip("nothing here boots a real kernel")
	}

	dockerfile := func(marker string) string {
		return "FROM " + registry + "/library/base:v1\n" +
			"RUN mkdir -p /opt && echo " + marker + " > /opt/" + marker + "\n"
	}

	for _, m := range machines {
		t.Run(m.name, func(t *testing.T) {
			workdir := hostTree(t, map[string]string{
				"main.go": "package main\n",
				EnvFile:   dockerfile("first"),
			})
			// One state directory across both starts: this is the same
			// sandbox being restarted, not a new one.
			state := t.TempDir()
			ctx := context.Background()

			boot := func(want string) *VM {
				vm, err := NewVM(workdir, VMOptions{StateDir: state, Hypervisor: m.hv, Instance: want})
				if err != nil {
					t.Fatal(err)
				}
				if err := vm.SetResources(Resources{CPUs: 2, MemoryGB: 2, DiskGB: testDiskGB, Processes: 256}); err != nil {
					t.Fatal(err)
				}
				bctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
				defer cancel()
				if err := vm.Build(bctx, nil); err != nil {
					if log, rerr := os.ReadFile(m.console); rerr == nil {
						t.Logf("guest console:\n%s", log)
					}
					t.Fatalf("build: %v", err)
				}
				return vm
			}

			vm := boot("")
			needsAShell(t, vm)
			if got := strings.TrimSpace(string(run(t, vm, "cat /opt/first").Stdout)); got != "first" {
				t.Fatalf("the first environment was not built: %q", got)
			}
			// Work in progress, in the sandbox and not on the host.
			run(t, vm, "echo 'half a day of work' > notes.txt")
			if err := vm.Stop(ctx); err != nil {
				t.Fatal(err)
			}

			// The developer adds something to their toolchain.
			if err := os.WriteFile(filepath.Join(workdir, filepath.FromSlash(EnvFile)),
				[]byte(dockerfile("second")), 0o644); err != nil {
				t.Fatal(err)
			}

			vm = boot("")
			t.Cleanup(func() { vm.Stop(context.Background()) })

			// The environment is the new one, and only the new one.
			if got := strings.TrimSpace(string(run(t, vm, "cat /opt/second").Stdout)); got != "second" {
				t.Errorf("the changed definition was not built: %q", got)
			}
			if res := run(t, vm, "cat /opt/first 2>&1"); res.ExitCode == 0 {
				t.Errorf("the old environment survived the rebuild: %q", res.Stdout)
			}

			// And the work is still there, still unapplied.
			if got := strings.TrimSpace(string(run(t, vm, "cat notes.txt").Stdout)); got != "half a day of work" {
				t.Errorf("an environment rebuild lost the sandbox's own work: %q", got)
			}
			if _, err := os.Stat(filepath.Join(workdir, "notes.txt")); !os.IsNotExist(err) {
				t.Error("the work reached the host without an apply")
			}
			diffs, err := vm.Diff(ctx)
			if err != nil || len(diffs) != 1 || diffs[0].Path != "notes.txt" {
				t.Errorf("diff after a rebuild = %+v %v", diffs, err)
			}
		})
	}
}

// Two sandboxes in one workspace share the environment and share nothing else.
//
// The environment is the expensive part and is identical for both, so it is
// built once and mounted by both — the second sandbox fetches nothing. What it
// must not share is what a session writes: two agents doing different things
// each get their own layer over it, and neither sees the other's.
func TestTwoSandboxesShareTheEnvironmentAndNotTheirWork(t *testing.T) {
	layer, ok := busyboxLayer(t)
	if !ok {
		t.Skip("no guest image built; see sandbox/guest/mkimage.sh")
	}
	registry := serveImage(t, layer)
	machines := realMachines(t)
	if len(machines) == 0 {
		t.Skip("nothing here boots a real kernel")
	}
	files := map[string]string{
		"main.go": "package main\n",
		EnvFile: "FROM " + registry + "/library/base:v1\n" +
			"RUN mkdir -p /opt && echo shared > /opt/shared\n",
	}

	for _, m := range machines {
		t.Run(m.name, func(t *testing.T) {
			workdir := hostTree(t, files)
			state := t.TempDir()
			ctx := context.Background()

			boot := func(want string) *VM {
				vm, err := NewVM(workdir, VMOptions{StateDir: state, Hypervisor: m.hv, Instance: want})
				if err != nil {
					t.Fatal(err)
				}
				if err := vm.SetResources(Resources{CPUs: 2, MemoryGB: 2, DiskGB: testDiskGB, Processes: 256}); err != nil {
					t.Fatal(err)
				}
				bctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
				defer cancel()
				if err := vm.Build(bctx, nil); err != nil {
					if log, rerr := os.ReadFile(m.console); rerr == nil {
						t.Logf("guest console:\n%s", log)
					}
					t.Fatalf("build: %v", err)
				}
				return vm
			}

			before := requests.Load()
			first := boot("")
			t.Cleanup(func() { first.Stop(context.Background()) })
			built := requests.Load() - before
			if built == 0 {
				t.Fatal("the first sandbox fetched nothing")
			}
			needsAShell(t, first)

			// The second shares the environment: it fetches nothing at all.
			// It asks for a sandbox of its own, which is what a developer
			// answers the dialog with when another terminal has the first.
			second := boot(NewInstance)
			t.Cleanup(func() { second.Stop(context.Background()) })
			if got := requests.Load() - before - built; got != 0 {
				t.Errorf("the second sandbox fetched %d times; the environment is shared", got)
			}
			if first.Instance() == second.Instance() {
				t.Fatal("both sessions took the same sandbox")
			}

			// Both have it.
			for name, vm := range map[string]*VM{"first": first, "second": second} {
				if got := strings.TrimSpace(string(run(t, vm, "cat /opt/shared").Stdout)); got != "shared" {
					t.Errorf("%s sandbox has no environment: %q", name, got)
				}
			}

			// And what one writes into it is its own. A session has every
			// right to install something; what it installs is not everybody's.
			run(t, first, "echo mine > /opt/only-in-first")
			if got := strings.TrimSpace(string(run(t, first, "cat /opt/only-in-first").Stdout)); got != "mine" {
				t.Errorf("a sandbox could not write into its own environment: %q", got)
			}
			if res := run(t, second, "cat /opt/only-in-first 2>&1"); res.ExitCode == 0 {
				t.Errorf("one sandbox's write reached another's environment: %q", res.Stdout)
			}
		})
	}
}

// Rebuilding the environment inside a real machine keeps the work.
//
// This is the operation that had no answer: an environment that is wrong — a
// half-finished install, a base image that moved — used to cost a developer
// everything the sandbox had written, because /reset was the only way to force
// a rebuild.
func TestRebuildingTheEnvironmentInAMachineKeepsTheWork(t *testing.T) {
	layer, ok := busyboxLayer(t)
	if !ok {
		t.Skip("no guest image built; see sandbox/guest/mkimage.sh")
	}
	registry := serveImage(t, layer)
	machines := realMachines(t)
	if len(machines) == 0 {
		t.Skip("nothing here boots a real kernel")
	}

	for _, m := range machines {
		t.Run(m.name, func(t *testing.T) {
			workdir := hostTree(t, map[string]string{
				"main.go": "package main\n",
				EnvFile: "FROM " + registry + "/library/base:v1\n" +
					"RUN mkdir -p /opt && echo from-the-build > /opt/marker\n",
			})
			vm, err := NewVM(workdir, VMOptions{StateDir: t.TempDir(), Hypervisor: m.hv})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { vm.Stop(context.Background()) })
			if err := vm.SetResources(Resources{CPUs: 2, MemoryGB: 2, DiskGB: testDiskGB, Processes: 256}); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			if err := vm.Build(ctx, nil); err != nil {
				t.Fatalf("build: %v", err)
			}
			needsAShell(t, vm)

			// Work in progress, and something installed by hand that a
			// rebuild is entitled to take away.
			run(t, vm, "echo 'half a day of work' > notes.txt")
			run(t, vm, "echo installed-by-hand > /opt/by-hand")
			was := vm.Instance()

			before := requests.Load()
			if err := vm.RebuildEnvironment(ctx, nil); err != nil {
				t.Fatalf("rebuild: %v", err)
			}
			if requests.Load() == before {
				t.Error("the rebuild did not fetch the base image again")
			}
			if vm.Instance() != was {
				t.Errorf("the rebuild moved to sandbox %s, was %s", vm.Instance(), was)
			}

			// The environment is fresh: built again, and without what was
			// installed into it by hand.
			if got := strings.TrimSpace(string(run(t, vm, "cat /opt/marker").Stdout)); got != "from-the-build" {
				t.Errorf("the environment was not rebuilt: %q", got)
			}
			if res := run(t, vm, "cat /opt/by-hand 2>&1"); res.ExitCode == 0 {
				t.Errorf("the old environment's layer survived: %q", res.Stdout)
			}

			// And the work is still there, still unapplied.
			if got := strings.TrimSpace(string(run(t, vm, "cat notes.txt").Stdout)); got != "half a day of work" {
				t.Errorf("the rebuild lost the sandbox's own work: %q", got)
			}
			if diffs, err := vm.Diff(ctx); err != nil || len(diffs) != 1 {
				t.Errorf("diff after a rebuild = %+v %v", diffs, err)
			}
		})
	}
}

// Inside the environment a program can open a pseudo-terminal and share
// memory: /dev/pts and /dev/shm are there. Without them `script`, `expect`,
// Python's pty and apt's own log ("Is /dev/pts mounted?") failed, and so did
// anything that shares memory between processes.
func TestTheEnvironmentHasPseudoTerminalsAndSharedMemory(t *testing.T) {
	layer, ok := busyboxLayer(t)
	if !ok {
		t.Skip("no guest image built; see sandbox/guest/mkimage.sh")
	}
	registry := serveImage(t, layer)
	real := realMachines(t)
	if len(real) == 0 {
		t.Skip("nothing here boots a real kernel")
	}
	files := map[string]string{
		"main.go": "package main\n",
		EnvFile:   "FROM " + registry + "/library/base:v1\n",
	}
	for _, m := range real {
		t.Run(m.name, func(t *testing.T) {
			vm, _ := bootRealMachine(t, m, hostTree(t, files), policy.Protections{})
			needsAShell(t, vm)
			res := run(t, vm, "exec 3<>/dev/ptmx && ls /dev/pts && echo PTY-OK; "+
				"echo x > /dev/shm/probe && cat /dev/shm/probe && echo SHM-OK")
			out := string(res.Stdout) + string(res.Stderr)
			for _, want := range []string{"PTY-OK", "SHM-OK"} {
				if !strings.Contains(out, want) {
					t.Fatalf("%s missing from:\n%s", want, out)
				}
			}
		})
	}
}

// Commands run at once, as the agent runs them, and every one answers. Each
// used to start in the workspace — a FUSE mount the agent itself serves —
// and Go's vfork child changes to its directory before it execs, with the
// forking thread frozen. A collection's stop-the-world then waited on that
// thread while the child waited on the agent: every command and the whole
// control link hung, which a few commands run together did within a round.
func TestCommandsRunAtOnceAllAnswer(t *testing.T) {
	layer, ok := busyboxLayer(t)
	if !ok {
		t.Skip("no guest image built; see sandbox/guest/mkimage.sh")
	}
	registry := serveImage(t, layer)
	real := realMachines(t)
	if len(real) == 0 {
		t.Skip("nothing here boots a real kernel")
	}
	files := map[string]string{
		"main.go":   "package main\n",
		"sub/a.txt": "a\n",
		EnvFile:     "FROM " + registry + "/library/base:v1\n",
	}
	for _, m := range real {
		t.Run(m.name, func(t *testing.T) {
			vm, workdir := bootRealMachine(t, m, hostTree(t, files), policy.Protections{})
			needsAShell(t, vm)
			for round := 0; round < 20; round++ {
				ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
				errs := make(chan error, 6)
				var wg sync.WaitGroup
				for i := 0; i < 6; i++ {
					wg.Add(1)
					go func(i int) {
						defer wg.Done()
						res, err := vm.Exec(ctx, fmt.Sprintf("echo %d > r%d-%d.txt && cat r%d-%d.txt", i, round, i, round, i), "", 30*time.Second)
						if err != nil {
							errs <- err
						} else if strings.TrimSpace(string(res.Stdout)) != fmt.Sprint(i) {
							errs <- fmt.Errorf("command %d said %q", i, res.Stdout)
						}
					}(i)
				}
				wg.Wait()
				cancel()
				close(errs)
				for err := range errs {
					t.Fatalf("round %d: %v", round, err)
				}
			}
			// And a command still starts where it was asked to.
			if got := strings.TrimSpace(string(run(t, vm, "pwd").Stdout)); got != GuestMount(workdir) {
				t.Errorf("pwd = %q, want %q", got, GuestMount(workdir))
			}
			res, err := vm.Exec(context.Background(), "cat a.txt", "sub", time.Minute)
			if err != nil || strings.TrimSpace(string(res.Stdout)) != "a" {
				t.Errorf("a command in sub/: %q %v", res.Stdout, err)
			}
		})
	}
}
