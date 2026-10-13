// The C surface of the Virtualization.framework machine.
//
// Everything Objective-C about this port lives behind these six functions.
// Go never holds a framework object, never touches the virtual machine's
// dispatch queue, and never has to know that a listener is a delegate — which
// is what keeps machine_vz.go readable next to machine_qemu.go.

#ifndef JOHN_VZ_DARWIN_H
#define JOHN_VZ_DARWIN_H

#include <stddef.h>

// vz_boot's arguments, gathered into one struct because cgo calls with a
// dozen parameters are where the mistakes live.
typedef struct {
	const char *kernel;      // bootable Linux image, uncompressed
	const char *initramfs;   // the guest: an init, a shell and the agent
	const char *cmdline;     // console=, panic=, john.cfg=
	const char *disk;        // the state disk, or NULL for none
	const char *envdisk;     // the workspace's environment disk, or NULL
	int envwritable;         // 1 when this machine is the one building it
	const char *console;     // where the kernel log goes, or NULL
	unsigned int cpus;
	unsigned long long memory; // bytes
	unsigned int ports[4];     // the vsock ports the guest dials back on
	unsigned int nports;
} vz_config;

// vz_supported reports whether this Mac and this process can run a virtual
// machine at all, and says which of the reasons it is when it cannot.
//
//	0  yes
//	1  the OS is too old
//	2  the hardware or the VM host does not support virtualization
//	3  the binary is not signed with com.apple.security.virtualization
int vz_supported(char *err, size_t errlen);

// vz_boot starts a machine and returns an opaque handle, or NULL with err
// filled in. It returns once the machine is running, which is before the
// guest has dialed back.
void *vz_boot(const vz_config *cfg, char *err, size_t errlen);

// vz_accept waits for the guest to open a connection to one of the ports and
// returns a file descriptor for it. It returns -1 on error and -2 when the
// timeout passed with nothing connecting, so a caller can poll it and still
// notice its context being cancelled.
int vz_accept(void *handle, unsigned int port, int timeout_ms, char *err, size_t errlen);

// vz_forget lets go of the connection behind a descriptor vz_accept returned.
// Call it before closing that descriptor, never after: once it is closed its
// number can come back for the next connection.
void vz_forget(void *handle, int fd);

// vz_wait_stopped waits up to timeout_ms for the machine to stop on its own,
// and returns 1 if it did.
int vz_wait_stopped(void *handle, int timeout_ms);

// vz_stop powers the machine off. It is safe to call more than once.
void vz_stop(void *handle);

// vz_release drops the handle. The machine must be stopped first.
void vz_release(void *handle);

#endif
