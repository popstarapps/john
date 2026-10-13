package sandbox

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// A machine is what actually isolates the sandbox. The rest of the package
// programs against this and nothing else, so the union, the protections, the
// diff and the apply path are the same code whichever hypervisor is under
// them — and so a bug in the workspace is never a bug that only reproduces on
// one operating system.
//
// Implementations:
//
//	loopback  the agent as a host process. No isolation at all; it exists so
//	          that every layer above the hypervisor can be tested, and so
//	          john can be developed on a machine with no hypervisor.
//	krun      Linux, KVM.
//	vz        macOS, Virtualization.framework.
//	hyperv    Windows, Hyper-V.
type hypervisor interface {
	// Name is what the sandbox reports it is running on.
	Name() string
	// Available reports whether this machine can run here, and says why not
	// when it cannot — a developer whose BIOS has virtualization disabled
	// should be told that, not handed a boot failure.
	Available() error
	// Boot starts the machine and returns once the guest agent has dialed
	// back on both streams.
	Boot(ctx context.Context, cfg bootConfig) (machine, error)
}

// bootConfig is everything a machine needs to start one sandbox.
type bootConfig struct {
	// Dir is the sandbox's state directory on the host.
	Dir string
	// Agent is the host path of the binary to run inside as the guest agent.
	Agent string
	// Mount is where the workspace is mounted inside the sandbox.
	Mount string
	// Upper is where the sandbox's own writes are kept, as the guest sees it.
	Upper string
	// Env is the machine-wide environment inside the sandbox: the proxy and
	// the certificate authority that lets it terminate TLS.
	Env []string
	// ProxySocket is the host path of the socket the john proxy listens on,
	// and the machine's only route out.
	ProxySocket string
	// ProxyCAPEM goes into the sandbox trust store.
	ProxyCAPEM []byte
	// BuildNetwork is where the environment build's traffic goes. It is a
	// property of how the machine is wired while it builds, which is why it
	// is here rather than a check somewhere later.
	BuildNetwork string
	// Environment is the project's sandbox definition, already resolved.
	Environment environment
	// EnvDisk is the workspace's environment image: one disk shared by every
	// sandbox in the workspace, because the environment is expensive and
	// identical for all of them. EnvWritable says whether this machine is the
	// one building it — exactly one is, and everybody else attaches it
	// read-only and lays their own writable layer over it inside.
	EnvDisk     string
	EnvWritable bool
	// Kernel and Initramfs are the guest image, extracted and ready to boot.
	// Every machine boots the same pair; what differs between them is how.
	Kernel    string
	Initramfs string
	// Resources are the limits the machine starts with. They are properties
	// of the machine, so a machine that is already running keeps the ones it
	// started with.
	Resources Resources
	// MaxOutputBytes caps what one Exec sends back.
	MaxOutputBytes int
}

// machine is one running sandbox.
type machine interface {
	// Control is the stream the host issues tool calls on.
	Control() net.Conn
	// Lower is the stream the guest asks about the host working directory
	// on. The host serves it; the guest is the one that asks.
	Lower() net.Conn
	// Root is the prefix the guest's absolute paths sit under on the host.
	// A real machine has its own filesystem and answers ""; the loopback
	// machine, which is a process, answers with the directory that stands in
	// for the guest's root.
	Root() string
	// ProxyAddr is the address the guest dials to reach the john proxy on the
	// host, in the same spelling as the guest link — or "" when this machine
	// cannot offer one.
	//
	// It is the machine's answer and not the sandbox's because it is a
	// property of how the machine is wired: a machine with a socket device
	// can carry the many concurrent connections a proxy needs, and one whose
	// only channel is a serial port cannot. The sandbox has no route out at
	// all when this is empty, which is the safe direction — a build that
	// cannot fetch is better than a sandbox that reaches the network by some
	// path nobody policed.
	ProxyAddr() string
	// Stop shuts the machine down.
	Stop(ctx context.Context) error
}

// shutdownGrace is how long Stop gives a guest to end on its own before
// pulling the power. Closing the control link is what asks it to go: the agent
// exits, and init unmounts the disks and powers off. A machine cut off before
// that leaves its state disk for the next boot to recover, and with it
// whatever the sandbox wrote last.
const shutdownGrace = 5 * time.Second

// envDiskPath is the workspace's environment image for one environment. It is
// named for the environment's identity, so a changed definition is a different
// file and the old one stays until it is reclaimed — a sandbox still running
// on it does not have the disk pulled out from under it.
func envDiskPath(dir stateDir, envID string) string {
	if envID == "" {
		return ""
	}
	return dir.path("env-" + envID + diskExt)
}

// isEnvDisk reports whether a directory entry in a workspace's state is one of
// its environment images. The extension is the platform's — diskExt — because
// what a machine will attach is not the same everywhere, and everything that
// enumerates these files has to agree with the machine that made them.
func isEnvDisk(name string) bool {
	return strings.HasPrefix(name, "env-") && strings.HasSuffix(name, diskExt)
}

// stateDisk is the sandbox's own disk, made if it is not there already.
//
// It costs what has been written to it and not what it could hold — a sparse
// image, or a dynamic VHDX where the machine takes one — and the guest makes a
// filesystem on it the first time it boots. The size is the point: everything
// the sandbox installs, builds and has not applied lives on it and cannot grow
// past it, which is the limit a container sharing the host's filesystem could
// never enforce.
//
// Every machine that boots a real kernel wants exactly this, so it lives here
// rather than in one of them.
func stateDisk(cfg bootConfig) (string, error) {
	path := filepath.Join(cfg.Dir, "state"+diskExt)
	// Only ever grown. Shrinking a disk out from under a sandbox that has
	// already written to it would lose the work it is kept for — and the
	// filesystem on it would not survive being cut short either.
	if err := makeDisk(path, int64(max(cfg.Resources.DiskGB, 1))<<30); err != nil {
		return "", err
	}
	return path, nil
}

// createConsole opens a machine's console log for a new boot, keeping the
// last one beside it as <name>.prev.log. Every start used to truncate it —
// and the start most likely to follow a boot that went wrong is a restart,
// which emptied exactly the log there was a reason to read. One previous boot
// is kept and no more, so the logs never grow.
func createConsole(path string) (*os.File, error) {
	if info, err := os.Stat(path); err == nil && info.Size() > 0 {
		os.Rename(path, strings.TrimSuffix(path, ".log")+".prev.log")
	}
	return os.Create(path)
}
