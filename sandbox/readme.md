# The sandbox

The sandbox executes tool calls in a Linux virtual machine.
[`sandbox.go`](sandbox.go) defines the client interface, [`vm.go`](vm.go)
implements the VM sandbox, and [`fake.go`](fake.go) provides an in-memory test
double.

Windows, macOS, and Linux use their platform hypervisors. If virtualization is
unavailable, sandbox startup fails; there is no automatic fallback to weaker
isolation.

Read [`readme.md`](../readme.md) for what john is, and
[`protocol/readme.md`](../protocol/readme.md) for the client/backend wire. This
is the other link: between the client on the developer's machine and the agent
inside the sandbox.

## Package layout

Files are grouped by role within one package. Code above the hypervisor is
shared across platforms; platform-specific files use build tags.

| | |
|---|---|
| `sandbox.go` `vm.go` | the interface the client programs against, and the sandbox |
| `fake.go` | a test double for the client; not a sandbox and not offered as one |
| `machine_*.go` | the hypervisors — one per platform, plus two test harnesses |
| `guest_*.go` `guest.go` | the agent, and the union it serves; only ever runs inside the Linux sandbox |
| `link_*.go` | the wire between the two, and the transports it runs over |
| `environment*.go` | the project's `.john/Dockerfile`: parsed, pulled and built |
| `hostfs.go` `hostarch_*.go` `image.go` `state.go` `path.go` | the host's side: the tree it serves, the image it boots, the disk it keeps |

Two names are worth keeping apart: [`image.go`](image.go) is the **guest
image** — the kernel and initramfs the machine boots — and
[`environment.go`](environment.go) is the **environment**, the project's
`.john/Dockerfile`.

## Sandbox guarantees

Every implementation is tested against two guarantees:

- **Only `Apply` writes to the host working directory.** The host side of the
  mount exposes no write operations: [`hostfs.go`](hostfs.go) has no create,
  unlink, or chmod operation.
- **Paths are workspace-relative and cannot escape.** Rejected in `CleanPath`
  before any implementation sees them.

## The workspace

```
   the developer's machine                    inside the sandbox
   ───────────────────────                    ──────────────────

   /Users/bob/project ──── read-only, live, filtered ────┐
                                                          ▼
                                                 /Users/bob/project
                                                   (the union)
                                                          ▲
   ~/.john/sandboxes/<key>/state.img ─── the sandbox's own writes ──┘
```

The working directory is mounted at the path it occupies on the host, because
compilers write absolute paths into debug information, build caches key on
them, and stack traces carry them. `GuestMount` in [`path.go`](path.go)
does the mapping; Windows, whose paths Linux cannot spell, becomes
`/c/Users/bob/project`.

The mount is a union, implemented in [`guest_union.go`](guest_union.go) rather
than handed to kernel overlayfs. overlayfs documents a lower directory that
changes while mounted as undefined behaviour, and a lower directory that
changes while mounted is exactly the requirement: a file the developer edits
during a session is what the next command inside the sandbox sees. Owning the
merge means the semantics are ours to define and ours to test, and it puts the
enumeration of changes — the diff the developer reviews — in the same place as
the rules about what may reach the host.

The upper layer keeps written files and deletion markers in two parallel trees:

```
<upper>/f/…   what the sandbox wrote, path for path
<upper>/w/…   what the sandbox deleted, path for path
```

overlayfs spells its whiteouts as `.wh.`-prefixed names beside the files they
hide, which needs an escaping scheme so a real file cannot be mistaken for a
marker. Two trees need none: no name a project can contain is ever read as a
deletion, so an agent cannot make a file vanish from the diff by creating one
with the right name. `TestADeletionCannotBeForgedByWritingAFile` is that
property.

`Diff` is then the upper layer, not a hash sweep of the whole project on both
sides. A file the sandbox never touched is not a copy that might have drifted —
it is the developer's file, being read where it lies.

### Symbolic links

Links cross. A checkout that uses them is an ordinary checkout — a pnpm
workspace, a `node_modules/.bin`, a monorepo linking one package into another —
and a sandbox that answered "no such file" for one made such a project look
broken in a way nothing in it explained. The sandbox can create them too, which
is most of what `npm install` does.

What does not happen is the host following one. A link is a path to anywhere,
so a link in the developer's tree is the way out of the workspace and the way
around an excluded path: the operating system would resolve `link/passwd`
happily, and `secrets → .env` is a second spelling of a file a policy keeps
out. So [`hostfs.go`](hostfs.go) serves a link as the link it is, target and
all, and refuses any path whose parents lead through one. Resolving is the
guest's job, and it does it in the two places a path arrives:

- **The mount.** The kernel walks the workspace itself, one component at a
  time, so a relative link stays inside the mounted workspace and an absolute
  one resolves in the sandbox's own filesystem — the sandbox's, not the
  developer's.
- **A tool call**, which arrives as a whole path with nothing to walk it.
  `union.follow` resolves it within the workspace, and the protections are then
  checked against what it resolved to rather than how it was spelled. A link
  that leaves the workspace is refused by name.

`Apply` keeps the same rule from the other side, and it is the one that matters
most because it is the only code that writes the developer's tree: a link that
stays inside the workspace is followed, one that leaves is held back and shown
to the developer as such, and a link at the path itself is replaced rather than
written through. `TestALinkIsNotAWayOutOfTheWorkspace` and
`TestApplyDoesNotWriteThroughALink` are those properties.

One thing a link is still not: applicable. `Diff` reports regular files, so a
link the sandbox created lives in the sandbox and is not offered to the host
tree — the same rule as for a device node, and for the same reason, that Apply
writes files and nothing else.

## Protections

The three modes come from the signed policy snapshot and are enforced by the
sandbox itself, on every path in and every path out, so no caller reaches a
protected path by taking a different route to it. Where each one is enforced
matters, and the honest split is:

| Mode | Enforced | Can a root shell inside get around it? |
|---|---|---|
| Excluded | Host side of the mount | **No.** The path is not served; there is no file to find |
| Read-only | The mount, then `Apply` | The mount, yes — by writing to the disk the upper layer is on. `Apply` refuses it anyway |
| No-delete | The mount, then `Apply` | Same |

`Apply` is the backstop, and it evaluates the policies against paths found by
enumerating the sandbox rather than against paths a caller supplied.
`TestAReadOnlyPathIsRefusedAtTheMountAndAgainAtApply` goes around the mount the
way root would and checks that the developer's file survives it.

Path matching is case-insensitive on every platform, because the sandbox is
Linux and the working directory may not be.

## The guest link

Two streams run between the client on the host and the agent inside the
sandbox, each carrying requests in one direction only. One bidirectional
multiplexer would do both, but the two directions have genuinely different
shapes — control calls are few and occasionally very long-lived, lower-layer
calls are many and short — and two one-way streams are much easier to reason
about and to test.

```
   client                                  guest agent
   ──────                                  ───────────
        ── control ──────────────────────►   read, write, delete,
                                             exec, changes, protect, mount
        ◄──────────────────────── lower ──   stat, readdir, read
```

Frames are a small JSON header and any number of raw payload blobs
([`link_rpc.go`](link_rpc.go)). File content never goes through JSON: base64 in
a header would cost a third of every byte read, and the lower layer serves
every read of every file in the project.

Errors cross as text, which loses their type — and one type must not be lost. A
`*policy.ProtectedError` is a decision, not a failure: the client turns it into
a blocked tool result naming the rule, so the model learns the protection
instead of retrying it. [`err.go`](err.go) carries it across intact, and
`TestTheAgentThatShipsRunsAsItsOwnProcess` checks that it survives a real
process boundary.

The agent is `john __guest` — the same binary the developer runs, handed to the
machine at boot. Nothing else would be right: the two ends speak a protocol to
each other with no handshake, so a guest image carrying an agent from a
different release would be a version skew with nothing to catch it.

## Machines

`hypervisor` and `machine` in [`machine.go`](machine.go) are the whole of what
is platform-specific. Everything above them — the union, the live lower layer,
the protections, the diff, the apply path, the control protocol — is the same
code under all of them, so a bug in the workspace is never a bug that
reproduces on one operating system only.

| | | |
|---|---|---|
| `krun` | Linux, KVM through libkrun | ships |
| `vz` | macOS, Virtualization.framework | ships |
| `hyperv` | Windows, Hyper-V | ships |
| `qemu` | anywhere, under emulation | test harness |
| `loopback` | the agent as a host process | test harness, **isolates nothing** |

Two of the three are the operating system's own API and are in the binary.
Linux is the exception: /dev/kvm gives out virtual CPUs and memory and expects
the device model to be the caller's, so the Linux machine uses
[libkrun](https://github.com/containers/libkrun) as its monitor and opens it at
run time. It is the one thing the sandbox needs that john does not carry, and a
machine without it runs every other part of john and says so when it is asked
for a sandbox.

The two harnesses are why the rest of this document is verifiable rather than
aspirational, and they cover different halves. The loopback machine runs the
agent as an ordinary process, so every layer above the hypervisor can be
developed and tested in milliseconds on any machine. The QEMU machine boots a
real Linux kernel under pure emulation, needing no hardware virtualization at
all — so the boundary itself can be tested on a build machine that is a virtual
machine with no nested virtualization, which is precisely where the shipping
machines cannot run.

Neither is ever chosen automatically. The loopback machine says so on the
terminal when it is selected; the QEMU machine has no defaults at all and has
to be handed every path, so it cannot become the sandbox somebody got by
accident.

All three are built, and each one costs the platform something different. What
that is, and what it buys, is the rest of this section.

```
make guest-image     the kernel and initramfs, for both architectures
make test-machine    the sandbox, inside every machine this host can start
```

`make test-machine` is the bar. It is not a smoke test: it runs the whole
package against every machine this host can start, including the contract
suite — a protected path is refused whatever route is taken to it, a change to
one is reported and then refused again at apply, `.SECURE` and `.secure` are
the same file, applying one path does not apply another — and the tests that
only mean something inside a machine, which are that the workspace is mounted
at the host's own path, a host edit mid-session is visible to the next command,
an excluded file is absent rather than refused, and the machine has no network
interface. Each machine's own test file says how to point it at an image.

### macOS, and what it costs

Virtualization.framework is Objective-C, so `vz` is cgo — the Objective-C is
all in [`machine_vz_darwin.m`](machine_vz_darwin.m), behind the seven functions
in [`machine_vz_darwin.h`](machine_vz_darwin.h), and
[`machine_vz.go`](machine_vz.go) is the same shape as the QEMU machine next
door. A macOS build made without cgo still compiles and still runs everything
else; it says it cannot start a sandbox rather than not building.

**macOS will not start a virtual machine for a binary that is not signed with
`com.apple.security.virtualization`.** [`machine_vz.plist`](machine_vz.plist)
is the entitlement, and ad-hoc signing is enough to run the sandbox on the
machine that built it:

```
codesign --entitlements sandbox/machine_vz.plist -f -s - ./john
```

`make clients` signs every macOS build, and `make test-machine` signs the test
binary too, because `go test` cannot sign the one it builds. Both mean the
client builds have to run on a Mac — one Mac builds all five platforms, and
nothing else builds a macOS client that can start a sandbox. See
`CLIENT_PLATFORMS` in the Makefile; a build going to other people's machines
wants a real identity in `CODESIGN_ID` and then notarizing.

The guest link is vsock, in the direction this design needs: the guest dials
the host, so the host installs two `VZVirtioSocketListener`s rather than
calling `connectToPort:`.

### Windows, and what it costs

Hyper-V is reached through the **Host Compute System API** in `vmcompute.dll`
rather than through WMI. That is the decision the Windows port turns on and it
buys two things. It needs only the **Virtual Machine Platform** feature rather
than the full Hyper-V role, so it works on the machines developers actually
have — including Windows Home, and including every machine that has ever run
WSL 2. And it is a flat C API of eight functions rather than COM, so
[`hcs_windows.go`](hcs_windows.go) reaches it through `x/sys/windows` and **the
Windows client stays pure Go**, which is what keeps `make clients` able to
cross-compile it from the Mac that has to build the macOS one.

The machine is a JSON document — [`machine_hyperv.go`](machine_hyperv.go) —
and it is deliberately small: two disks, a socket device and a serial port.
There is no network adapter, no video, no keyboard and no guest integration
service, because everything else it might have had is a way in or a way out
that would then need policing.

`Chipset.LinuxKernelDirect` boots the kernel and initramfs directly, the same
two files every other machine boots, with `john.cfg=` on the same command line.
The Hyper-V *manager* cannot do this — a Generation 2 virtual machine is UEFI
and takes a bootable disk — which is why this port was expected to need the
guest image rebuilt as an EFI application. It did not.

**Creating a virtual machine needs an elevated token**, or membership of the
Hyper-V Administrators group, which is the same requirement without the prompt
every time. Nothing else in the port is privileged.

john offers to arrange it, once, rather than telling the developer to. On a
machine that cannot start a sandbox for that reason and no other, the first run
asks — before a weaker sandbox is settled for, and on the plain terminal before
any UI is drawn over it. Yes runs `john __sandbox-admin` elevated, which adds
this user to Hyper-V Administrators and exits; that is the whole of what john
ever does with administrator rights, and john itself never runs with them —
an agent that executes code a model sent it is the last process that should.
The privilege arrives with the next Windows sign-in, and the offer says so.
[`admin.go`](admin.go) is the shape of it and
[`admin_windows.go`](admin_windows.go) is the only implementation; the answer
is remembered, so it is asked once, and `john -sandbox vm` asks again.

Two things differ inside the machine and both are in the image rather than
here. Hyper-V has no virtio, so disks arrive over VMBus as a SCSI bus and
`guest/init` takes `/dev/sda` where another machine gives it `/dev/vda`. And
the guest link is **hv_sock**, which Linux registers as a vsock transport — so
the agent dials `vsock:2201` under every hypervisor and needed no Windows code
at all. On the host it is `AF_HYPERV`, where vsock port *N* is the service GUID
`N-facb-11e6-bd58-64006a7986d3`; [`hvsock_windows.go`](hvsock_windows.go) is
the whole of that, including the `net.Conn` for a socket family Go has never
heard of.

The one thing Hyper-V will not do is attach a raw disk image, so
[`vhd_windows.go`](vhd_windows.go) makes dynamic VHDXs through `virtdisk.dll`
beside the paths the rest of the package names. Dynamic is the VHDX word for
sparse: the disk still costs what has been written to it, so the size limit is
still a limit rather than an allocation.

### Linux, and what it costs

KVM is not the same kind of interface as the other two. Virtualization.framework
and Hyper-V each hand you a whole machine with a device model already attached;
`/dev/kvm` hands you virtual CPUs and memory and expects the device model —
virtio-blk, vsock, the interrupt controller, the boot protocol — to be yours.
Writing one is a different project, so `krun` uses
[libkrun](https://github.com/containers/libkrun), a virtual machine monitor in
library form, and [`machine_krun.go`](machine_krun.go) is the same shape as the
other two.

**libkrun is opened at run time with dlopen, not linked**, and
[`libkrun_linux.go`](libkrun_linux.go) is that binding. Linking it would need
cgo, and a cgo Linux client cannot be cross-compiled from the Mac that builds
every other platform; it would also mean a john that does not start *at all* on
a machine without the library, so `john login` and `john backend` would fail
where no sandbox was ever wanted. Opened at run time neither is true. The cost
is that a Linux build is dynamically linked where it used to be static — it
needs a loader to exist, though it asks for no versioned glibc symbol, so there
is no version floor.

**The machine runs in a process of its own.** `krun_start_enter` does not
return: the library takes the process over and exits it when the machine stops,
so a john that called it would be a john that had turned into a virtual
machine. `john __machine` is that second process — the same binary, the way
`john __guest` is the same binary inside the sandbox.

The guest link runs the opposite way to the other two. libkrun maps a guest
vsock port to a Unix socket *path* on the host and dials it, so this machine
listens and libkrun brings the connections to it. The agent is unchanged and
still dials `vsock:2201`, because which end listens is a property of the
machine and not of the agent.

The x86-64 guest kernel is a bzImage, which libkrun has no loader for — but its
compressed-image format does exactly the right thing, so `kernelFormat` reads
which shape the image is out of the file and checks the one assumption that
format makes. The guest image was not touched for this platform either.

Three things in libkrun are worked around here rather than lived with, and each
is commented where it is handled: it must be built with `BLK=1` or it has no
block device at all; it shrinks a raw disk image when a guest discards the tail
of it, which left every sandbox's first boot with a disk too small for the
filesystem just made on it; and its vsock proxy discards pending data when the
host end hangs up, which truncated about a third of all proxied responses.

**What is not proven is aarch64.** The machine picks `ttyAMA0` and a raw kernel
image there, both right on paper and neither ever run, because the machines
virtualize rather than emulate and this was written on an x86-64 host. It wants
an hour on arm64 Linux hardware before anyone relies on it.

## The guest

The image is a kernel and an initramfs, built by
[`guest/mkimage.sh`](guest/mkimage.sh) with no root and nothing installed on
the machine doing the building — a build that needs root is a build that
cannot run in CI. Alpine supplies the kernel and busybox, because a sandbox
wants the smallest Linux that can still run a project's build.

**It travels inside the john binary.** `make clients` builds two images —
arm64 and amd64 — and embeds the matching one in each of the five client
builds, so a first run needs nothing but the binary: no download, no cache to
warm, no half-fetched image to recover from, and `john apply` works on a
machine that has never reached a backend. It costs nothing in consistency
either, because the version handshake already refuses a client whose build does
not match the backend's — there is no state in which a machine could usefully
hold an image from a different release than its client.

Two images and not five, because the machines virtualize rather than emulate:
the guest's architecture is the host **CPU**'s, not the host operating
system's. `darwin/arm64` and `linux/arm64` carry the arm64 image;
`darwin/amd64`, `windows/amd64` and `linux/amd64` carry the amd64 one.

The one case where that goes wrong is Rosetta — an x86-64 build of john on
Apple Silicon reports `amd64` for itself and would ask the hypervisor to boot
an x86 guest on an ARM processor. [`hostarch_darwin.go`](hostarch_darwin.go)
asks the kernel whether the process is translated, so the developer gets a
sentence telling them to install the right build rather than a boot failure
with no explanation.

The agent inside the image is built with the `john_guest` tag: the same source
and the same release as the john a developer runs, with the backend, the web
console, the terminal UI and SQLite left unreachable so the linker drops them.
33 MB becomes 4, the initramfs 17 MB becomes 2.6, and the kernel reaches
`/init` in 0.69s instead of 1.00s — the initramfs is unpacked into the
sandbox's memory on every start.

Two things in there are worth knowing. Alpine ships the arm64 kernel as an EFI
"zboot" image — a PE wrapper around a compressed Linux image — which needs
firmware to boot, so the script unwraps it into the raw image a hypervisor can
load directly. (The x86-64 kernel is an ordinary bzImage and is passed through,
which is why the script checks the format rather than the architecture.) And
`/init` is [`guest/init`](guest/init), which brings up just
enough of a system for the agent to be the rest of it: the kernel filesystems,
a shell on the path so `run` has something to run commands with, FUSE, the
transports, an entropy source, and the sandbox's own disk. Then it execs
`john __guest` and the agent is the machine.

Everything the sandbox writes and has not applied lives on that disk, mounted
at `/john`, and the disk is what makes `disk` in `john resources` a real limit
rather than a number — the thing a container sharing the host's filesystem
could never enforce. It arrives blank, so a sandbox's first boot makes an ext4
filesystem on it and every later boot finds one. `mke2fs` is in the initramfs
for that, taken from Alpine's own packages: the build machine is usually a Mac
and has no `mke2fs` for Linux, and Alpine builds busybox without any of the ext
mkfs applets.

Three of those are modules in Alpine's kernel rather than built in, which is
worth knowing because each fails silently. Without the vsock modules the guest
boots perfectly and cannot reach the host. Without `virtio_blk` and `ext4` it
boots perfectly and keeps `/john` in memory, so the limit is not a limit.
Without `virtio-rng` it boots perfectly and then blocks for four seconds the
first time anything wants randomness, which is `mke2fs` asking for a UUID.

The agent reads where to reach the host from the kernel command line, because
at the moment a machine boots there is nowhere else it could have been told:
no filesystem the host has written, no network, and no link yet — the
configuration is what says how to open the link.

## One workspace, one sandbox

A sandbox is named for the directory it backs — `~/.john/sandboxes/
billing-api-9f8e7d6c1a2b` — because a directory full of hashes is one nobody
can audit, reclaim or reason about, and the first question anyone asks of it is
which of these is my project.

The hash beside the name is of the path as the filesystem resolves it, not as
it was typed, and that matters more than the legibility does. On macOS `/tmp`
is a symbolic link to `/private/tmp`, and the default filesystem is
case-insensitive — so the same project reached as `/tmp/x` and
`/private/tmp/x`, or as `~/Projects/John` and `~/Projects/john`, would
otherwise get **two** sandboxes. Two sandboxes for one project means two
environments and two sets of unapplied work, and `john apply` run from one of
them cannot see what the other is holding.
[`workspace.go`](workspace.go) has the rules.

### One workspace, several sandboxes

A workspace has one directory and any number of sandboxes in it, split by what
they are:

```
~/.john/sandboxes/billing-api-9f8e7d6c/
    ca/  protections.json  resources.json      the project's
    env-<envid>.img                            the environment, shared
    instances/1/state.img                      one line of work
    instances/2/state.img                      another
```

The project's half is shared because it is the project's: the limits, the
protections, the certificate authority, and the environment — which is the
expensive part, a base image plus everything the Dockerfile installs, and
identical for everyone. An instance's half is its own because it is one line of
work: its disk, and so its edits and its unapplied changes.

Two agents on one repository doing different things get two of them, and
neither sees the other's work. What they share is the environment, built once:
the second sandbox to start fetches nothing.

Most of the time none of this is visible. One sandbox is found, used, and
carried on with — including after the terminal has been closed and reopened,
because an instance outlives the process that made it. Two terminals at once
get two, because two lines of work must not share an upper layer. Only coming
back to more than one asks, because it is the only case where choosing wrong
strands somebody's work:

```
you have 2 sandboxes for this workspace. Which one do you want to use?
  1: last used 4 minutes ago ("help design the payments API") · 3 unapplied
  2: last used 2 days ago ("refactor the core interfaces so the ledger is n…")
Choose one with -sandbox-instance <id>.
```

A sandbox is claimed with an operating-system lock on an open file for as long
as it runs, so two processes never open one disk image — and so a crash
releases it rather than stranding it.

`john sandbox` is how a developer sees and reclaims all of this: what each
project holds, what each sandbox was last doing, and what is still unapplied in
it. `john sandbox prune` reclaims the dormant ones and never touches one that
is running or one holding work that was never applied. `john sandbox rebuild`
— and `/rebuild` in a session — builds the environment again while keeping
that work, which is the difference between it and `/reset`.

The environment is built in a machine of its own, started for it and stopped
after it. It has to be: a disk image cannot be attached to one machine
read-write and another read-only at the same time, and a session that held it
writable would be one no other sandbox in the workspace could start alongside.
So the build runs and finishes, and every session then attaches the result
read-only and lays a writable layer of its own over it — a session that
installs something gets to install it, and what it installs is nobody else's.

## Sandbox environment

The sandbox environment is built from `.john/Dockerfile`. This lets projects
define development tools separately from their deployment image, which may
omit a shell, compiler, or test runner. The Dockerfile format works with
existing editors and linters; the supported instructions are listed below.
[`environment.go`](environment.go) resolves the definition and its identity.

The build context is limited to `.john/`, so builds can copy supporting files
from that directory without depending on the rest of the workspace. The
context is hashed on every start to identify the environment. A changed
definition rebuilds the environment while preserving the workspace and
unapplied changes.

### How it is built, without a container runtime

The VM sandbox builds the Dockerfile itself, and the work is split across the
boundary rather than done on one side of it:

- **The host** parses the definition, resolves the base image and pulls its
  layers — [`environment_dockerfile.go`](environment_dockerfile.go),
  [`environment_registry.go`](environment_registry.go) and
  [`environment_build.go`](environment_build.go). That is where the proxy is,
  so the pull obeys the `policy.Build` setting without a second route; and it
  is plain Go over HTTP, so it is tested against a registry served in-process
  rather than only inside a booted kernel.
- **The guest** unpacks the layers with their ownership and their whiteouts,
  and runs the build's commands inside the result —
  [`guest_env.go`](guest_env.go). That is the part which has to be Linux.

It reads the **host** copy of `.john/Dockerfile`, never the sandbox's. That is
what makes an agent's edit to its own environment safe: the agent can change
the file, the change arrives as a diff like any other, and it takes effect when
the developer applies it. An agent cannot widen its own build's reach by
writing to a file it controls.

Six instructions are supported — `FROM`, `RUN`, `COPY`, `ENV`, `WORKDIR` and
`ARG` — which is what a file describing a toolchain uses. Everything else is
refused **by name**: a silently ignored `USER` means the build ran as root
when the file said otherwise, and a silently ignored second `FROM` means the
environment is a different image than the one the developer wrote down.

### A private base image

An organization's base image is usually in an organization's registry, so the
pull can need a sign-in. john reads the credential the developer already has
rather than asking for a second copy of it: `~/.docker/config.json` first, and
— importantly — the credential **helpers** it names, because ECR, Artifact
Registry and ACR store no password at all and mint a token that lives minutes.
A reader that parsed the config and never ran a helper would work on a demo and
fail at every customer. For a machine with no docker on it,
`john registry login <registry>` stores one in `~/.john/registries.json`, and
proves it against the registry before keeping it.

Both registry challenges are answered. `Bearer` means a token endpoint, where
the credential is spent (an identity token is exchanged as a refresh grant, a
password goes as Basic on the token request); `Basic` means there is no
endpoint and the credential goes on each request, which is what Harbor,
Artifactory and most on-premises registries do.

**The credential never enters the sandbox.** The pull happens on the host
because that is where the proxy is, so the password is read there, spent on an
`Authorization` header there, and what crosses into the machine is the image's
layers. The machine is told three things about its environment — the route out,
the variables commands run with, and the proxy's certificate authority — and
`TestTheMachineIsToldTheRouteOutAndNothingAboutTheCredential` is what keeps a
fourth from appearing.

The result is a root filesystem on the sandbox's own disk, and a command runs
inside it with `chroot`. The workspace is mounted underneath that root at the
host's own path, so `pwd` is the same whatever is installed — what changes is
which `/usr` is under it.

### The route out

The sandbox has no network interface, and the environment build is the one
thing that has a reason to want one. The machine carries a third stream to the
john proxy on the host — a socket device, not a network — and the agent
listens on loopback inside and forwards to it, because `apt-get`, `curl` and
`go mod download` know how to use a proxy at a host and a port and nothing
else. A program that ignores `http_proxy` reaches nothing at all; one that
honours it reaches the proxy and no further.

Where a build's traffic goes is [`policy/build.go`](../policy/build.go), and
the default is `direct`: a build is how a project gets its compilers, and it is
what makes the sandbox useful afterwards under a policy that allows no egress
at all. Under `direct` the base image is pulled straight from the registry, and
the machine that builds the environment — only that one — is wired to a proxy
of its own that is a plain pass-through: HTTPS is tunnelled to the origin
without being decrypted, and plain HTTP is forwarded as it comes, so its `RUN`
steps reach package indexes no policy covers. It is a proxy at all only
because the machine has no network interface to be given instead. Every session's machine is wired to the policy-enforcing proxy
as always. `proxied` puts the whole build, pull and `RUN` steps, under the same
network policies as everything else.

## Tests

The parts of the interface that are controls rather than conveniences live in
[`contract_test.go`](contract_test.go) and run against every implementation
this machine can start — the fake, the virtual machine over the loopback link,
and the virtual machine inside a booted kernel when a guest image has been
built. A protection that held only where one of them is installed would be no
protection at all, and a boundary is only really a boundary in the last of
them.
