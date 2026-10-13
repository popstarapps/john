package sandbox

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// QEMU: a verification harness, not the machine john ships.
//
// The machines john ships are the platform's own — Virtualization.framework,
// Hyper-V, KVM. This one exists because it runs the guest under pure
// emulation, with no hardware virtualization at all, and so it can boot a real
// Linux kernel on a build machine that is itself a virtual machine with no
// nested virtualization. That makes the boundary testable: a real kernel, real
// virtio devices, the agent as its own process inside a machine, and the
// workspace mounted across an actual wall rather than a socket pair.
//
// It is deliberately not discoverable. Nothing selects it automatically and
// there are no defaults to fall back on: every path has to be supplied, so it
// cannot become the sandbox somebody got by accident on a machine where the
// real one failed to start.
//
// The guest link runs over virtio serial ports rather than vsock. vsock is
// what the shipping machines use and what the agent prefers, but it needs
// /dev/vhost-vsock on the host, which a build machine may not grant — and the
// transport is a property of the machine, which is exactly why the agent takes
// the address to reach the host as a string instead of choosing for itself.
type qemu struct {
	// Binary is the qemu-system-<arch> to run.
	Binary string
	// Kernel is a bootable Linux image. An EFI-only image will not do: this
	// boots the kernel directly, with no firmware in between.
	Kernel string
	// Initramfs holds the guest: an init, a shell, and the john binary.
	Initramfs string
	// Env is extra environment for the QEMU process, for a build that has to
	// be told where its libraries are.
	Env []string
	// Machine and CPU override the emulated hardware.
	Machine string
	CPU     string
	// DataDir is where this QEMU keeps its firmware blobs, for a build that
	// is not installed where it expects to be. An x86 guest needs a BIOS even
	// when the kernel is loaded directly, which an ARM `virt` guest does not —
	// so a harness that works for one and not the other usually differs only
	// here.
	DataDir string
	// Console, when set, is a file the guest's kernel log is written to.
	// Booting is where this goes wrong, and a boot with no log is a boot
	// nobody can debug.
	Console string
}

func (q *qemu) Name() string { return "qemu" }

// Available is about the machine, not about what it will boot: the image
// arrives in the boot config, extracted from the binary, and a machine that
// cannot run at all is a different answer from a build carrying no image.
func (q *qemu) Available() error { return q.available("", "") }

func (q *qemu) available(kernel, initramfs string) error {
	files := []struct{ what, path string }{{"binary", q.Binary}}
	if kernel != "" || initramfs != "" {
		files = append(files, struct{ what, path string }{"kernel", kernel},
			struct{ what, path string }{"initramfs", initramfs})
	}
	for _, f := range files {
		if f.path == "" {
			return fmt.Errorf("sandbox: qemu harness has no %s configured", f.what)
		}
		if _, err := os.Stat(f.path); err != nil {
			return fmt.Errorf("sandbox: qemu %s: %w", f.what, err)
		}
	}
	return nil
}

func (q *qemu) Boot(ctx context.Context, cfg bootConfig) (machine, error) {
	// The image normally arrives in the boot config, extracted from the
	// binary; the fields on the harness override it so a test can point at
	// one it built itself.
	kernel, initramfs := q.Kernel, q.Initramfs
	if kernel == "" {
		kernel = cfg.Kernel
	}
	if initramfs == "" {
		initramfs = cfg.Initramfs
	}
	if kernel == "" || initramfs == "" {
		return nil, ErrNoGuestImage
	}
	if err := q.available(kernel, initramfs); err != nil {
		return nil, err
	}

	// The sockets QEMU listens on. They live in a short directory of their
	// own because AF_UNIX paths are capped near 108 bytes and a sandbox state
	// directory is nowhere near short enough — the same reason the proxy puts
	// its socket in the temp dir.
	sockDir, err := os.MkdirTemp("", "jq")
	if err != nil {
		return nil, err
	}
	m := &qemuMachine{sockDir: sockDir}
	ctrlSock := filepath.Join(sockDir, "c")
	lowerSock := filepath.Join(sockDir, "l")

	guest := GuestConfig{
		Mount: cfg.Mount,
		Upper: cfg.Upper,
		// The order the ports are added below is the order the guest numbers
		// them, so control is port 1 and the lower layer is port 2.
		Control:        "dev:/dev/vport0p1",
		Lower:          "dev:/dev/vport0p2",
		MaxOutputBytes: cfg.MaxOutputBytes,
		MaxProcesses:   cfg.Resources.Processes,
	}
	raw, err := jsonMarshal(guest)
	if err != nil {
		m.Stop(context.Background())
		return nil, err
	}

	disk, err := stateDisk(cfg)
	if err != nil {
		m.Stop(context.Background())
		return nil, err
	}

	machineType, cpu, serial := q.target()
	console := "console=" + serial
	if q.Console == "" {
		console += " quiet"
	}

	args := []string{
		"-machine", machineType,
		// Emulation, not acceleration. The whole point of this harness is to
		// run where there is no hypervisor to accelerate with.
		"-accel", "tcg",
		"-cpu", cpu,
		"-smp", strconv.Itoa(max(cfg.Resources.CPUs, 1)),
		"-m", strconv.Itoa(max(cfg.Resources.MemoryGB, 1) * 1024),
		// The sandbox has no network of its own. On the shipping machines the
		// only route out is the proxy; here there is no route at all, which is
		// the same claim tested more strictly.
		"-nic", "none",
		"-kernel", kernel,
		"-initrd", initramfs,
		"-append", console + " panic=1 john.cfg=" + encodeGuestConfig(raw),
		"-device", "virtio-serial-pci",
		"-chardev", "socket,id=ctrl,path=" + ctrlSock + ",server=on,wait=off",
		"-device", "virtserialport,chardev=ctrl,name=john.control",
		"-chardev", "socket,id=lower,path=" + lowerSock + ",server=on,wait=off",
		"-device", "virtserialport,chardev=lower,name=john.lower",
		// The sandbox's own writes go on a disk here too. The harness does not
		// need one — memory would do for a test — but a machine the shipping
		// machines are checked against should be wired the way they are, and
		// the guest's first boot making a filesystem is exactly the step worth
		// running somewhere that needs no hypervisor.
		//
		// The disk is made by the package rather than by this harness, in
		// whichever format the platform's own machine attaches: raw everywhere
		// but Windows, where Hyper-V takes a VHDX and nothing else. QEMU reads
		// both and has to be told which.
		"-drive", "file=" + disk + ",format=" + diskFormat + ",if=none,id=state",
		"-device", "virtio-blk-pci,drive=state",
		// An entropy source, for the same reason the shipping machines have
		// one: the first thing in the guest to want randomness is mke2fs, and
		// a kernel with an empty pool makes it wait.
		"-device", "virtio-rng-pci",
		"-nographic", "-no-reboot",
	}
	// The workspace's environment, on a second disk, read-only for every
	// machine but the one building it.
	if cfg.EnvDisk != "" {
		mode := ",readonly=on"
		if cfg.EnvWritable {
			mode = ""
		}
		args = append(args,
			"-drive", "file="+cfg.EnvDisk+",format="+diskFormat+",if=none,id=env"+mode,
			"-device", "virtio-blk-pci,drive=env")
	}
	if q.DataDir != "" {
		args = append(args, "-L", q.DataDir)
	}

	cmd := exec.Command(q.Binary, args...)
	cmd.Env = append(os.Environ(), q.Env...)
	if q.Console != "" {
		log, err := os.Create(q.Console)
		if err != nil {
			m.Stop(context.Background())
			return nil, err
		}
		m.log = log
		cmd.Stdout, cmd.Stderr = log, log
	} else {
		cmd.Stdout, cmd.Stderr = nil, os.Stderr
	}
	if err := cmd.Start(); err != nil {
		m.Stop(context.Background())
		return nil, err
	}
	m.proc = cmd

	// QEMU creates its sockets as it starts, and the guest opens its end of
	// them somewhat later. Dialling only needs the first; VM.Build waits for
	// the agent itself, because that wait is one every machine needs.
	if m.lower, err = dialWithin(lowerSock, 30*time.Second); err != nil {
		m.Stop(context.Background())
		return nil, fmt.Errorf("sandbox: qemu lower channel: %w", err)
	}
	if m.control, err = dialWithin(ctrlSock, 30*time.Second); err != nil {
		m.Stop(context.Background())
		return nil, fmt.Errorf("sandbox: qemu control channel: %w", err)
	}
	return m, nil
}

// dialWithin waits for a socket to accept, rather than forever.
func dialWithin(path string, d time.Duration) (net.Conn, error) {
	deadline := time.Now().Add(d)
	for {
		conn, err := net.Dial("unix", path)
		if err == nil {
			return conn, nil
		}
		if time.Now().After(deadline) {
			return nil, err
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// target is the emulated hardware for the architecture this QEMU builds for,
// which is not always the one the host runs. The three differ by more than a
// name: an x86 guest's first serial port is ttyS0 and an ARM guest's is
// ttyAMA0, and a kernel told the wrong one boots silently — which looks
// exactly like a kernel that did not boot.
func (q *qemu) target() (machineType, cpu, serial string) {
	machineType, cpu, serial = "virt", "cortex-a72", "ttyAMA0"
	if strings.Contains(filepath.Base(q.Binary), "x86_64") {
		machineType, cpu, serial = "q35", "max", "ttyS0"
	}
	if q.Machine != "" {
		machineType = q.Machine
	}
	if q.CPU != "" {
		cpu = q.CPU
	}
	return machineType, cpu, serial
}

type qemuMachine struct {
	sockDir string
	proc    *exec.Cmd
	log     *os.File
	control net.Conn
	lower   net.Conn
	once    sync.Once
}

func (m *qemuMachine) Control() net.Conn { return m.control }
func (m *qemuMachine) Lower() net.Conn   { return m.lower }
func (m *qemuMachine) Root() string      { return "" }

// The harness offers no route out. A virtio serial port is a single stream of
// bytes with no way to tell one connection from another, and a proxy needs
// many at once — so carrying one here would mean inventing a multiplexer that
// exists nowhere else. The machines that ship have a socket device and do not
// need it; what this harness covers is everything else.
func (m *qemuMachine) ProxyAddr() string { return "" }

func (m *qemuMachine) Stop(ctx context.Context) error {
	m.once.Do(func() {
		if m.control != nil {
			m.control.Close()
		}
		if m.lower != nil {
			m.lower.Close()
		}
		// The guest powers off by itself once its link closes, after its
		// init has unmounted the disks (-no-reboot makes that QEMU exiting);
		// the kill is for one that has not managed it in time.
		if m.proc != nil && m.proc.Process != nil {
			exited := make(chan struct{})
			go func() {
				m.proc.Wait()
				close(exited)
			}()
			select {
			case <-exited:
			case <-time.After(shutdownGrace):
				m.proc.Process.Kill()
				<-exited
			}
		}
		if m.log != nil {
			m.log.Close()
		}
		os.RemoveAll(m.sockDir)
	})
	return nil
}
