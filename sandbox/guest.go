package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/popstarapps/john/policy"
)

// The agent inside the sandbox. It holds the union, answers the control
// stream, and — where the platform provides it — mounts the union so that a
// shell command sees the same workspace the edit tool does.
//
// The agent is the same `john` binary the developer runs, invoked as
// `john __guest`. Nothing else would be right: the agent and the client speak
// a protocol to each other, and a guest image carrying an agent from a
// different release would be a version skew with no handshake to catch it.
// Handing the binary to the machine at boot means the agent is always exactly
// the client that started it.

// GuestConfig is what the host tells the agent at start-up. It is exported
// because it crosses a process boundary: the machine passes it to
// `john __guest` as a single JSON argument.
type GuestConfig struct {
	// Mount is where the workspace is mounted inside the sandbox: the host
	// working directory's own path, mirrored.
	Mount string `json:"mount"`
	// Upper is the directory on the sandbox's disk holding what it writes.
	Upper string `json:"upper"`
	// Control and Lower are the addresses the agent dials for the two
	// streams. In a virtual machine these are vsock ports; under the
	// loopback machine they are Unix sockets.
	Control string `json:"control"`
	Lower   string `json:"lower"`
	// Root is the prefix the agent's absolute paths sit under. A real
	// machine owns its filesystem and leaves this empty; the loopback
	// machine is a process on the developer's own operating system, and
	// everything it writes has to land inside the directory standing in for
	// the guest's root rather than on the developer's actual /usr.
	Root string `json:"root,omitempty"`
	// MaxOutputBytes caps what an Exec sends back.
	MaxOutputBytes int `json:"max_output_bytes"`
	// MaxProcesses is how many processes the sandbox's commands may have
	// running at once, together. It is the machine's Resources.Processes,
	// carried in because the limit has to be applied by the side that starts
	// the processes — the hypervisor gives out CPUs, memory and a disk, and
	// has nothing to say about a fork.
	MaxProcesses int `json:"max_processes,omitempty"`
}

// guest is the agent's state.
type guest struct {
	cfg   GuestConfig
	u     *union
	mount *fuseMount
	dial  func(addr string) (net.Conn, error)

	mu      sync.Mutex
	mounted bool
	// env is the machine-wide environment every command runs with: the proxy
	// and the certificate authority that lets it terminate TLS. It arrives
	// after the link is up rather than on the kernel command line, because a
	// certificate does not fit there.
	env       []string
	forwarder *proxyForwarder
	// caPEM is the proxy's certificate authority, kept so that it can be put
	// where commands actually run: inside the environment, whose root is not
	// the guest's own. caAt is the root it was last written under.
	caPEM []byte
	caMu  sync.Mutex
	caAt  string
	// pids is the sandbox's process limit, applied to every command it runs.
	// Nil is no limit: the machine was given none, or this is the loopback
	// machine, which limits nothing because it isolates nothing.
	pids *processLimit
	// envDefaults and envWorkdir are what the environment's own image says a
	// command starts with — PATH above all, which a base image declares and
	// without which a build finds nothing in /usr/bin.
	envDefaults []string
	envWorkdir  string
}

// RunGuestAgent is the `john __guest` entry point: the agent inside the
// sandbox, dialling back to the client that started it.
func RunGuestAgent(ctx context.Context, cfg GuestConfig) error {
	return RunGuest(ctx, cfg, func(addr string) (net.Conn, error) {
		return dialGuestLink(addr)
	})
}

// RunGuest is RunGuestAgent with the transport supplied, which is how the
// loopback machine runs the agent in-process over a Unix socket and how a
// virtual machine will run it over vsock.
func RunGuest(ctx context.Context, cfg GuestConfig, dial func(addr string) (net.Conn, error)) error {
	lowerConn, err := dial(cfg.Lower)
	if err != nil {
		return fmt.Errorf("sandbox: guest cannot reach the host filesystem: %w", err)
	}
	defer lowerConn.Close()

	u, err := newUnion(&rpcLower{c: newRPCClient(lowerConn)}, cfg.Upper, policy.Protections{})
	if err != nil {
		return err
	}
	g := &guest{cfg: cfg, u: u, dial: dial}
	// The process limit, where there is a machine to hold one. Under the
	// loopback machine the agent is a process on the developer's own computer
	// and has no business writing cgroups there, which is the same line
	// envSupported draws and for the same reason.
	if cfg.Root == "" {
		limit, err := newProcessLimit(cfg.MaxProcesses)
		if err != nil {
			// Loud, and not fatal. This is a resource guard rather than a
			// boundary — the machine's memory is the backstop — and a sandbox
			// that refuses to start because a kernel has no pids controller
			// would be trading a working sandbox for a number.
			fmt.Fprintf(os.Stderr, "john: the sandbox's process limit could not be applied: %v\n", err)
		} else {
			g.pids = limit
			defer g.pids.close()
		}
	}
	g.envLoad()
	defer g.unmount()
	defer g.closeForwarder()

	ctrl, err := dial(cfg.Control)
	if err != nil {
		return fmt.Errorf("sandbox: guest cannot reach the host: %w", err)
	}
	return serveRPC(ctx, ctrl, g.serveWatched)
}

// setEnviron installs the machine-wide environment and opens the route out.
//
// It is a control call rather than part of the boot configuration because the
// certificate authority is kilobytes and the kernel command line is not, and
// because a policy refresh can change it while the sandbox is running.
func (g *guest) setEnviron(proxyAddr string, env []string, caPEM []byte) (string, error) {
	if err := installGuestCA(g.cfg.Root, caPEM); err != nil {
		return "", err
	}
	g.caMu.Lock()
	g.caPEM, g.caAt = caPEM, ""
	g.caMu.Unlock()

	g.mu.Lock()
	defer g.mu.Unlock()

	// The forwarder is opened once and kept: its address is in the
	// environment of every command already running, so a refresh that moved
	// it would strand them.
	if proxyAddr != "" && g.forwarder == nil {
		f, err := startProxyForwarder(proxyAddr, g.dial)
		if err != nil {
			return "", fmt.Errorf("sandbox: no route to the proxy: %w", err)
		}
		g.forwarder = f
	}
	local := ""
	if g.forwarder != nil {
		local = g.forwarder.Addr()
	}
	g.env = append(append([]string{}, env...), proxyEnviron(local, g.cfg.Root, true)...)
	return local, nil
}

// workspacePath is where the union is mounted. With an environment installed
// it sits under that root, so a command chrooted into the environment still
// finds the project at the path the host keeps it at.
func (g *guest) workspacePath() string {
	if root := g.envRoot(); root != "" {
		return filepath.Join(root, filepath.FromSlash(g.cfg.Mount))
	}
	return g.cfg.Mount
}

// envInherited is what a command inside the environment starts with: the base
// image's own environment rather than the initramfs's. PATH is the one that
// matters — the agent's own PATH names busybox, and inside the environment
// that is not where anything is.
func (g *guest) envInherited() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := append([]string{}, g.envDefaults...)
	if g.envWorkdir != "" {
		out = append(out, "HOME=/root")
	}
	return out
}

func (g *guest) environ() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string{}, g.env...)
}

func (g *guest) closeForwarder() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.forwarder != nil {
		g.forwarder.Close()
		g.forwarder = nil
	}
}

func (g *guest) unmount() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.mount != nil {
		g.mount.Close()
		g.mount = nil
		g.mounted = false
	}
}

// stuckAfter is how long a control request may take before the agent says
// what every goroutine in it is doing. Exec has its own deadline and answers
// well inside it; anything else this slow is stuck.
const stuckAfter = 2 * time.Minute

// serveWatched is serve, with a watchdog: a request still running after
// stuckAfter writes the agent's goroutines to the console log, once. A guest
// that stops answering is otherwise a black box — the host sees only its
// own calls waiting — and the console is the one thing the host keeps of it.
// It cannot see a runtime that has stopped altogether, as doExec's deadlock
// did; the kernel's own hung-task report is what shows that one.
func (g *guest) serveWatched(ctx context.Context, op string, body json.RawMessage, blobs [][]byte) (any, [][]byte, error) {
	limit := stuckAfter
	if op == "exec" {
		var req struct {
			TimeoutMS int64 `json:"timeout_ms"`
		}
		json.Unmarshal(body, &req)
		limit += time.Duration(req.TimeoutMS) * time.Millisecond
	}
	t := time.AfterFunc(limit, func() {
		buf := make([]byte, 1<<20)
		n := runtime.Stack(buf, true)
		fmt.Fprintf(os.Stderr, "john: control request %q has run for %s; goroutines:\n%s\n", op, limit, buf[:n])
	})
	defer t.Stop()
	return g.serve(ctx, op, body, blobs)
}

// serve answers one control request.
func (g *guest) serve(ctx context.Context, op string, body json.RawMessage, blobs [][]byte) (any, [][]byte, error) {
	switch op {
	case "ping":
		return struct {
			Mounted bool `json:"mounted"`
		}{g.isMounted()}, nil, nil

	case "settime":
		// The host's clock, sent once the link is up. A guest's clock is
		// whatever its machine's real-time clock said at boot, and nothing
		// in the image keeps it right afterwards: Hyper-V's once held the
		// host's local time, read as UTC, and the sandbox ran two hours
		// ahead with nobody correcting it. The host is the one clock the
		// developer's files and certificates are judged by.
		var req struct {
			UnixNano int64 `json:"unix_nano"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			return nil, nil, err
		}
		// The loopback machine's agent is a process on the developer's own
		// computer, whose clock is already the host's and not john's to set.
		if g.cfg.Root != "" {
			return nil, nil, nil
		}
		return nil, nil, setClock(time.Unix(0, req.UnixNano))

	case "protect":
		var req struct {
			Protections policy.Protections `json:"protections"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			return nil, nil, err
		}
		g.u.setProtections(req.Protections)
		return nil, nil, nil

	case "environ":
		var req struct {
			Proxy string   `json:"proxy"`
			Env   []string `json:"env"`
			CAPEM []byte   `json:"ca_pem"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			return nil, nil, err
		}
		addr, err := g.setEnviron(req.Proxy, req.Env, req.CAPEM)
		if err != nil {
			return nil, nil, err
		}
		return struct {
			Proxy string `json:"proxy"`
		}{addr}, nil, nil

	case "envstate":
		return struct {
			ID        string `json:"id"`
			Supported bool   `json:"supported"`
		}{g.envInstalled(), envSupported(g.cfg.Root)}, nil, nil

	case "envbegin":
		return nil, nil, g.envBegin()

	case "envstage":
		for _, b := range blobs {
			if err := g.envStage(b); err != nil {
				return nil, nil, err
			}
		}
		return nil, nil, nil

	case "envunpack":
		return nil, nil, g.envUnpack()

	case "envwrite":
		var req struct {
			Path string `json:"path"`
			Mode int64  `json:"mode"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			return nil, nil, err
		}
		var data []byte
		if len(blobs) > 0 {
			data = blobs[0]
		}
		return nil, nil, g.envWrite(req.Path, req.Mode, data)

	case "envrun":
		var req struct {
			Cmd       string   `json:"cmd"`
			Env       []string `json:"env"`
			Workdir   string   `json:"workdir"`
			TimeoutMS int64    `json:"timeout_ms"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			return nil, nil, err
		}
		timeout := time.Duration(req.TimeoutMS) * time.Millisecond
		if timeout <= 0 {
			timeout = 30 * time.Minute
		}
		res, err := g.envRun(ctx, req.Cmd, req.Env, req.Workdir, timeout)
		if err != nil {
			return nil, nil, err
		}
		return struct {
			ExitCode int `json:"exit_code"`
		}{res.ExitCode}, [][]byte{res.Stdout, res.Stderr}, nil

	case "envcommit":
		var req struct {
			ID      string   `json:"id"`
			Env     []string `json:"env"`
			Workdir string   `json:"workdir"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			return nil, nil, err
		}
		return nil, nil, g.envCommit(req.ID, req.Env, req.Workdir)

	case "shutdown":
		// The machine is about to be powered off, and a virtual machine is
		// powered off rather than asked nicely: there is no orderly Linux
		// shutdown between here and the host cutting the power. So the last
		// thing the agent does is put the sandbox's disk in a state that
		// survives that — the workspace unmounted and everything written
		// actually on the disk rather than in a cache about to disappear.
		//
		// Without this a developer loses whatever the agent wrote in the
		// seconds before they stopped the sandbox, which is exactly the work
		// the sandbox exists to keep.
		g.unmount()
		// The environment disk especially: the next sandbox mounts it
		// read-only, and a read-only mount cannot replay a journal. A machine
		// that was cut off mid-write leaves one that nothing else can open.
		unmountEnvironment(g.envAt())
		return nil, nil, syncDisks()

	case "mount":
		mounted, why, err := g.doMount()
		if err != nil {
			return nil, nil, err
		}
		return struct {
			Mounted bool   `json:"mounted"`
			Reason  string `json:"reason"`
		}{mounted, why}, nil, nil

	case "read":
		var req struct {
			Path     string `json:"path"`
			MaxBytes int    `json:"max_bytes"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			return nil, nil, err
		}
		// Resolve the links in the path before anything else looks at it.
		// A tool call arrives as a whole path and nothing else would walk it,
		// and the protections are checked on what it resolves to — a link
		// into an excluded directory is a way in otherwise.
		rel, err := g.u.follow(ctx, req.Path, true)
		if err != nil {
			return nil, nil, err
		}
		if err := g.u.protections().CheckRead(rel); err != nil {
			return nil, nil, err
		}
		data, err := g.u.ReadFile(ctx, rel)
		if err != nil {
			return nil, nil, err
		}
		truncated := false
		if req.MaxBytes > 0 && len(data) > req.MaxBytes {
			data, truncated = data[:req.MaxBytes], true
		}
		return struct {
			Truncated bool `json:"truncated"`
		}{truncated}, [][]byte{data}, nil

	case "write":
		var req struct {
			Path string `json:"path"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			return nil, nil, err
		}
		if len(blobs) != 1 {
			return nil, nil, errors.New("sandbox: write without content")
		}
		// Writing through a link writes what it points at, which is what
		// opening one does everywhere else.
		rel, err := g.u.follow(ctx, req.Path, true)
		if err != nil {
			return nil, nil, err
		}
		return nil, nil, g.u.WriteFile(ctx, rel, blobs[0])

	case "delete":
		var req struct {
			Path string `json:"path"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			return nil, nil, err
		}
		// Deleting a link removes the link, not what it names, so the last
		// element is the one thing not followed here.
		rel, err := g.u.follow(ctx, req.Path, false)
		if err != nil {
			return nil, nil, err
		}
		err = g.u.Remove(ctx, rel, false)
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil, nil // rm -f: gone is the outcome asked for
		}
		return nil, nil, err

	case "settle":
		var req struct {
			Paths []change `json:"paths"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			return nil, nil, err
		}
		for _, c := range req.Paths {
			rel, err := CleanPath(c.Path)
			if err != nil {
				return nil, nil, err
			}
			if err := g.u.settle(rel, c.Hash, c.Deleted); err != nil {
				return nil, nil, err
			}
		}
		return nil, nil, nil

	case "changes":
		changes, err := g.u.Changes()
		if err != nil {
			return nil, nil, err
		}
		return struct {
			Changes []change `json:"changes"`
		}{changes}, nil, nil

	case "exec":
		return g.doExec(ctx, body)

	default:
		return nil, nil, fmt.Errorf("sandbox: unknown control op %q", op)
	}
}

func (g *guest) isMounted() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.mounted
}

// doMount puts the union on the sandbox's filesystem at the mirrored path, so
// that `go test ./...` inside the sandbox reads the developer's files and
// writes into the sandbox's own layer without knowing either exists.
//
// Not being able to mount at all is an answer rather than a failure, and only
// for the one platform that genuinely has none: the agent normally runs inside
// the Linux sandbox, but the loopback machine runs it as an ordinary process
// on the developer's own operating system, and on macOS or Windows there is no
// FUSE for it to use. Everything the union does over the control link still
// works there; a shell does not, and doExec says so rather than running
// somewhere else. Any other mount failure is still a failure — a real machine
// is Linux inside and never takes this path, so a sandbox that ought to have a
// workspace never comes up quietly without one.
func (g *guest) doMount() (bool, string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.mounted {
		return true, "", nil
	}
	mountpoint := g.workspacePath()
	if err := os.MkdirAll(mountpoint, 0o755); err != nil {
		return false, "", err
	}
	m, err := mountUnion(g.u, mountpoint)
	if errors.Is(err, errNoWorkspaceMount) {
		return false, err.Error(), nil
	}
	if err != nil {
		return false, "", err
	}
	g.mount, g.mounted = m, true
	return true, "", nil
}

func (g *guest) doExec(ctx context.Context, body json.RawMessage) (any, [][]byte, error) {
	var req struct {
		Cmd       string   `json:"cmd"`
		Cwd       string   `json:"cwd"`
		TimeoutMS int64    `json:"timeout_ms"`
		Env       []string `json:"env"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, nil, err
	}
	if !g.isMounted() {
		// Without the mount there is no workspace for a command to run in.
		// Answering with output produced somewhere else would be a lie the
		// model could not detect.
		return nil, nil, errors.New("sandbox: the workspace is not mounted, so there is " +
			"nowhere for a command to run")
	}

	dir := g.cfg.Mount
	if req.Cwd != "" && req.Cwd != "." {
		dir = filepath.Join(g.cfg.Mount, filepath.FromSlash(req.Cwd))
	}
	timeout := time.Duration(req.TimeoutMS) * time.Millisecond
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	execCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	start := time.Now()
	// The shell goes to the workspace itself, once it is running. Go starts a
	// child with vfork, and the child changes to its directory before it
	// execs, while the agent's thread that forked it is frozen. The
	// workspace is a FUSE mount this agent serves, so that chdir is a
	// request only the agent can answer — and if the runtime stops the world
	// for a collection meanwhile, it waits for the frozen thread, which waits
	// for the child, which waits for the agent. Every command and the whole
	// control link then hang until the machine is stopped, which is what a
	// few commands run at once — apt beside a `which` beside an `ls` — did.
	// Started in / instead, the child touches nothing the agent serves
	// before it execs.
	c := exec.CommandContext(execCtx, "sh", "-lc", "cd -- "+shellQuote(dir)+" || exit 1\n"+req.Cmd)
	c.Dir = "/"
	c.Env = append(append(os.Environ(), g.environ()...), req.Env...)

	// With an environment installed the command runs inside it, so `go`,
	// `git` and everything else the project's Dockerfile asked for are the
	// ones on the path. The workspace is mounted under that root at the
	// host's own path, so the directory the command starts in is the same
	// either way — what changes is which /usr is underneath it.
	if root := g.envRoot(); root != "" {
		if err := mountKernelFilesystems(root, g.workspacePath()); err != nil {
			return nil, nil, err
		}
		if err := setChroot(c, root); err != nil {
			return nil, nil, err
		}
		c.Env = append(append(g.envInherited(), g.environ()...), req.Env...)
	}
	g.pids.apply(c)
	inOwnGroup(c)
	var stdout, stderr bytes.Buffer
	c.Stdout, c.Stderr = &stdout, &stderr
	// A child that outlives the shell — `npm install &`, or anything still
	// running when the shell is killed at the deadline — holds the output
	// open, and Run would wait for it however long it takes. The host gives
	// up first and the command's result is lost. So the output is cut off a
	// little after the shell ends, and the command answers.
	c.WaitDelay = 5 * time.Second
	err := c.Run()
	if errors.Is(err, exec.ErrWaitDelay) {
		err = nil // the shell itself exited cleanly; its code is in ProcessState
	}

	max := g.cfg.MaxOutputBytes
	if max <= 0 {
		max = 1 << 20
	}
	out, outTrunc := truncateOutput(stdout.Bytes(), max)
	errOut, errTrunc := truncateOutput(stderr.Bytes(), max)

	res := struct {
		ExitCode   int   `json:"exit"`
		Truncated  bool  `json:"truncated"`
		DurationMS int64 `json:"duration_ms"`
	}{Truncated: outTrunc || errTrunc, DurationMS: time.Since(start).Milliseconds()}

	var exitErr *exec.ExitError
	switch {
	case err == nil:
	case execCtx.Err() != nil:
		// Before the exit code: a shell killed at the deadline has one too,
		// -1, which says nothing about why.
		res.ExitCode = 124
		errOut = append(errOut, []byte("\ncommand timed out\n")...)
	case errors.As(err, &exitErr):
		res.ExitCode = exitErr.ExitCode()
	default:
		return nil, nil, err
	}
	return res, [][]byte{out, errOut}, nil
}

// rpcLower is the lower layer as the guest sees it: every question about the
// developer's tree is a question for the host, which is the only side that
// can see it.
type rpcLower struct{ c *rpcClient }

func (l *rpcLower) Stat(ctx context.Context, rel string) (lowerStat, error) {
	var st lowerStat
	err := l.c.callJSON(ctx, "stat", map[string]string{"path": rel}, &st)
	return st, err
}

func (l *rpcLower) ReadDir(ctx context.Context, rel string) ([]lowerStat, error) {
	var out struct {
		Entries []lowerStat `json:"entries"`
	}
	err := l.c.callJSON(ctx, "readdir", map[string]string{"path": rel}, &out)
	return out.Entries, err
}

func (l *rpcLower) ReadAt(ctx context.Context, rel string, off int64, n int) ([]byte, error) {
	f, err := l.c.call(ctx, "read", map[string]any{"path": rel, "off": off, "n": n}, nil)
	if err != nil {
		return nil, err
	}
	if len(f.blobs) != 1 {
		return nil, errors.New("sandbox: lower read returned no content")
	}
	return f.blobs[0], nil
}

// errNoWorkspaceMount is the one mount failure that is not a failure: the
// platform the agent is running on has no FUSE at all. It is spelled here,
// beside the agent, because both the implementation that answers it and the
// client that has to recognise it are on the other side of a build tag.
var errNoWorkspaceMount = errors.New("sandbox: no workspace mount on this platform")

// shellQuote quotes s as one word for sh.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
