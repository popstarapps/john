package sandbox

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// The environment, from the inside: a root filesystem on the sandbox's own
// disk, with the project's tools in it.
//
// The host decides what goes in — it parses the Dockerfile and talks to the
// registry, because that is where the proxy is and because it is testable
// without a machine. The guest does the part that has to be Linux: unpacking
// layers with their ownership and their whiteouts intact, and running the
// build's commands inside the result.
//
// It lives on the state disk rather than a disk of its own. The disk is
// already the sandbox's size limit, and an environment that shares it is one
// filesystem to make, one to mount, and one number for a developer to reason
// about — "the sandbox has 100 GB" rather than two allowances that can each
// run out separately.

// envLayout is where the environment lives inside the guest.
//
// The paths sit under the agent's root, like every other absolute path it
// writes. A real machine owns its filesystem and roots them at /john; the
// loopback machine is a process on the developer's own operating system, and
// roots them inside the directory standing in for the guest's root — so that
// nothing the agent does reaches the developer's actual /. That machine never
// builds an environment (see envSupported), and the paths are rooted anyway:
// an agent that cannot name a path outside its root cannot write one by taking
// a route nobody thought of.
type envLayout struct {
	// base is the environment as it was built: a disk of its own, shared by
	// every sandbox in this workspace and mounted read-only once it is
	// finished. It is the expensive part — a base image plus everything the
	// project's Dockerfile installs — and it is identical for everyone, so
	// building it twice would be paying twice for the same thing.
	base string
	// dir is what a command actually runs in: the environment with a writable
	// layer of this sandbox's own over it. A session that installs something
	// gets to install it, and what it installs is this sandbox's and appears
	// in nobody else's. upper and work are what that layer is made of.
	dir   string
	upper string
	work  string
	// stamp records which environment is installed, so a start that changes
	// nothing does not rebuild. It lives on the environment's own disk rather
	// than beside it, because it describes that disk and travels with it.
	stamp string
	// stage is where a layer lands while it is being received.
	stage string
}

// envLayoutUnder is the environment's layout for an agent whose root is root.
// An empty root is a machine that owns its filesystem, and gives the paths
// exactly as they are written here.
func envLayoutUnder(root string) envLayout {
	john := filepath.Join(root, "/john")
	l := envLayout{
		base:  filepath.Join(john, "envbase"),
		dir:   filepath.Join(john, "env"),
		upper: filepath.Join(john, "envupper"),
		work:  filepath.Join(john, "envwork"),
		stage: filepath.Join(john, "env.stage"),
	}
	l.stamp = filepath.Join(l.base, ".john-env-id")
	return l
}

// envAt is where this agent keeps its environment.
func (g *guest) envAt() envLayout { return envLayoutUnder(g.cfg.Root) }

// envInstalled reports the environment already on the disk.
func (g *guest) envInstalled() string {
	l := g.envAt()
	id, err := os.ReadFile(l.stamp)
	if err != nil {
		return ""
	}
	if _, err := os.Stat(filepath.Join(l.base, "bin")); err != nil {
		// A stamp with no root under it is a build that did not finish.
		return ""
	}
	return strings.TrimSpace(string(id))
}

// envRoot is the directory commands run inside, or "" when this sandbox has no
// environment and commands run in the guest's own busybox.
//
// It is the overlay rather than the environment itself, so that what a session
// installs is its own. Mounting it happens here, on first use, because there
// is nothing to lay anything over until an environment has been built.
func (g *guest) envRoot() string {
	l := g.envAt()
	if g.envInstalled() == "" {
		return ""
	}
	if err := mountEnvOverlay(l); err != nil {
		return ""
	}
	g.caInto(l.dir)
	return l.dir
}

// caInto puts the proxy's certificate authority inside the environment.
// Commands run chrooted into it, and the variables that point them at the
// certificate name a path they resolve there — not in the guest's own root,
// where setEnviron writes it. Without this, every HTTPS request from inside an
// environment fails on a file that is not there, allowed by policy or not. It
// lands in this sandbox's writable layer, so the shared environment image is
// left as it was built.
func (g *guest) caInto(root string) {
	g.caMu.Lock()
	defer g.caMu.Unlock()
	if len(g.caPEM) == 0 || g.caAt == root {
		return
	}
	if err := installGuestCA(root, g.caPEM); err != nil {
		fmt.Fprintf(os.Stderr, "john: the proxy's certificate could not be put in the environment: %v\n", err)
		return
	}
	g.caAt = root
}

// envBegin clears whatever was there. An environment is rebuilt rather than
// patched: the definition is content-addressed, so a different definition is a
// different environment, and layering a new build over an old one would leave
// files no Dockerfile asked for.
//
// The overlay goes too. A rebuilt environment under a session's old writable
// layer would carry files nothing asked for, with no way to tell where they
// came from.
func (g *guest) envBegin() error {
	l := g.envAt()
	os.Remove(l.stage)
	// The overlay and its working directories are this sandbox's own, on its
	// own disk, and go entirely.
	for _, dir := range []string{l.dir, l.upper, l.work} {
		unmountIfMounted(dir)
		if err := os.RemoveAll(dir); err != nil {
			return err
		}
	}
	// The environment itself is a mounted disk, so it is emptied rather than
	// removed: unmounting it here would leave the build writing into the
	// directory the disk was mounted on, and the finished environment would
	// be on the wrong filesystem — present to the machine that built it and
	// missing from every other one.
	if err := os.MkdirAll(l.base, 0o755); err != nil {
		return err
	}
	return clearDir(l.base)
}

// envStage appends a chunk of a layer. Layers are tens of megabytes and the
// link's frames are not, so they arrive in pieces.
func (g *guest) envStage(chunk []byte) error {
	l := g.envAt()
	f, err := os.OpenFile(l.stage, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(chunk)
	return err
}

// envUnpack applies the staged layer to the root and discards it.
func (g *guest) envUnpack() error {
	l := g.envAt()
	f, err := os.Open(l.stage)
	if err != nil {
		return err
	}
	defer func() {
		f.Close()
		os.Remove(l.stage)
	}()
	return unpackLayer(f, l.base)
}

// unpackLayer writes one image layer onto the root.
//
// A layer is a tar of what changed, and what it removes is spelled as a file
// rather than an absence: `.wh.name` beside a path deletes it, and
// `.wh..wh..opq` in a directory clears everything the layers below put there.
// A build that ignored those would end up with files the image says are gone —
// the classic way a hand-rolled unpacker produces an environment that does not
// match the one the Dockerfile describes.
func unpackLayer(r io.Reader, root string) error {
	// Layers are gzipped in practice and the media type says so, but a plain
	// tar is legal and costs one sniff to accept.
	br := bufio.NewReader(r)
	head, err := br.Peek(2)
	var src io.Reader = br
	if err == nil && head[0] == 0x1f && head[1] == 0x8b {
		zr, err := gzip.NewReader(br)
		if err != nil {
			return err
		}
		defer zr.Close()
		src = zr
	}

	tr := tar.NewReader(src)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		name := filepath.Clean(filepath.FromSlash(h.Name))
		if name == "." || strings.HasPrefix(name, "..") {
			continue
		}
		target := filepath.Join(root, name)
		// A layer that walks out of the root with .. or an absolute link is
		// not something to be tolerant about.
		if !within(root, target) {
			return fmt.Errorf("sandbox: image layer writes outside the environment: %q", h.Name)
		}

		base := filepath.Base(name)
		if base == ".wh..wh..opq" {
			if err := clearDir(filepath.Dir(target)); err != nil {
				return err
			}
			continue
		}
		if strings.HasPrefix(base, ".wh.") {
			os.RemoveAll(filepath.Join(filepath.Dir(target), strings.TrimPrefix(base, ".wh.")))
			continue
		}

		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		if err := writeEntry(tr, h, target); err != nil {
			return err
		}
	}
}

func writeEntry(tr *tar.Reader, h *tar.Header, target string) error {
	switch h.Typeflag {
	case tar.TypeDir:
		if err := os.MkdirAll(target, os.FileMode(h.Mode)&os.ModePerm); err != nil {
			return err
		}
	case tar.TypeReg:
		// A path replaced by a later layer is replaced, not merged into.
		os.RemoveAll(target)
		f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(h.Mode)&os.ModePerm)
		if err != nil {
			return err
		}
		_, err = io.Copy(f, tr)
		f.Close()
		if err != nil {
			return err
		}
	case tar.TypeSymlink:
		os.RemoveAll(target)
		if err := os.Symlink(h.Linkname, target); err != nil {
			return err
		}
	case tar.TypeLink:
		os.RemoveAll(target)
		// The link's target is relative to the root, not to this file.
		root := strings.TrimSuffix(target, filepath.FromSlash(h.Name))
		if err := os.Link(filepath.Join(root, filepath.FromSlash(h.Linkname)), target); err != nil {
			return err
		}
	default:
		// Character and block devices, fifos and sockets. The sandbox mounts
		// a real devtmpfs over /dev, so the image's own device nodes are not
		// needed and making them would need privileges the build should not
		// be exercising for a file nothing reads.
		return nil
	}
	os.Lchown(target, h.Uid, h.Gid)
	if h.Typeflag != tar.TypeSymlink {
		os.Chtimes(target, h.ModTime, h.ModTime)
	}
	return nil
}

func clearDir(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

// within reports whether p is inside root, so a layer cannot write past it.
func within(root, p string) bool {
	rel, err := filepath.Rel(root, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// envWrite puts one file from the build context into the environment, which is
// what COPY does.
func (g *guest) envWrite(path string, mode int64, data []byte) error {
	l := g.envAt()
	target := filepath.Join(l.base, filepath.FromSlash(path))
	if !within(l.base, target) {
		return fmt.Errorf("sandbox: COPY writes outside the environment: %q", path)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	if mode == 0 {
		mode = 0o644
	}
	return os.WriteFile(target, data, os.FileMode(mode)&os.ModePerm)
}

// envRun runs one of the build's commands inside the half-built environment.
//
// It is a chroot rather than the sandbox's own shell: the whole point is that
// `apt-get install` writes the image's /usr and reads the image's /etc, and a
// command that ran outside would install into the initramfs and be gone at the
// next boot.
func (g *guest) envRun(ctx context.Context, cmd string, env []string, workdir string, timeout time.Duration) (ExecResult, error) {
	l := g.envAt()
	// Nothing to protect here: a build runs before the environment is
	// installed, so the workspace is still mounted in the guest's own root
	// rather than under this one.
	if err := mountKernelFilesystems(l.base, ""); err != nil {
		return ExecResult{}, err
	}
	if workdir == "" {
		workdir = "/"
	}
	if err := os.MkdirAll(filepath.Join(l.base, filepath.FromSlash(workdir)), 0o755); err != nil {
		return ExecResult{}, err
	}

	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	c := exec.CommandContext(runCtx, "/bin/sh", "-c", cmd)
	c.Dir = workdir
	c.Env = env
	if err := setChroot(c, l.base); err != nil {
		return ExecResult{}, err
	}
	// A build step is the sandbox running a command, and counts against the
	// same limit: `make -j` in a RUN line is exactly the case the number is
	// there for.
	g.pids.apply(c)
	var stdout, stderr bytes.Buffer
	c.Stdout, c.Stderr = &stdout, &stderr
	err := c.Run()

	res := ExecResult{Stdout: stdout.Bytes(), Stderr: stderr.Bytes()}
	var exitErr *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exitErr):
		res.ExitCode = exitErr.ExitCode()
	default:
		return res, err
	}
	return res, nil
}

// envCommit records what was built, so the next start finds it rather than
// building it again.
func (g *guest) envCommit(id string, env []string, workdir string) error {
	l := g.envAt()
	g.mu.Lock()
	g.envDefaults, g.envWorkdir = append([]string{}, env...), workdir
	g.mu.Unlock()
	if err := os.WriteFile(filepath.Join(l.base, ".john-env"),
		[]byte(strings.Join(env, "\n")+"\n\x00"+workdir+"\n"), 0o644); err != nil {
		return err
	}
	return os.WriteFile(l.stamp, []byte(id+"\n"), 0o644)
}

// envLoad reads back what envCommit wrote, for a machine that was already
// running when the agent started.
func (g *guest) envLoad() {
	l := g.envAt()
	raw, err := os.ReadFile(filepath.Join(l.base, ".john-env"))
	if err != nil {
		return
	}
	envPart, workdir, _ := strings.Cut(string(raw), "\x00")
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, line := range strings.Split(strings.TrimSpace(envPart), "\n") {
		if line != "" {
			g.envDefaults = append(g.envDefaults, line)
		}
	}
	g.envWorkdir = strings.TrimSpace(workdir)
}
