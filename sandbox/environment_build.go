package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/popstarapps/john/policy"
)

// Building the environment.
//
// The host reads the project's definition, resolves the base image and streams
// it down; the guest unpacks it and runs the build's commands inside it. The
// split is not arbitrary — the host is where the proxy is and where the
// definition can be read from the developer's own disk, and the guest is the
// only side that is Linux.
//
// The definition is read from the HOST copy of .john/Dockerfile and never from
// the sandbox's. That is the property that makes an agent's edit to its own
// environment safe: the agent can change the file, the change arrives as a
// diff like any other, and it takes effect when the developer applies it. An
// agent cannot grant its own build network access by writing to a file it
// controls.
//
// The environment is identified by the content of the whole build context, so
// a changed definition is a different environment and rebuilds; an unchanged
// one is found already installed and costs a single round trip.

// errNoEnvironmentHere is the one build failure that is not one: this machine
// runs the agent somewhere an environment cannot be entered.
var errNoEnvironmentHere = errors.New("sandbox: an environment can only be built inside the Linux sandbox")

// envChunkSize is how much of a layer travels in one frame. The link caps a
// frame well above this; the smaller number is so that a slow pull reports
// progress rather than appearing to hang on a fifty-megabyte silence.
const envChunkSize = 4 << 20

// environmentPlan reads what this workspace's environment is going to be,
// without building anything.
//
// It is answered before a machine starts, because the answer decides how the
// machine is wired: a workspace with no environment attaches no environment
// disk and boots once, and a workspace whose environment is not built yet
// needs a machine that can write it.
//
// A base of "" is the sandbox as it boots — busybox and the agent. That is a
// real answer for a test and for a first look, and it is not the answer a
// developer wants, so nothing chooses it silently: the client names an image,
// and this is what a caller that named none gets.
func (v *VM) environmentPlan(env environment) (string, *dockerfile, error) {
	if env.Default() {
		if v.opts.DefaultEnvironment == "" {
			return "", &dockerfile{}, nil
		}
		df, err := parseDockerfile(v.opts.DefaultEnvironment)
		if err != nil {
			return "", nil, fmt.Errorf("sandbox: the built-in environment: %w", err)
		}
		return df.From, df, nil
	}
	text, err := os.ReadFile(env.Dockerfile)
	if err != nil {
		return "", nil, err
	}
	df, err := parseDockerfile(string(text))
	if err != nil {
		return "", nil, err
	}
	return df.From, df, nil
}

// buildEnvironment installs the project's environment, in a machine started
// for that and stopped after it.
func (v *VM) buildEnvironment(ctx context.Context, ctrl *rpcClient, env environment, base string, df *dockerfile, proxyEnv []string, report func(string)) error {
	if base == "" {
		return nil
	}

	var installed struct {
		ID        string `json:"id"`
		Supported bool   `json:"supported"`
	}
	if err := ctrl.callJSON(ctx, "envstate", struct{}{}, &installed); err != nil {
		return err
	}
	// An environment is a Linux root filesystem entered with chroot, so the
	// loopback machine — the agent as a process on the developer's own
	// operating system — cannot host one. That is the same shape as the
	// workspace mount: an answer rather than a failure, because everything
	// else about that machine still works and it is not a sandbox anyway.
	if !installed.Supported {
		return errNoEnvironmentHere
	}
	if installed.ID == env.ID && env.ID != "" {
		return nil
	}

	ref, err := parseImageRef(base)
	if err != nil {
		return err
	}
	report("pulling " + ref.String())

	// Where the build's own traffic goes is a policy decision, and it is
	// already made: direct by default, because a build is how a project gets
	// its compilers and is what makes the sandbox useful under a policy that
	// allows no egress at all.
	socket, caPEM := "", []byte(nil)
	if v.opts.BuildNetwork != "" && v.opts.BuildNetwork != policy.BuildDirect {
		socket, caPEM = v.opts.ProxySocket, v.opts.ProxyCAPEM
	}
	client := newRegistryClient(socket, caPEM, v.opts.RegistryCredentials)

	img, err := client.resolve(ctx, ref, runtime.GOARCH)
	if err != nil {
		return err
	}

	if err := ctrl.callJSON(ctx, "envbegin", struct{}{}, nil); err != nil {
		return err
	}
	for i, layer := range img.Layers {
		report(fmt.Sprintf("unpacking %s (layer %d of %d)", ref.Repo, i+1, len(img.Layers)))
		if err := v.streamLayer(ctx, ctrl, client, ref, layer); err != nil {
			return fmt.Errorf("sandbox: %s layer %d: %w", ref, i+1, err)
		}
	}

	// A build through the policy proxy has its TLS terminated there, and its
	// RUN steps are pointed at john's certificate by path — a path inside the
	// environment being built, where the image never put one.
	if trustsJohn(proxyEnv) && len(v.opts.ProxyCAPEM) > 0 {
		if _, err := ctrl.call(ctx, "envwrite", map[string]any{
			"path": guestCAPath, "mode": int64(0o644),
		}, [][]byte{v.opts.ProxyCAPEM}); err != nil {
			return fmt.Errorf("sandbox: putting the proxy's certificate in the environment: %w", err)
		}
	}

	// The image's own environment first, then the Dockerfile's: PATH comes
	// from the image and a build that lost it finds nothing in /usr/bin.
	buildEnv := append(append([]string{}, img.Env...), df.Env...)
	workdir := df.Workdir
	if workdir == "" {
		workdir = img.Workdir
	}

	for i, step := range df.Steps {
		stepEnv := append(append([]string{}, img.Env...), step.Env...)
		stepEnv = append(stepEnv, proxyEnv...)
		switch step.Op {
		case "run":
			report(fmt.Sprintf("%s (step %d of %d)", firstLine(step.Cmd), i+1, len(df.Steps)))
			if err := v.runStep(ctx, ctrl, step, stepEnv); err != nil {
				// A build step that fetches is the usual reason a build
				// fails, and a sandbox with no route out fails every one of
				// them the same way. Say so once here rather than leaving a
				// developer to read a DNS error and guess.
				if len(proxyEnv) == 0 {
					return fmt.Errorf("%w\n  This sandbox has no route out: john's proxy is the "+
						"sandbox's only way to reach a network, and none was configured", err)
				}
				return err
			}
		case "copy":
			report(fmt.Sprintf("copying %s (step %d of %d)", strings.Join(step.Src, " "), i+1, len(df.Steps)))
			if err := v.copyStep(ctx, ctrl, env, step); err != nil {
				return err
			}
		}
	}

	return ctrl.callJSON(ctx, "envcommit", map[string]any{
		"id": env.ID, "env": buildEnv, "workdir": workdir,
	}, nil)
}

// streamLayer sends one layer down in pieces and has the guest apply it.
func (v *VM) streamLayer(ctx context.Context, ctrl *rpcClient, c *registryClient, ref imageRef, layer descriptor) error {
	if strings.Contains(layer.MediaType, "zstd") {
		return fmt.Errorf("the layer is zstd-compressed, which john does not read yet")
	}
	blob, err := c.blob(ctx, ref, layer.Digest)
	if err != nil {
		return err
	}
	defer blob.Close()

	buf := make([]byte, envChunkSize)
	for {
		n, err := io.ReadFull(blob, buf)
		if n > 0 {
			if _, err := ctrl.call(ctx, "envstage", struct{}{}, [][]byte{buf[:n]}); err != nil {
				return err
			}
		}
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			break
		}
		if err != nil {
			return err
		}
	}
	return ctrl.callJSON(ctx, "envunpack", struct{}{}, nil)
}

// runStep runs one RUN, and fails the build when it fails. A build whose
// commands failed is not an environment with a few things missing — it is one
// that does not match its definition, and continuing would leave a developer
// debugging a toolchain that was never installed.
func (v *VM) runStep(ctx context.Context, ctrl *rpcClient, step buildStep, env []string) error {
	f, err := ctrl.call(ctx, "envrun", map[string]any{
		"cmd": step.Cmd, "env": env, "workdir": step.Workdir,
	}, nil)
	if err != nil {
		return err
	}
	var res struct {
		ExitCode int `json:"exit_code"`
	}
	if len(f.Body) > 0 {
		if err := json.Unmarshal(f.Body, &res); err != nil {
			return err
		}
	}
	if res.ExitCode != 0 {
		detail := ""
		for _, b := range f.blobs {
			if t := strings.TrimSpace(string(b)); t != "" {
				detail += t + "\n"
			}
		}
		return fmt.Errorf("sandbox: %s: RUN %s\n  exited %d\n%s",
			EnvFile, firstLine(step.Cmd), res.ExitCode, indent(strings.TrimSpace(detail)))
	}
	return nil
}

// copyStep sends the files a COPY names, read from the build context on the
// host — which is .john/, and not the project, so the environment cannot
// quietly depend on the thing it is sandboxing.
func (v *VM) copyStep(ctx context.Context, ctrl *rpcClient, env environment, step buildStep) error {
	for _, src := range step.Src {
		clean := filepath.Clean(filepath.FromSlash(strings.TrimPrefix(src, "./")))
		full := filepath.Join(env.Context, clean)
		if !within(env.Context, full) {
			return fmt.Errorf("sandbox: %s: COPY %s reaches outside %s, which is the build context",
				EnvFile, src, EnvDir)
		}
		info, err := os.Stat(full)
		if err != nil {
			return fmt.Errorf("sandbox: %s: COPY %s: %w", EnvFile, src, err)
		}
		if info.IsDir() {
			err = filepath.Walk(full, func(p string, fi os.FileInfo, err error) error {
				if err != nil || fi.IsDir() {
					return err
				}
				rel, _ := filepath.Rel(full, p)
				return v.sendFile(ctx, ctrl, p, path.Join(step.Dst, filepath.ToSlash(rel)), fi)
			})
			if err != nil {
				return err
			}
			continue
		}
		dst := step.Dst
		if strings.HasSuffix(dst, "/") {
			dst = path.Join(dst, filepath.Base(clean))
		}
		if err := v.sendFile(ctx, ctrl, full, dst, info); err != nil {
			return err
		}
	}
	return nil
}

func (v *VM) sendFile(ctx context.Context, ctrl *rpcClient, hostPath, guestPath string, info os.FileInfo) error {
	data, err := os.ReadFile(hostPath)
	if err != nil {
		return err
	}
	_, err = ctrl.call(ctx, "envwrite", map[string]any{
		"path": guestPath, "mode": int64(info.Mode().Perm()),
	}, [][]byte{data})
	return err
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 60 {
		s = s[:57] + "..."
	}
	return strings.TrimSpace(s)
}

func indent(s string) string {
	if s == "" {
		return ""
	}
	return "  " + strings.ReplaceAll(s, "\n", "\n  ")
}

// envDiskBytes is how big the environment's disk is made.
//
// It is not the sandbox's disk limit and is deliberately generous: an
// environment is a base image plus a toolchain, which is hundreds of megabytes
// and occasionally several gigabytes, and it is sparse so it costs what is
// written rather than what it could hold. What it is not is unbounded — a
// runaway build fills a disk john made rather than the developer's.
const envDiskBytes = 32 << 30

// envBuilt reports whether this workspace's environment has been built and
// finished. The marker is written by the machine that built it, after the
// guest has said the build is committed.
func envBuilt(dir stateDir, envID string) bool {
	_, err := os.Stat(dir.path("env-" + envID + ".built"))
	return err == nil
}

// markEnvBuilt records that the environment is finished and can be shared.
func markEnvBuilt(dir stateDir, envID string) error {
	return os.WriteFile(dir.path("env-"+envID+".built"), []byte(time.Now().Format(time.RFC3339)+"\n"), 0o600)
}

// waitForEnvironment decides who builds the workspace's environment.
//
// It returns a held lock for the one machine that will build it, and nil for
// everyone else — which means the environment is already there and can be
// attached read-only. A machine that arrives while another is building waits
// for it rather than building a second copy into the same file.
func waitForEnvironment(ctx context.Context, dir stateDir, envID string) (*os.File, error) {
	if envBuilt(dir, envID) {
		return nil, nil
	}
	if err := dir.ensure(); err != nil {
		return nil, err
	}
	deadline := time.Now().Add(30 * time.Minute)
	for {
		lock, err := takeLock(dir.path("env-" + envID + ".lock"))
		if err == nil {
			// Taken between the check above and here: somebody built it while
			// this one was waiting, and there is nothing left to do.
			if envBuilt(dir, envID) {
				releaseLock(lock)
				return nil, nil
			}
			return lock, nil
		}
		if !errors.Is(err, errSandboxBusy) {
			return nil, err
		}
		if envBuilt(dir, envID) {
			return nil, nil
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("sandbox: another john has been building this workspace's " +
				"environment for half an hour; something is wrong with the build")
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// buildEnvironmentMachine starts a machine with the environment disk
// writable, builds into it, and stops it.
//
// It is a whole machine for one job and it is worth it: the alternative is a
// session that holds the environment disk read-write for as long as it runs,
// which no other sandbox in the workspace could then attach at all. A boot
// costs a third of a second and happens once per environment.
func (v *VM) buildEnvironmentMachine(ctx context.Context, boot func(bool) (machine, error),
	env environment, base string, df *dockerfile, report func(int, string, string)) error {

	report(2, "building environment", env.Describe())
	mach, err := boot(true)
	if err != nil {
		return err
	}
	defer mach.Stop(context.WithoutCancel(ctx))

	// The lower layer, because the agent opens it before anything else — it
	// serves nothing a build reads, but a guest that cannot open it does not
	// come up at all.
	host := newHostFS(v.workdir, v.protections())
	go serveRPC(context.WithoutCancel(ctx), mach.Lower(), host.serve)

	ctrl := newRPCClient(mach.Control())
	defer ctrl.Close()
	if err := waitForAgent(ctx, ctrl); err != nil {
		return err
	}
	// Before anything is fetched: a build is apt and TLS, and both judge
	// what they download by the clock.
	if err := setGuestClock(ctx, ctrl); err != nil {
		return err
	}

	var routed struct {
		Proxy string `json:"proxy"`
	}
	if err := ctrl.callJSON(ctx, "environ", v.environPayload(mach.ProxyAddr()), &routed); err != nil {
		return err
	}

	// Through the pass-through proxy nothing is decrypted, so there is no
	// certificate of john's for the build to trust; through the policy proxy
	// there is, and it goes into the environment being built.
	if err := v.buildEnvironment(ctx, ctrl, env, base, df, proxyEnviron(routed.Proxy, "", !v.buildPassesThrough()),
		func(step string) { report(2, "building environment", step) }); err != nil {
		return err
	}
	// Settled first, then declared. The disk has to be unmounted and flushed
	// before another sandbox may attach it read-only — a filesystem with a
	// journal still to replay is one a read-only mount refuses outright.
	settle(context.WithoutCancel(ctx), ctrl)
	return markEnvBuilt(v.stateDir, env.ID)
}
