package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/popstarapps/john/policy"
)

// The sandbox, in a real machine.
//
// Every other test in this package runs the agent as a process on this
// machine, because that is fast and because everything above the hypervisor is
// the same code either way. This one boots a Linux kernel, lets the agent come
// up inside it as its own init, and drives the workspace across an actual
// virtual machine boundary — the guest link over a serial port, the union
// mounted by a kernel that has never seen the developer's disk, and a shell
// that can only reach the project through it.
//
// It runs where a guest image has been built:
//
//	sandbox/guest/mkimage.sh /tmp/guestimg
//	JOHN_QEMU=$(which qemu-system-aarch64) \
//	JOHN_QEMU_KERNEL=/tmp/guestimg/kernel \
//	JOHN_QEMU_INITRAMFS=/tmp/guestimg/initramfs.cpio.gz \
//	  go test ./sandbox -run InARealMachine
//
// and skips where one has not, because a test that needed a Linux image
// checked into the repository would not be run by anybody.
//
// qemuHarnessOrNil answers nil rather than skipping, for callers that have other
// implementations to fall back on.
func qemuHarnessOrNil(t *testing.T) *qemu {
	t.Helper()
	q := &qemu{
		Binary:    os.Getenv("JOHN_QEMU"),
		Kernel:    os.Getenv("JOHN_QEMU_KERNEL"),
		Initramfs: os.Getenv("JOHN_QEMU_INITRAMFS"),
		DataDir:   os.Getenv("JOHN_QEMU_DATA"),
		Console:   filepath.Join(t.TempDir(), "console.log"),
	}
	if env := os.Getenv("JOHN_QEMU_ENV"); env != "" {
		q.Env = strings.Split(env, " ")
	}
	// The harness needs a binary to run, and an image either handed to it or
	// carried by this build.
	if q.Available() != nil {
		return nil
	}
	if (q.Kernel == "" || q.Initramfs == "") && guestFS == nil {
		return nil
	}
	return q
}

// realMachine is one hypervisor on this host that boots an actual kernel, and
// the file its guest writes its console to.
type realMachine struct {
	name    string
	hv      hypervisor
	console string
	// proxySocket and proxyCAPEM wire the sandbox to a john proxy, for the
	// tests that are about the route out. Empty everywhere else, which is a
	// sandbox with no route at all.
	proxySocket string
	proxyCAPEM  []byte
}

// realMachines is every one of them here: the qemu harness where a binary and
// an image are configured, and the platform's own machine — the one john
// ships — where this build can start it.
//
// Both, rather than either. They are not alternatives: qemu proves the guest
// boots under emulation on a build machine with no hypervisor at all, and the
// platform machine proves the thing a developer will actually run. A test that
// took the first one it found would silently stop covering whichever was
// second on the machine that had both.
func realMachines(t *testing.T) []realMachine {
	t.Helper()
	var machines []realMachine
	if q := qemuHarnessOrNil(t); q != nil {
		machines = append(machines, realMachine{name: q.Name(), hv: q, console: q.Console})
	}
	if hv, console := platformHarnessOrNil(t); hv != nil {
		machines = append(machines, realMachine{name: hv.Name(), hv: hv, console: console})
	}
	return machines
}

// realMachinesWithProxy is realMachines for the machines that can carry a
// route to the john proxy, wired to one. A machine whose only channel is a
// serial port answers no to that, so the qemu harness is absent here and
// present everywhere else.
func realMachinesWithProxy(t *testing.T, socket string, caPEM []byte) []realMachine {
	t.Helper()
	machines := []realMachine{{name: "loopback", hv: &loopback{}}}
	if hv, console := platformHarnessOrNil(t); hv != nil {
		machines = append(machines, realMachine{name: hv.Name(), hv: hv, console: console})
	}
	var out []realMachine
	for _, m := range machines {
		m.proxySocket, m.proxyCAPEM = socket, caPEM
		out = append(out, m)
	}
	return out
}

// bootRealMachine brings a sandbox up over the working directory and reports
// the guest's console when it cannot, because a boot with no log is a boot
// nobody can debug.
// testDiskGB is the state disk every test boots with. The default is a hundred
// gigabytes, which is right for a developer and wasteful here: the disk is made
// and formatted once per test, and a test wants the smallest one that proves
// the same thing.
const testDiskGB = 2

func bootRealMachine(t *testing.T, m realMachine, workdir string, prot policy.Protections) (*VM, string) {
	t.Helper()
	vm, err := NewVM(workdir, VMOptions{StateDir: t.TempDir(), Protect: prot, Hypervisor: m.hv,
		ProxySocket: m.proxySocket, ProxyCAPEM: m.proxyCAPEM})
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
		if log, rerr := os.ReadFile(m.console); rerr == nil {
			t.Logf("guest console:\n%s", log)
		}
		t.Fatalf("boot: %v", err)
	}
	return vm, workdir
}

// eachRealMachine runs one set of assertions inside every machine that boots a
// real kernel.
func eachRealMachine(t *testing.T, files map[string]string, prot policy.Protections, fn func(t *testing.T, vm *VM, workdir string)) {
	t.Helper()
	machines := realMachines(t)
	if len(machines) == 0 {
		t.Skip("nothing here boots a real kernel; see sandbox/guest/mkimage.sh")
	}
	for _, m := range machines {
		t.Run(m.name, func(t *testing.T) {
			vm, workdir := bootRealMachine(t, m, hostTree(t, files), prot)
			fn(t, vm, workdir)
		})
	}
}

func TestTheSandboxWorksInARealMachine(t *testing.T) {
	eachRealMachine(t, map[string]string{
		"main.go":     "package main\n",
		"lib/util.go": "package lib\n",
	}, policy.Protections{}, func(t *testing.T, vm *VM, workdir string) {
		ctx := context.Background()

		// The workspace is mounted at the path the host keeps it at, inside a
		// kernel that has never seen the host's disk. On Windows "the host's
		// own path" is the transliteration rather than the literal one —
		// C:\Users\bob\project is not a path Linux can hold — so the
		// assertion is against the mirror the sandbox promises rather than
		// against the host string, and it is the same assertion everywhere
		// else, where mirrorPath is the identity.
		want := GuestMount(workdir)
		if got := strings.TrimSpace(string(run(t, vm, "pwd").Stdout)); got != want {
			t.Fatalf("pwd inside the machine = %q, want the host's own path %q", got, want)
		}

		// Reading reaches the developer's tree, one page at a time, over the link.
		content, err := vm.Read(ctx, "main.go", 0)
		if err != nil || string(content.Data) != "package main\n" {
			t.Fatalf("read = %q %v", content.Data, err)
		}
		if got := strings.TrimSpace(string(run(t, vm, "cat lib/util.go").Stdout)); got != "package lib" {
			t.Fatalf("a shell in the machine read %q", got)
		}

		// Writing lands in the machine's own layer and nowhere else.
		run(t, vm, "echo 'built here' > out.txt && echo '// more' >> main.go")
		if _, err := os.Stat(filepath.Join(workdir, "out.txt")); !os.IsNotExist(err) {
			t.Fatal("a command inside the machine created a file on the host")
		}
		onHost, _ := os.ReadFile(filepath.Join(workdir, "main.go"))
		if string(onHost) != "package main\n" {
			t.Fatalf("a command inside the machine changed the host's file: %q", onHost)
		}

		diffs, err := vm.Diff(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(diffs) != 2 {
			t.Fatalf("diff = %d entries, want 2: %+v", len(diffs), diffs)
		}
		if n, err := vm.Apply(ctx, nil); err != nil || n != 2 {
			t.Fatalf("apply = %d %v", n, err)
		}
		if got, _ := readHost(t, workdir, "out.txt"); strings.TrimSpace(got) != "built here" {
			t.Fatalf("out.txt on the host = %q", got)
		}
	})
}

// The live lower layer, across a real machine boundary: the guest has no disk
// of the developer's to have cached, so every read it does is a question it
// asks the host at the moment it asks.
func TestAHostEditReachesInsideARealMachine(t *testing.T) {
	eachRealMachine(t, map[string]string{"config.yaml": "version: 1\n"}, policy.Protections{},
		func(t *testing.T, vm *VM, workdir string) {

			if got := strings.TrimSpace(string(run(t, vm, "cat config.yaml").Stdout)); got != "version: 1" {
				t.Fatalf("cat = %q", got)
			}
			if err := os.WriteFile(filepath.Join(workdir, "config.yaml"), []byte("version: 2\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if got := strings.TrimSpace(string(run(t, vm, "cat config.yaml").Stdout)); got != "version: 2" {
				t.Fatalf("after a host edit the machine reads %q, want version: 2", got)
			}
		})
}

// Exclusion is enforced by the side of the mount the machine cannot reach.
// Inside a real machine that is not a figure of speech: the guest kernel has
// no path to the host's filesystem at all, so an excluded file is not
// something it is refused, it is something that was never sent.
func TestAnExcludedFileNeverEntersARealMachine(t *testing.T) {
	prot := protections(t, map[string][]string{policy.ModeExclude: {".env"}})
	eachRealMachine(t, map[string]string{
		".env":    "AWS_SECRET_ACCESS_KEY=real\n",
		"main.go": "package main\n",
	}, prot, func(t *testing.T, vm *VM, workdir string) {

		if res := run(t, vm, "cat .env"); res.ExitCode == 0 {
			t.Fatalf("a shell in the machine read an excluded file: %q", res.Stdout)
		}
		if got := string(run(t, vm, "ls -a").Stdout); strings.Contains(got, ".env") {
			t.Fatalf("an excluded file is listed inside the machine: %q", got)
		}
		// The workspace is the only part of the developer's machine the sandbox
		// can see at all, so it is the only place the secret could have got to.
		if got := string(run(t, vm, "grep -r AWS_SECRET . 2>/dev/null || true").Stdout); strings.Contains(got, "real") {
			t.Fatalf("an excluded secret was reachable from inside the machine: %q", got)
		}
		if got, _ := readHost(t, workdir, ".env"); got != "AWS_SECRET_ACCESS_KEY=real\n" {
			t.Fatalf("the excluded host file changed: %q", got)
		}
	})
}

// The sandbox's own writes go on the sandbox's own disk, and the disk is what
// makes the size limit real. A machine that boots without mounting it looks
// exactly like one that did — everything works, /john is memory, and the limit
// is a number nothing enforces — so this asserts the mount rather than the
// behaviour that survives without it.
func TestTheSandboxWritesToItsOwnDisk(t *testing.T) {
	eachRealMachine(t, map[string]string{"main.go": "package main\n"}, policy.Protections{},
		func(t *testing.T, vm *VM, _ string) {
			// The first boot of a sandbox finds a blank disk and makes a
			// filesystem on it; this is that filesystem, mounted.
			//
			// The device it lands on is the machine's business and not this
			// test's: vda under virtio, and under Hyper-V's SCSI bus whichever
			// of sda and sdb the kernel finished probing first, which is why
			// the guest picks it by LUN rather than by name. What is asserted
			// is that /john is a real disk with a real filesystem on it — a
			// machine that failed to mount one leaves /john on the rootfs,
			// which is neither.
			mounts := string(run(t, vm, "cat /proc/mounts").Stdout)
			if !mountedFromADisk(mounts, "/john", "ext4") {
				t.Fatalf("/john is not the sandbox's disk:\n%s", mounts)
			}

			// And it is where the writes land, rather than somewhere in
			// memory that happens to have the same path.
			// The union keeps what the sandbox wrote under <upper>/f, path
			// for path.
			run(t, vm, "echo 'built here' > out.txt")
			if got := string(run(t, vm, "cat /john/upper/f/out.txt").Stdout); strings.TrimSpace(got) != "built here" {
				t.Fatalf("the sandbox's write is not on its disk: %q", got)
			}

			// The disk is the size it was asked for, so the limit is a limit.
			df := string(run(t, vm, "df -k /john | tail -1").Stdout)
			fields := strings.Fields(df)
			if len(fields) < 2 {
				t.Fatalf("df /john = %q", df)
			}
			blocks, err := strconv.Atoi(fields[1])
			if err != nil {
				t.Fatalf("df /john = %q", df)
			}
			if want := testDiskGB << 20; blocks < want*8/10 || blocks > want {
				t.Errorf("the sandbox's disk holds %d KiB, want about %d", blocks, want)
			}
		})
}

// mountedFromADisk reports whether /proc/mounts says the path is a filesystem
// of that type on a block device, whatever the kernel called the device.
func mountedFromADisk(mounts, path, fstype string) bool {
	for _, line := range strings.Split(mounts, "\n") {
		f := strings.Fields(line)
		if len(f) >= 3 && strings.HasPrefix(f[0], "/dev/") && f[1] == path && f[2] == fstype {
			return true
		}
	}
	return false
}

// The machine has no network device at all, which is the claim the proxy
// depends on: the sandbox cannot reach anything john did not give it.
func TestARealMachineHasNoRouteOut(t *testing.T) {
	eachRealMachine(t, map[string]string{"main.go": "package main\n"}, policy.Protections{},
		func(t *testing.T, vm *VM, _ string) {

			got := string(run(t, vm, "ip link 2>/dev/null || ifconfig -a 2>/dev/null || cat /proc/net/dev").Stdout)
			for _, iface := range []string{"eth0", "ens", "enp"} {
				if strings.Contains(got, iface) {
					t.Fatalf("the machine has a network interface: %q", got)
				}
			}
		})
}

// The process limit is a limit.
//
// A developer sets it with /resources and john prints it back, and for a while
// nothing held it — the number reached the boot config and stopped there — so
// a sandbox told to run at most so many processes ran as many as it liked, and
// what john reported was not what was true. A runaway build is the ordinary
// case: `make -j` with no argument, a test suite that spawns one process per
// core on a machine that reports the host's.
func TestASandboxRunsNoMoreProcessesThanItWasGiven(t *testing.T) {
	machines := realMachines(t)
	if len(machines) == 0 {
		t.Skip("nothing here boots a real kernel; see sandbox/guest/mkimage.sh")
	}
	const limit = 24
	for _, m := range machines {
		t.Run(m.name, func(t *testing.T) {
			workdir := hostTree(t, map[string]string{"main.go": "package main\n"})
			vm, err := NewVM(workdir, VMOptions{StateDir: t.TempDir(), Hypervisor: m.hv})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { vm.Stop(context.Background()) })
			if err := vm.SetResources(Resources{CPUs: 2, MemoryGB: 2, DiskGB: testDiskGB, Processes: limit}); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			if err := vm.Build(ctx, nil); err != nil {
				if log, rerr := os.ReadFile(m.console); rerr == nil {
					t.Logf("guest console:\n%s", log)
				}
				t.Fatalf("boot: %v", err)
			}
			needsAShell(t, vm)

			// A modest burst, well inside the limit, runs clean.
			few := run(t, vm, "n=0; while [ $n -lt 8 ]; do sleep 5 & n=$((n+1)); done; echo started")
			if !strings.Contains(string(few.Stdout), "started") || strings.Contains(string(few.Stderr), "fork") {
				t.Fatalf("eight processes did not fit in a limit of %d: %q / %q", limit, few.Stdout, few.Stderr)
			}

			// Far more than it is allowed, started as fast as the shell can
			// start them. The kernel refuses the fork, and the shell says so:
			// the sandbox runs what it was given room for and no more.
			many := run(t, vm, "n=0; while [ $n -lt 200 ]; do sleep 20 & n=$((n+1)); done; echo survived")
			if !strings.Contains(string(many.Stderr), "fork") {
				t.Fatalf("200 processes started in a sandbox limited to %d: %q / %q",
					limit, many.Stdout, many.Stderr)
			}

			// And the sandbox comes back on its own as the burst drains. A
			// limit that leaves the machine it is protecting unusable has
			// protected nothing — the refused forks are the developer's to
			// see, and the next command is john's to keep answering.
			deadline := time.Now().Add(90 * time.Second)
			for {
				res, err := vm.Exec(context.Background(), "echo alive", "", 30*time.Second)
				if err == nil && strings.Contains(string(res.Stdout), "alive") {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("the sandbox never answered again after its process limit was hit: %v", err)
				}
				time.Sleep(time.Second)
			}
		})
	}
}

// A command answers when its shell does, whatever it left running.
//
// The agent backgrounds things — `npm install &`, a dev server — and the child
// keeps the output open after the shell has gone. Waiting for it held the
// answer until the host gave up, and the command came back as an error with
// nothing in it. And a command stopped at its deadline says so, rather than
// reporting the -1 of the shell that was killed.
func TestACommandAnswersWhenItsShellDoes(t *testing.T) {
	eachRealMachine(t, map[string]string{"main.go": "package main\n"}, policy.Protections{},
		func(t *testing.T, vm *VM, _ string) {

			start := time.Now()
			res := run(t, vm, "sleep 600 & echo started")
			if took := time.Since(start); took > time.Minute {
				t.Fatalf("a backgrounded child held the command for %v", took)
			}
			if res.ExitCode != 0 || !strings.Contains(string(res.Stdout), "started") {
				t.Fatalf("exit %d, stdout %q", res.ExitCode, res.Stdout)
			}

			res, err := vm.Exec(context.Background(), "sleep 600 | cat", "", 2*time.Second)
			if err != nil {
				t.Fatalf("a command at its deadline did not answer: %v", err)
			}
			if res.ExitCode != 124 || !strings.Contains(string(res.Stderr), "timed out") {
				t.Fatalf("at the deadline: exit %d, stderr %q", res.ExitCode, res.Stderr)
			}
		})
}

// Through the real filesystem, a refused rmdir is "Directory not empty" — the
// errno the kernel passes on to the program — and never an I/O error, which
// is what Node's rmSync, and with it every `vite build` after the first, gave
// up on.
func TestRmdirOfAFullDirectorySaysItIsNotEmpty(t *testing.T) {
	eachRealMachine(t, map[string]string{"main.go": "package main\n"}, policy.Protections{},
		func(t *testing.T, vm *VM, _ string) {
			needsAShell(t, vm)
			res := run(t, vm, "mkdir -p dist/assets && echo x > dist/assets/app.js && rmdir dist/assets 2>&1; true")
			out := string(res.Stdout) + string(res.Stderr)
			if strings.Contains(strings.ToLower(out), "i/o error") || !strings.Contains(strings.ToLower(out), "not empty") {
				t.Fatalf("rmdir of a full directory said %q", out)
			}
		})
}

// A sandbox restarted after it stopped answering comes back the same: what it
// wrote is still there, and it runs commands.
func TestARestartedSandboxKeepsItsWork(t *testing.T) {
	eachRealMachine(t, map[string]string{"main.go": "package main\n"}, policy.Protections{},
		func(t *testing.T, vm *VM, _ string) {
			needsAShell(t, vm)
			run(t, vm, "echo kept > notes.txt && sync")
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			if err := vm.Restart(ctx, nil); err != nil {
				t.Fatal(err)
			}
			if got := strings.TrimSpace(string(run(t, vm, "cat notes.txt").Stdout)); got != "kept" {
				t.Fatalf("after the restart notes.txt = %q", got)
			}
		})
}

// A new boot's console log keeps the last one beside it: a restart is the
// start most likely to follow a boot that went wrong, and it used to empty
// exactly the log there was a reason to read.
func TestABootKeepsTheLastBootsConsole(t *testing.T) {
	path := filepath.Join(t.TempDir(), "console.log")
	for _, boot := range []string{"first", "second", "third"} {
		f, err := createConsole(path)
		if err != nil {
			t.Fatal(err)
		}
		f.WriteString(boot + " boot\n")
		f.Close()
	}
	now, _ := os.ReadFile(path)
	prev, _ := os.ReadFile(strings.TrimSuffix(path, ".log") + ".prev.log")
	if string(now) != "third boot\n" || string(prev) != "second boot\n" {
		t.Fatalf("console.log = %q, console.prev.log = %q", now, prev)
	}
}

// The sandbox keeps the host's time. Hyper-V once gave its guest the host's
// local time in a clock Linux reads as UTC, so on a host at UTC+2 the sandbox
// ran two hours ahead: every mtime it wrote was wrong, and on a host west of
// UTC apt and TLS would have refused what they fetched as not yet valid.
func TestARealMachineKeepsTheHostsTime(t *testing.T) {
	eachRealMachine(t, nil, policy.Protections{}, func(t *testing.T, vm *VM, workdir string) {
		out := strings.TrimSpace(string(run(t, vm, "date -u +%s").Stdout))
		guest, err := strconv.ParseInt(out, 10, 64)
		if err != nil {
			t.Fatalf("date -u +%%s in the machine = %q", out)
		}
		if skew := guest - time.Now().Unix(); skew < -5 || skew > 5 {
			t.Fatalf("the machine's clock is %ds off the host's", skew)
		}
	})
}
