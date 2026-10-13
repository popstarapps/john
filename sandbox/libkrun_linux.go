//go:build (amd64 || arm64) && !john_guest

package sandbox

import (
	"errors"
	"fmt"
	"sync"
	"syscall"

	"github.com/ebitengine/purego"
)

// libkrun, as john calls it.
//
// The library is a virtual machine monitor in library form: it provides the
// microVM, virtio-blk and vsock, and leaves the lifecycle to the caller. What
// john uses of it is small and is all here — configure a machine, boot it,
// hand the guest a socket device — so that machine_krun.go next door reads
// like the other two machines rather than like an FFI binding.
//
// It is opened at run time with dlopen rather than linked at build time, and
// that is the decision in this file worth writing down. Linking it would cost
// two things john would not get back.
//
// The client would need cgo. A cgo Linux client cannot be cross-compiled from
// the Mac that builds every other platform, and one build machine is a
// property the Makefile is arranged around — see the comment above
// CLIENT_PLATFORMS, where macOS needing cgo is the whole reason the build
// machine has to be a Mac and Linux and Windows not needing it is the reason a
// Mac is enough.
//
// And a john linked against a library that is not installed does not start at
// all. `john login`, `john apply` and `john backend` would fail on a machine
// that was never going to run a sandbox, with a loader error naming a library
// nobody asked for.
//
// Opened at run time neither is true: the client is still pure Go and still
// cross-compiles flag-only, and a machine without libkrun runs every other
// part of john and says exactly what is missing when it is asked for a
// sandbox. What it costs is that the Linux binary is dynamically linked
// against libc for dlopen where it used to be static. It asks for no versioned
// symbol, so there is no glibc floor to break on an older server; it does need
// a dynamic loader to exist, which an image built FROM scratch does not have.

// The soname john binds, which carries libkrun's ABI: 1, which every release
// is, and whose calls below are all there from 1.17 on. Naming it rather than
// opening libkrun.so asks for the ABI john has been run against. libkrun's
// main branch is 2.0, and in September 2026 it dropped this C API for
// generated bindings, so a 2.0 library is not one john can use yet.
const libkrunSoname = "libkrun.so.1"

// How libkrun should read the kernel it is handed. The guest image is not
// built for libkrun — every machine boots the same two files — so the format
// is worked out from the image rather than assumed; see kernelFormat.
const (
	krunKernelRaw       = 0 // a bootable image, loaded as it is
	krunKernelELF       = 1
	krunKernelImageBz2  = 3 // an ELF vmlinux, bzip2-compressed, inside a wrapper
	krunKernelImageGz   = 4 // ... gzip, which is what an x86 bzImage is
	krunKernelImageZstd = 5
)

// The sandbox's disks are sparse raw images on every machine but Hyper-V; see
// disk_other.go.
const krunDiskRaw = 0

// Log levels, as libkrun numbers them.
const (
	krunLogWarn = 2
	// Escape sequences in a file nobody is watching are noise, and the
	// console log is read after the fact.
	krunLogStyleNever = 2
)

// libkrunLib is the library, once it has been found.
type libkrunLib struct {
	initLog      func(fd int32, level, style, options uint32) int32
	createCtx    func() int32
	setVMConfig  func(ctx uint32, vcpus uint8, ramMiB uint32) int32
	setKernel    func(ctx uint32, kernel string, format uint32, initramfs, cmdline string) int32
	addDisk      func(ctx uint32, id, path string, format uint32, readOnly bool) int32
	addVsock     func(ctx uint32, tsiFeatures uint32) int32
	addVsockPort func(ctx uint32, port uint32, path string, listen bool) int32
	addSerial    func(ctx uint32, in, out int32) int32
	startEnter   func(ctx uint32) int32

	// A context starts with a vsock device of its own — with TSI on, which
	// forwards the guest's sockets to the host's network — and a console.
	// john adds its own vsock with TSI off, and libkrun refuses a second one,
	// so these are called before anything is added.
	disableVsock   func(ctx uint32) int32
	disableConsole func(ctx uint32) int32
}

var (
	libkrunOnce sync.Once
	libkrunLoad *libkrunLib
	libkrunErr  error
)

// errNoLibkrun is what a machine with no library says, and it says how to get
// one: this is the only part of the Linux sandbox that is not in the binary
// the developer already has.
var errNoLibkrun = errors.New("sandbox: libkrun is not installed. " +
	"The Linux sandbox is a KVM virtual machine and libkrun is the monitor it runs under. " +
	"Install libkrun 1.17 or later — on Fedora `dnf install libkrun`; elsewhere build a " +
	"release of https://github.com/containers/libkrun with BLK=1, which is what gives the " +
	"sandbox its disk — and make sure " + libkrunSoname + " is somewhere the loader looks " +
	"(ldconfig -p | grep libkrun)")

// loadLibkrun opens the library and binds what john calls. It is done once and
// the answer kept, because Available asks it on every Build and the answer
// does not change while john is running.
func loadLibkrun() (*libkrunLib, error) {
	libkrunOnce.Do(func() {
		handle, err := purego.Dlopen(libkrunSoname, purego.RTLD_NOW|purego.RTLD_GLOBAL)
		if err != nil || handle == 0 {
			libkrunErr = fmt.Errorf("%w (%v)", errNoLibkrun, err)
			return
		}
		l := &libkrunLib{}
		// Every symbol is looked up before any of them is used, so a library
		// that is missing one says so here rather than at the call. The one
		// this catches in practice is krun_add_disk2: libkrun compiles its
		// block device out unless it was built with BLK=1, and a sandbox
		// whose disk is missing is a sandbox with no size limit and no
		// persistence — which would otherwise look like a machine that booted
		// perfectly.
		for _, sym := range []struct {
			fn   any
			name string
		}{
			{&l.initLog, "krun_init_log"},
			{&l.createCtx, "krun_create_ctx"},
			{&l.disableVsock, "krun_disable_implicit_vsock"},
			{&l.disableConsole, "krun_disable_implicit_console"},
			{&l.setVMConfig, "krun_set_vm_config"},
			{&l.setKernel, "krun_set_kernel"},
			{&l.addDisk, "krun_add_disk2"},
			{&l.addVsock, "krun_add_vsock"},
			{&l.addVsockPort, "krun_add_vsock_port2"},
			{&l.addSerial, "krun_add_serial_console_default"},
			{&l.startEnter, "krun_start_enter"},
		} {
			addr, err := purego.Dlsym(handle, sym.name)
			if err != nil || addr == 0 {
				libkrunErr = fmt.Errorf("sandbox: this %s has no %s — "+
					"it is older than john expects (1.17), or was built without the block "+
					"device support the sandbox needs (BLK=1)", libkrunSoname, sym.name)
				return
			}
			purego.RegisterFunc(sym.fn, addr)
		}
		libkrunLoad = l
	})
	return libkrunLoad, libkrunErr
}

// krunError turns one of libkrun's negative return values into something worth
// reading. They are negated errno, which is the whole of what the library
// says about a failure.
func krunError(what string, code int32) error {
	if code >= 0 {
		return nil
	}
	return fmt.Errorf("sandbox: libkrun: %s: %w", what, syscall.Errno(-code))
}
