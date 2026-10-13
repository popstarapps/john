package sandbox

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/popstarapps/john/policy"
)

// VM is the sandbox: a Linux virtual machine per working directory, with the
// developer's tree mounted copy-on-write inside it.
//
// Four things are the point of it. The workspace is a live mount rather than a
// snapshot, so an edit made on the host during a session is seen by the next
// command inside it. The machine's only network interface is the proxy, so
// there is no route out that policy does not govern — including from an image
// build. The disk limit is the size of a disk rather than a number written
// down. And the isolation boundary is a hypervisor rather than a shared
// kernel.
//
// What it keeps is the part that matters most: Apply is still the only code
// path in the system that writes the host working directory, and the host side
// of the mount has no write operation at all.
type VM struct {
	workdir  string
	key      string
	stateDir stateDir
	// inst is this session's own sandbox inside the workspace's directory:
	// its disk, and so its own unapplied work. Empty until Build picks one.
	inst instanceDir
	opts VMOptions
	hv   hypervisor

	mu   sync.Mutex
	mach machine
	// mounted says whether the agent could put the workspace on a
	// filesystem; see WorkspaceMounted.
	mounted         bool
	unmountedReason string
	// routed says whether anything inside can reach the proxy at all.
	routed bool
	// noEnvironment says why this sandbox has no environment, when that is
	// something the machine could not do rather than something the project
	// did not ask for.
	noEnvironment string
	// lock is this process's claim on the workspace's sandbox, held for as
	// long as the machine runs.
	lock *os.File
	// note is what this session is working on; see SetNote.
	note  string
	ctrl  *rpcClient
	host  *hostFS
	image string
	prot  policy.Protections
}

// VMOptions configure a VM sandbox.
type VMOptions struct {
	// StateDir overrides ~/.john/sandboxes/<key>.
	StateDir string
	// Instance names which of the workspace's sandboxes to use, or "new" for
	// one of its own. Empty is the ordinary case: it takes the only sandbox
	// there is, makes the first one, and otherwise answers errChooseInstance
	// so the developer is asked — see resolveInstance.
	Instance string
	// Protect are the workspace protections to start with. A session installs
	// the ones from its signed snapshot with Protect; leaving this empty picks
	// up whatever the last session persisted for this workspace.
	Protect policy.Protections
	// MaxOutputBytes caps Exec output (head+tail truncation). Default 1 MiB.
	MaxOutputBytes int
	// Env is the machine-wide environment inside the sandbox: the proxy and
	// the certificate authority that lets it terminate TLS.
	Env []string
	// ProxySocket is the host path of the Unix socket the john proxy listens
	// on. It is the machine's only route out — not a setting inside it that
	// something could decline to honour — so a process that ignores the proxy
	// environment reaches nothing at all.
	ProxySocket string
	// ProxyCAPEM is the proxy's certificate authority, installed into the
	// sandbox trust store so TLS interception is accepted.
	ProxyCAPEM []byte
	// RegistryCredentials answers what this machine signs in to a registry
	// with, for the base image pull. Nil means anonymous, which is what a
	// test wants and what a machine with no registry configured has.
	//
	// It is a callback rather than a credential because the answer is the
	// developer's, can be a program that has to be run (every cloud registry
	// mints a short-lived token through a helper), and must not be resolved
	// before it is needed. The sandbox spends it on the host and hands the
	// machine layers; nothing of it crosses the boundary.
	RegistryCredentials func(host string) (RegistryCredential, error)
	// DefaultEnvironment is the Dockerfile a project that ships no
	// .john/Dockerfile is built from — john passes StarterEnvironment, so a
	// sandbox has the same basic tools whether or not the project wrote the
	// file down. Empty means no environment at all: the sandbox is then the
	// guest image's own busybox, which is what a test wants and never what a
	// developer does.
	DefaultEnvironment string
	// BuildNetwork is where the environment build's traffic goes: direct or
	// proxied. See policy/build.go. Empty is direct, which is what lets a
	// project install its tooling and then work under a policy that allows no
	// egress at all.
	BuildNetwork string
	// BuildProxySocket is the proxy a direct build reaches the network
	// through: one that refuses nothing. Only the machine that builds the
	// environment is wired to it; every session's machine is wired to
	// ProxySocket and its policies. Empty leaves a direct build on
	// ProxySocket too, held to the policies like everything else.
	BuildProxySocket string
	// Version names the release this client is, so the guest image it
	// extracts is kept under a name that changes when the client does. An
	// upgraded client writes a new image beside the old one rather than over
	// it, which matters because a machine may still be running on the old.
	Version string
	// Hypervisor names the machine to run on. Empty picks the platform's.
	Hypervisor hypervisor
}

// environPayload is everything the machine is told about its environment, and
// deliberately the whole of it: the route out, the variables every command
// runs with, and the authority that makes TLS through the proxy verify.
//
// What is not in it is the point. The credential that fetched the base image
// is spent on the host, where the proxy is, and stops there — the machine is
// handed layers, never the password that fetched them. Everything in this map
// crosses the boundary, so the list is guarded by a test rather than by
// whoever is reading the diff.
func (v *VM) environPayload(proxyAddr string) map[string]any {
	return map[string]any{
		"proxy":  proxyAddr,
		"env":    v.opts.Env,
		"ca_pem": v.opts.ProxyCAPEM,
	}
}

// SetProxy points the sandbox at the john proxy. Must be called before Build:
// the proxy is how the machine is wired, not something configured inside it.
func (v *VM) SetProxy(socketPath string, caPEM []byte) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.opts.ProxySocket = socketPath
	v.opts.ProxyCAPEM = caPEM
}

// NewVM creates the handle. Nothing boots until Build.
func NewVM(workdir string, opts VMOptions) (*VM, error) {
	abs, err := filepath.Abs(workdir)
	if err != nil {
		return nil, err
	}
	if opts.MaxOutputBytes == 0 {
		opts.MaxOutputBytes = 1 << 20
	}
	hv := opts.Hypervisor
	if hv == nil {
		hv = platformHypervisor()
	}
	key := WorkspaceKey(abs)
	dir := stateDir(opts.StateDir)
	if dir == "" {
		if dir, err = defaultStateDir(key); err != nil {
			return nil, err
		}
		// Sandboxes used to be named by a hash alone. An upgrade must find
		// the one it already has rather than quietly starting a second and
		// stranding whatever the first was holding.
		adoptLegacyStateDir(abs, dir)
	}
	dir.setWorkspace(abs)
	v := &VM{workdir: abs, key: key, stateDir: dir, opts: opts, hv: hv, prot: opts.Protect}
	if v.prot.Empty() {
		// `john apply` runs with no backend and no snapshot, and it is the
		// last gate before the host tree. It reads what the last session
		// stored so the paths an administrator protected stay protected when
		// nobody is logged in.
		if p, ok := v.stateDir.protections(); ok {
			v.prot = p
		}
	}
	return v, nil
}

// imageVersion names the directory the guest image is extracted into.
func (v *VM) imageVersion() string {
	if v.opts.Version == "" {
		return "dev"
	}
	return v.opts.Version
}

// Key returns the workspace key this sandbox is named by.
func (v *VM) Key() string { return v.key }

// GuestMount is where this workspace is mounted inside the sandbox: the
// host's own path, mirrored, so that paths in compiler output, stack traces
// and build caches mean the same thing on both sides.
func (v *VM) GuestMount() string { return GuestMount(v.workdir) }

func (v *VM) Status(ctx context.Context) (Status, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	st := Status{State: StateNone, Image: v.image, Resources: v.stateDir.resources()}
	if v.mach != nil && v.ctrl != nil {
		st.State = StateReady
	}
	return st, nil
}

// Build boots the machine, mounts the workspace, and returns once a tool call
// would succeed. It is idempotent: a machine already running is left running,
// because the sandbox's state — installed packages, build caches, unapplied
// work — is the reason it persists between sessions.
func (v *VM) Build(ctx context.Context, progress func(Progress)) error {
	report := func(step int, label, detail string) {
		if progress != nil {
			progress(Progress{Step: step, Steps: 4, Label: label, Detail: detail})
		}
	}

	v.mu.Lock()
	already := v.mach != nil && v.ctrl != nil
	v.mu.Unlock()
	if already {
		report(4, "sandbox ready", "")
		return nil
	}

	if err := v.hv.Available(); err != nil {
		return err
	}
	if err := v.stateDir.ensure(); err != nil {
		return err
	}
	// Which of this workspace's sandboxes this session is, and a claim on it
	// held for as long as the machine runs. Taken before anything starts,
	// because what it guards is the disk the machine is about to open: two
	// processes over one image is two kernels writing one filesystem.
	inst, err := v.stateDir.resolveInstance(v.workdir, v.opts.Instance)
	if err != nil {
		return err
	}
	lock, err := inst.claim(v.workdir)
	if err != nil {
		return err
	}
	defer func() {
		if !v.running() {
			releaseLock(lock)
		}
	}()

	report(1, "preparing environment", v.hv.Name())
	env, err := resolveEnvironment(v.workdir, v.stateDir)
	if err != nil {
		return err
	}
	if env.Default() && v.opts.DefaultEnvironment != "" {
		// Named for what it holds, so a john whose built-in environment
		// changed rebuilds a sandbox made by one before it, rather than
		// finding the old one under the same name and keeping it.
		sum := sha256.Sum256([]byte(v.opts.DefaultEnvironment))
		env.ID = "default-" + hex.EncodeToString(sum[:])
	}

	report(2, "starting sandbox", env.Describe())
	image, err := guestImage(string(v.stateDir), v.imageVersion())
	if err != nil && !errors.Is(err, ErrNoGuestImage) {
		return err
	}
	// What this workspace's environment is going to be, decided before
	// anything boots because it decides how the machine is wired.
	base, df, err := v.environmentPlan(env)
	if err != nil {
		return err
	}
	envImage := ""
	if base != "" {
		envImage = envDiskPath(v.stateDir, env.ID)
		// The image has to exist before either machine attaches it. It costs
		// what the environment weighs rather than what it could hold, in
		// whichever format this platform's machine takes.
		if err := makeDisk(envImage, envDiskBytes); err != nil {
			return err
		}
	}

	boot := func(envWritable bool) (machine, error) {
		// The machine that builds the environment is the only one that can be
		// wired to the build's proxy, and only when the build is direct.
		proxySocket := v.opts.ProxySocket
		if envWritable && v.buildPassesThrough() {
			proxySocket = v.opts.BuildProxySocket
		}
		return v.hv.Boot(ctx, bootConfig{
			Dir:            string(inst),
			Mount:          v.GuestMount(),
			Upper:          "/john/upper",
			Env:            v.opts.Env,
			ProxySocket:    proxySocket,
			ProxyCAPEM:     v.opts.ProxyCAPEM,
			BuildNetwork:   v.opts.BuildNetwork,
			Environment:    env,
			EnvDisk:        envImage,
			EnvWritable:    envWritable,
			Kernel:         image.Kernel,
			Initramfs:      image.Initramfs,
			Resources:      v.stateDir.resources(),
			MaxOutputBytes: v.opts.MaxOutputBytes,
		})
	}

	// Building the environment is its own machine, started for it and stopped
	// after it.
	//
	// It has to be, because a disk image cannot be attached to one machine
	// read-write and another read-only at the same time — the hypervisor
	// refuses, and it is right to: the second would be reading a filesystem
	// the first is still changing. So the build runs in a machine of its own
	// and is gone before any session starts, and every session then attaches
	// the finished environment read-only and lays its own writable layer over
	// it inside.
	//
	// The lock decides who builds. Two sandboxes starting at once on a
	// workspace whose environment has never been built would otherwise both
	// build it into the same file; instead one builds and the other waits,
	// and neither ends up with half of somebody else's work.
	// noEnv is why this sandbox ends up without an environment, when it does.
	// A sandbox that quietly came up as bare busybox while the project has a
	// Dockerfile is one a developer debugs by guessing.
	noEnv := ""
	if envImage != "" {
		envLock, err := waitForEnvironment(ctx, v.stateDir, env.ID)
		if err != nil {
			return err
		}
		if envLock != nil {
			// A workspace that had another environment built is one whose
			// definition changed since it last ran — a pull, an edit while john
			// was not running. Say so, and that nothing unapplied is lost,
			// rather than start what looks like a first build.
			buildReport := report
			if hadOtherEnvironment(v.stateDir, env.ID) {
				buildReport = func(step int, label, detail string) {
					if label == "building environment" {
						label = "environment changed since the last run · rebuilding, unapplied work is kept"
					}
					report(step, label, detail)
				}
			}
			err := v.buildEnvironmentMachine(ctx, boot, env, base, df, buildReport)
			releaseLock(envLock)
			if err != nil && !errors.Is(err, errNoEnvironmentHere) {
				return err
			}
			if errors.Is(err, errNoEnvironmentHere) {
				// This machine cannot host an environment at all, so there is
				// no disk to attach and nothing to share.
				envImage = ""
				noEnv = "this sandbox has no environment: the " + v.hv.Name() +
					" machine runs the agent on this computer rather than inside one, " +
					"so there is no filesystem of its own to build " + env.Describe() + " into"
			}
		}
	}

	mount := v.GuestMount()
	mach, err := boot(false)
	if err != nil {
		return err
	}

	// The lower layer goes up before anything else: the guest asks about the
	// developer's tree, and it must never ask into a closed socket.
	report(3, "mounting workspace", mount)
	host := newHostFS(v.workdir, v.protections())
	go serveRPC(context.WithoutCancel(ctx), mach.Lower(), host.serve)

	ctrl := newRPCClient(mach.Control())
	ctrl.probeOp = "ping" // a call past its deadline checks the guest is still there
	if err := waitForAgent(ctx, ctrl); err != nil {
		mach.Stop(context.Background())
		return err
	}
	if err := setGuestClock(ctx, ctrl); err != nil {
		mach.Stop(context.Background())
		return err
	}
	if err := ctrl.callJSON(ctx, "protect", map[string]any{"protections": v.protections()}, nil); err != nil {
		mach.Stop(context.Background())
		return err
	}

	// The environment, and with it the route out. It goes in before the
	// workspace is mounted because the environment build is the first thing
	// that needs the network, and a sandbox that came up without its proxy
	// would fetch nothing and say nothing about why.
	var routed struct {
		Proxy string `json:"proxy"`
	}
	if err := ctrl.callJSON(ctx, "environ", v.environPayload(mach.ProxyAddr()), &routed); err != nil {
		mach.Stop(context.Background())
		return fmt.Errorf("sandbox: wiring the sandbox's environment: %w", err)
	}
	var mounted struct {
		Mounted bool   `json:"mounted"`
		Reason  string `json:"reason"`
	}
	if err := ctrl.callJSON(ctx, "mount", struct{}{}, &mounted); err != nil {
		mach.Stop(context.Background())
		return fmt.Errorf("sandbox: mounting the workspace: %w", err)
	}

	v.mu.Lock()
	v.mach, v.ctrl, v.host, v.image, v.lock, v.inst = mach, ctrl, host, env.Describe(), lock, inst
	v.mounted, v.unmountedReason = mounted.Mounted, mounted.Reason
	v.routed = routed.Proxy != ""
	v.noEnvironment = noEnv
	if base == "" {
		v.noEnvironment = "this sandbox has no environment: no " + EnvFile +
			" in the project and no default environment configured"
	}
	v.mu.Unlock()

	report(4, "sandbox ready", "")
	return nil
}

// Protect installs the workspace protections, persists them for the
// sessionless `john apply`, and pushes them into a running machine — on both
// sides. The host side decides what the mount shows at all, which is what
// makes an exclusion hold against a root shell; the guest side decides what
// may be written, which is what makes a read-only file behave like one.
func (v *VM) Protect(p policy.Protections) error {
	v.mu.Lock()
	v.prot = p
	host, ctrl := v.host, v.ctrl
	v.mu.Unlock()

	if err := v.stateDir.setProtections(p); err != nil {
		return err
	}
	if host != nil {
		host.setProtections(p)
	}
	if ctrl != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		return ctrl.callJSON(ctx, "protect", map[string]any{"protections": p}, nil)
	}
	return nil
}

func (v *VM) protections() policy.Protections {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.prot
}

// client returns the control stream, or the reason there is not one.
func (v *VM) client() (*rpcClient, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.ctrl == nil {
		return nil, errors.New("sandbox: not running; build it first")
	}
	return v.ctrl, nil
}

func (v *VM) Read(ctx context.Context, path string, maxBytes int) (Content, error) {
	p, err := CleanPath(path)
	if err != nil {
		return Content{}, err
	}
	c, err := v.client()
	if err != nil {
		return Content{}, err
	}
	f, err := c.call(ctx, "read", map[string]any{"path": p, "max_bytes": maxBytes}, nil)
	if err != nil {
		return Content{}, err
	}
	var body struct {
		Truncated bool `json:"truncated"`
	}
	if len(f.Body) > 0 {
		if err := jsonUnmarshal(f.Body, &body); err != nil {
			return Content{}, err
		}
	}
	if len(f.blobs) != 1 {
		return Content{}, errors.New("sandbox: read returned no content")
	}
	return Content{Data: f.blobs[0], Truncated: body.Truncated}, nil
}

func (v *VM) Write(ctx context.Context, path string, content []byte) error {
	p, err := CleanPath(path)
	if err != nil {
		return err
	}
	c, err := v.client()
	if err != nil {
		return err
	}
	_, err = c.call(ctx, "write", map[string]any{"path": p}, [][]byte{content})
	return err
}

func (v *VM) Delete(ctx context.Context, path string) error {
	p, err := CleanPath(path)
	if err != nil {
		return err
	}
	c, err := v.client()
	if err != nil {
		return err
	}
	return c.callJSON(ctx, "delete", map[string]any{"path": p}, nil)
}

func (v *VM) Exec(ctx context.Context, cmd, cwd string, timeout time.Duration) (ExecResult, error) {
	if cwd != "" && cwd != "." {
		if _, err := CleanPath(cwd); err != nil {
			return ExecResult{}, err
		}
	}
	c, err := v.client()
	if err != nil {
		return ExecResult{}, err
	}
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	// The guest enforces the timeout so the command is actually killed inside
	// the sandbox; the host waits a little longer so that a command stopped at
	// the deadline still gets to report what it produced.
	callCtx, cancel := context.WithTimeout(ctx, timeout+30*time.Second)
	defer cancel()

	f, err := c.call(callCtx, "exec", map[string]any{
		"cmd": cmd, "cwd": cwd, "timeout_ms": timeout.Milliseconds(), "env": v.opts.Env,
	}, nil)
	if err != nil {
		return ExecResult{}, err
	}
	var body struct {
		ExitCode   int   `json:"exit"`
		Truncated  bool  `json:"truncated"`
		DurationMS int64 `json:"duration_ms"`
	}
	if err := jsonUnmarshal(f.Body, &body); err != nil {
		return ExecResult{}, err
	}
	res := ExecResult{
		ExitCode:  body.ExitCode,
		Truncated: body.Truncated,
		Duration:  time.Duration(body.DurationMS) * time.Millisecond,
	}
	if len(f.blobs) > 0 {
		res.Stdout = f.blobs[0]
	}
	if len(f.blobs) > 1 {
		res.Stderr = f.blobs[1]
	}
	return res, nil
}

// Changed is the paths Diff would offer, from the same enumeration, without
// reading any of them out of the sandbox: what the status line counts after
// every tool call.
func (v *VM) Changed(ctx context.Context) ([]string, error) {
	c, err := v.client()
	if err != nil {
		return nil, err
	}
	var out struct {
		Changes []change `json:"changes"`
	}
	if err := c.callJSON(ctx, "changes", struct{}{}, &out); err != nil {
		return nil, err
	}
	prot := v.protections()
	var paths []string
	for _, ch := range out.Changes {
		hostPath, err := hostPathFor(v.workdir, ch.Path, false)
		if err != nil {
			continue // held back: it reaches the host through a link
		}
		if (FileDiff{Path: ch.Path}).protectedBy(prot) != "" {
			continue
		}
		before, readErr := readHostRegular(hostPath)
		_, statErr := os.Lstat(hostPath)
		if ch.Deleted {
			if statErr == nil {
				paths = append(paths, ch.Path)
			}
			continue
		}
		if readErr == nil {
			sum := sha256.Sum256(before)
			if hex.EncodeToString(sum[:]) == ch.Hash {
				continue
			}
		}
		paths = append(paths, ch.Path)
	}
	return paths, nil
}

// Diff is everything the sandbox has changed, as it stands right now.
//
// It costs an enumeration of the sandbox's own writes rather than a hash sweep
// of the whole project on both sides: the upper layer of the union *is* the
// diff. Files the sandbox never touched are not in it, because a live lower
// layer means an untouched file is not a copy that might have drifted — it is
// the developer's file, being read where it lies.
func (v *VM) Diff(ctx context.Context) ([]FileDiff, error) {
	c, err := v.client()
	if err != nil {
		return nil, err
	}
	var out struct {
		Changes []change `json:"changes"`
	}
	if err := c.callJSON(ctx, "changes", struct{}{}, &out); err != nil {
		return nil, err
	}

	prot := v.protections()
	diffs := make([]FileDiff, 0, len(out.Changes))
	for _, ch := range out.Changes {
		hostPath, err := hostPathFor(v.workdir, ch.Path, false)
		if err != nil {
			// The path reaches the developer's tree through a symbolic link,
			// so writing it would land outside the working directory. It is
			// shown and held back rather than dropped: a change the sandbox
			// made and Apply will not make is exactly what the held-back list
			// is for.
			diffs = append(diffs, FileDiff{Path: ch.Path, Deleted: ch.Deleted,
				Protected: heldSymlinked})
			continue
		}
		// A symbolic link in the developer's tree is present but is not
		// content: reading through it would compare the sandbox's file
		// against something else entirely.
		before, readErr := readHostRegular(hostPath)
		_, statErr := os.Lstat(hostPath)
		onHost := statErr == nil
		regular := readErr == nil

		if ch.Deleted {
			// A deletion of something the host does not have is not a change
			// to the host: the sandbox created it and then removed it again.
			if !onHost {
				continue
			}
			d := FileDiff{Path: ch.Path, Before: before, Deleted: true, Binary: looksBinary(before)}
			d.Protected = d.protectedBy(prot)
			diffs = append(diffs, d)
			continue
		}

		if regular {
			sum := sha256.Sum256(before)
			if hex.EncodeToString(sum[:]) == ch.Hash {
				continue // written back identical; not a change
			}
		}

		d := FileDiff{Path: ch.Path, Created: !onHost}
		if regular {
			d.Before = before
		}
		// A change at a protected path is reported rather than hidden — that
		// the agent wrote into .git is exactly what a developer should see —
		// but its content is not fetched. Apply will refuse it, so the bytes
		// can never leave, and an excluded tree the sandbox built for itself
		// can be very large.
		if d.Protected = d.protectedBy(prot); d.Protected != "" {
			diffs = append(diffs, d)
			continue
		}
		after, err := v.Read(ctx, ch.Path, 0)
		if err != nil {
			return nil, err
		}
		d.After = after.Data
		d.Binary = looksBinary(d.Before) || looksBinary(d.After)
		diffs = append(diffs, d)
	}

	sort.Slice(diffs, func(i, j int) bool { return diffs[i].Path < diffs[j].Path })
	return diffs, nil
}

// Apply writes the selected paths to the host working directory. This is the
// only code path anywhere that writes the host tree, and the protections are
// evaluated here again — against paths found by enumerating the sandbox, not
// against paths anything supplied — because a root shell inside the sandbox
// can talk its way past everything except this.
func (v *VM) Apply(ctx context.Context, paths []string) (int, error) {
	diffs, err := v.Diff(ctx)
	if err != nil {
		return 0, err
	}
	selected := map[string]bool{}
	for _, p := range paths {
		cp, err := CleanPath(p)
		if err != nil {
			return 0, err
		}
		selected[cp] = true
	}
	applied := 0
	var settled []change
	// Whatever was written is handed back to the developer's tree, even when
	// a later path fails: those files are on the host either way.
	defer func() {
		if len(settled) == 0 {
			return
		}
		if c, err := v.client(); err == nil {
			c.callJSON(ctx, "settle", struct {
				Paths []change `json:"paths"`
			}{settled}, nil)
		}
	}()
	for _, d := range diffs {
		if paths != nil && !selected[d.Path] {
			continue
		}
		if d.Protected != "" {
			continue
		}
		hostPath, err := hostPathFor(v.workdir, d.Path, false)
		if err != nil {
			return applied, err
		}
		if d.Deleted {
			if err := os.Remove(hostPath); err != nil && !os.IsNotExist(err) {
				return applied, err
			}
		} else {
			if err := os.MkdirAll(filepath.Dir(hostPath), 0o755); err != nil {
				return applied, err
			}
			// A symbolic link at the path is replaced, never written through.
			// Writing through it would put the sandbox's file wherever the
			// link happens to point, which may be nowhere near the workspace.
			if info, err := os.Lstat(hostPath); err == nil && info.Mode()&fs.ModeSymlink != 0 {
				if err := os.Remove(hostPath); err != nil {
					return applied, err
				}
			}
			if err := os.WriteFile(hostPath, d.After, 0o644); err != nil {
				return applied, err
			}
		}
		sum := sha256.Sum256(d.After)
		settled = append(settled, change{Path: d.Path, Deleted: d.Deleted, Hash: hex.EncodeToString(sum[:])})
		applied++
	}
	// The sandbox's copies of what was applied are dropped (above), so the
	// host's files show through again and the next Diff finds nothing at
	// those paths — until either side changes one.
	return applied, nil
}

// heldSymlinked is the reason given for a change whose path reaches the
// developer's tree through a symbolic link. It sits in the same field as a
// workspace-protection mode because it means the same thing to everyone who
// reads it: shown to the developer, and refused by Apply.
const heldSymlinked = "symlinked path"

// hostPathFor resolves a workspace-relative path against the developer's tree,
// following the symbolic links in it exactly as far as the sandbox does and
// no further.
//
// Apply is the only code anywhere that writes that tree, and this is what
// keeps that true of the tree rather than merely of the path: a checkout may
// contain a link to anywhere, so joining the path on in one go would let a
// write to `link/x` land wherever the link points. A link that stays inside
// the workspace is followed — a monorepo that links one package into another
// is a normal checkout, and refusing it would mean nothing under it could
// ever be applied — and one that leaves is refused by name.
//
// The last element is never followed. The sandbox already resolved it when it
// wrote, so following it again would only matter where the two trees disagree,
// and there the developer's link is the newer fact: Apply replaces it rather
// than writing through it.
func hostPathFor(root, rel string, followFinal bool) (string, error) {
	p, err := CleanPath(rel)
	if err != nil {
		return "", err
	}
	segs := strings.Split(p, "/")
	at, hops := "", 0
	for i := 0; i < len(segs); i++ {
		at = join(at, segs[i])
		if i == len(segs)-1 && !followFinal {
			break
		}
		for {
			info, err := os.Lstat(filepath.Join(root, filepath.FromSlash(at)))
			if err == nil && info.Mode()&fs.ModeIrregular != 0 {
				// A Windows junction, which Lstat does not call a link but
				// the operating system follows all the same, to anywhere.
				return "", fmt.Errorf("sandbox: %s: leads through a junction or other reparse point", rel)
			}
			if err != nil || info.Mode()&fs.ModeSymlink == 0 {
				break // not there, or not a link: nothing to follow
			}
			if hops++; hops > maxSymlinkHops {
				return "", fmt.Errorf("sandbox: %s: too many levels of symbolic links", rel)
			}
			target, err := os.Readlink(filepath.Join(root, filepath.FromSlash(at)))
			if err != nil {
				return "", err
			}
			if at, err = resolveLink(at, linkTarget(target)); err != nil {
				return "", err
			}
		}
	}
	return filepath.Join(root, filepath.FromSlash(at)), nil
}

// readHostRegular reads a file from the developer's tree, refusing anything
// that is not a regular file. Reading a symbolic link answers with whatever it
// points at rather than with the file the path names, which as a diff's
// "before" would be a comparison against the wrong file entirely.
func readHostRegular(host string) ([]byte, error) {
	info, err := os.Lstat(host)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s: not a regular file: %w", host, fs.ErrInvalid)
	}
	return os.ReadFile(host)
}

// Reset discards the sandbox entirely: the machine, its disks, and any work
// that was never applied.
func (v *VM) Reset(ctx context.Context) error {
	v.mu.Lock()
	mach, ctrl := v.mach, v.ctrl
	inst := v.inst
	v.mach, v.ctrl, v.host, v.image, v.inst = nil, nil, nil, "", ""
	v.mounted, v.unmountedReason, v.routed, v.noEnvironment = false, "", false, ""
	lock := v.lock
	v.lock = nil
	v.mu.Unlock()
	defer releaseLock(lock)

	settle(ctx, ctrl)
	if ctrl != nil {
		ctrl.Close()
	}
	if mach != nil {
		mach.Stop(ctx)
	}
	// The instance and everything in it: its disk, its unapplied work, the
	// record of what it was. The workspace's own settings — the limits, the
	// protections, the certificate authority — belong to the project rather
	// than to this line of work, and stay.
	if inst != "" {
		os.RemoveAll(string(inst))
	}
	return nil
}

// Restart stops the machine and starts it again: what to do with a sandbox
// that stopped answering. Its disk is kept, and with it everything not yet
// applied and everything installed; what is lost is whatever was running.
func (v *VM) Restart(ctx context.Context, progress func(Progress)) error {
	v.Stop(ctx)
	return v.Build(ctx, progress)
}

// Stop shuts the machine down without discarding it. The next Build brings
// back the same sandbox, with what was installed in it and what was not yet
// applied.
func (v *VM) Stop(ctx context.Context) error {
	// What this session leaves behind, written down while the sandbox can
	// still be asked. The upper layer is inside a filesystem image the host
	// cannot read, so a developer choosing between two sandboxes weeks later
	// has only what was recorded here to tell them apart.
	v.mu.Lock()
	inst, note, running := v.inst, v.note, v.mach != nil && v.ctrl != nil
	v.mu.Unlock()
	if inst != "" && running {
		changes := 0
		if diffs, err := v.Diff(ctx); err == nil {
			changes = len(diffs)
		}
		inst.recordSummary(changes, note)
	}

	v.mu.Lock()
	mach, ctrl := v.mach, v.ctrl
	v.mach, v.ctrl, v.host, v.inst = nil, nil, nil, ""
	v.mounted, v.unmountedReason, v.routed, v.noEnvironment = false, "", false, ""
	lock := v.lock
	v.lock = nil
	v.mu.Unlock()
	defer releaseLock(lock)

	// Ask the agent to settle the disk before the machine is stopped, because
	// stopping it is a power cut. A machine that will not answer is stopped
	// anyway — a sandbox that cannot be shut down would be worse than one
	// that loses the last few seconds.
	settle(ctx, ctrl)
	if ctrl != nil {
		ctrl.Close()
	}
	if mach != nil {
		return mach.Stop(ctx)
	}
	return nil
}

// settle gives the guest the moment it needs to flush its disk.
func settle(ctx context.Context, ctrl *rpcClient) {
	if ctrl == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	ctrl.callJSON(ctx, "shutdown", struct{}{}, nil)
}

// WorkspaceMounted reports whether a command run inside this sandbox has a
// workspace to run in, and why not when it has not.
//
// It is only ever false under the loopback machine on a host without FUSE,
// which is to say on a macOS or Windows developer's own operating system. That
// machine is not a sandbox and is never chosen automatically; everything the
// union does still works there, and Exec says what is missing rather than
// running the command somewhere else.
func (v *VM) WorkspaceMounted() (bool, string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.mounted, v.unmountedReason
}

// Routed reports whether the sandbox has a route out — which is to say, to
// the proxy and nowhere else. It is false when no proxy was configured and
// when the machine cannot carry one, and a false here is why an environment
// build that needs to fetch something will fail.
func (v *VM) Routed() bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.routed
}

// Environment reports the project's environment inside this sandbox: the
// description of what was built, and — when the machine could not host one at
// all — why not. A sandbox with no environment still works; what it does not
// have is the project's own tools, so a command that needs them says the
// command is missing and this is the reason.
func (v *VM) Environment() (string, string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.image, v.noEnvironment
}

// buildPassesThrough reports whether the environment build goes out through
// the pass-through proxy — direct, with one to go through — rather than the
// policy proxy that terminates TLS.
func (v *VM) buildPassesThrough() bool {
	direct := v.opts.BuildNetwork == "" || v.opts.BuildNetwork == policy.BuildDirect
	return direct && v.opts.BuildProxySocket != ""
}

// SetBuildProxy names the proxy a direct build goes through. Like SetProxy,
// it is how a machine is wired, so it has to be said before Build.
func (v *VM) SetBuildProxy(socketPath string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.opts.BuildProxySocket = socketPath
}

// SetBuildNetwork says where an environment build's traffic goes. It is the
// administrator's decision, carried in the signed policy snapshot, and it
// applies the next time the environment is built.
func (v *VM) SetBuildNetwork(where string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.opts.BuildNetwork = where
}

// running reports whether a machine is up, for the paths that have to know
// whether a half-finished Build left one behind.
func (v *VM) running() bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.mach != nil && v.ctrl != nil
}

// RebuildEnvironment throws this workspace's built environment away and brings
// the sandbox back on a freshly built one, keeping everything the sandbox has
// written and not yet applied.
//
// That is the difference between it and Reset, and the whole reason it exists.
// A toolchain that needs rebuilding — a half-finished install, a base image
// that moved, a Dockerfile that was right but built wrong — should not cost an
// afternoon's work. The environment is the workspace's and is rebuilt; the
// upper layer is this sandbox's and is untouched.
func (v *VM) RebuildEnvironment(ctx context.Context, progress func(Progress)) error {
	v.mu.Lock()
	inst := v.inst
	v.mu.Unlock()

	if err := v.Stop(ctx); err != nil {
		return err
	}
	if inst != "" {
		// Come back to the same sandbox, so the work is the same work.
		v.SetInstance(filepath.Base(string(inst)))
	}
	// This sandbox's own directory, not the one the workspace would resolve
	// to: a VM told where its state lives must not reach past it.
	if err := rebuildEnvironmentIn(v.stateDir); err != nil {
		return err
	}
	return v.Build(ctx, progress)
}

// SetInstance names the sandbox to use on the next Build. It is how a client
// answers errChooseInstance without the developer having to start john again
// with a flag.
func (v *VM) SetInstance(id string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.opts.Instance = id
}

// SetNote records what this session is working on, so that a developer coming
// back to two sandboxes can tell which was which. It is the first thing the
// session was asked to do, and it is written down when the sandbox stops.
func (v *VM) SetNote(note string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.note == "" {
		v.note = note
	}
}

// Instance names which of the workspace's sandboxes this session took. Empty
// until it has started one.
func (v *VM) Instance() string {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.inst == "" {
		return ""
	}
	return filepath.Base(string(v.inst))
}

// hadOtherEnvironment reports whether the workspace holds an environment
// image other than the one for envID.
func hadOtherEnvironment(dir stateDir, envID string) bool {
	entries, err := os.ReadDir(string(dir))
	if err != nil {
		return false
	}
	current := filepath.Base(envDiskPath(dir, envID))
	for _, e := range entries {
		if isEnvDisk(e.Name()) && e.Name() != current {
			return true
		}
	}
	return false
}

// StateDir is where this sandbox keeps what outlives it: its disks, the
// resource limits, the protections, and the proxy's certificate authority.
func (v *VM) StateDir() string { return string(v.stateDir) }

func (v *VM) Resources() Resources { return v.stateDir.resources() }

func (v *VM) SetResources(r Resources) error { return v.stateDir.setResources(r) }

// waitForAgent blocks until the agent inside answers, and gives up rather
// than hanging if it never does.
//
// Every machine needs this and not only the slow ones. A link is live from the
// moment the host end exists, which is before the guest has opened its end —
// on a serial port the first request can simply be dropped, and on a socket it
// sits in a buffer nobody is reading. Retrying a ping until it is answered is
// the one arrangement that works whether the machine is a process that started
// instantly or a kernel that is still bringing up its devices.
func waitForAgent(ctx context.Context, ctrl *rpcClient) error {
	deadline := time.Now().Add(2 * time.Minute)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	for attempt := 0; ; attempt++ {
		attemptCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		err := ctrl.callJSON(attemptCtx, "ping", struct{}{}, nil)
		cancel()
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("sandbox: the agent inside never answered: %w", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// setGuestClock gives the agent the host's time, the first thing after it
// answers. Every machine boots with whatever its real-time clock holds, and
// that was wrong on Hyper-V by the host's UTC offset: on a host west of UTC a
// sandbox behind the real time has apt refusing a Release file as "not valid
// yet" and TLS refusing a certificate issued that morning. Setting it here
// covers every machine type, including one whose clock is wrong some new way.
func setGuestClock(ctx context.Context, ctrl *rpcClient) error {
	if err := ctrl.callJSON(ctx, "settime", map[string]any{"unix_nano": time.Now().UnixNano()}, nil); err != nil {
		return fmt.Errorf("sandbox: setting the sandbox's clock: %w", err)
	}
	return nil
}

// HypervisorAvailable reports whether this machine can actually run the
// sandbox, and why not when it cannot — a developer whose firmware has
// virtualization switched off should be told that, rather than watching a
// boot fail in some more interesting way.
func (v *VM) HypervisorAvailable() error { return v.hv.Available() }

// Hypervisor names the machine this sandbox would run on.
func (v *VM) Hypervisor() string { return v.hv.Name() }
