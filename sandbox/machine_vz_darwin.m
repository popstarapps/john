// macOS: the Objective-C half of the Virtualization.framework machine.
//
// Two rules shape everything here. Every message to a VZVirtualMachine has to
// be sent on the serial dispatch queue it was created with, so nothing below
// touches the machine outside a block on that queue — get this wrong and the
// crashes look random and arrive days later. And our design has the guest
// dialing the host, so the socket device is used as a listener rather than
// with connectToPort:, which is the direction almost every example shows.

#import <Foundation/Foundation.h>
#import <Virtualization/Virtualization.h>
#import <Security/Security.h>
#include <unistd.h>
#include <stdio.h>
#include "machine_vz_darwin.h"

static void copyError(char *err, size_t errlen, NSError *e, NSString *what) {
	if (!err || errlen == 0) {
		return;
	}
	NSString *msg = e ? [NSString stringWithFormat:@"%@: %@", what, e.localizedDescription] : what;
	snprintf(err, errlen, "%s", msg.UTF8String);
}

@class JohnVZ;

// One listener per port. The alternative is a single delegate reading
// destinationPort off the connection, which works but puts the mapping in the
// place where a mistake is silent; a listener that knows its own port cannot
// hand a stream to the wrong reader.
@interface JohnVZPort : NSObject <VZVirtioSocketListenerDelegate>
@property (nonatomic, assign) unsigned int port;
@property (nonatomic, weak) JohnVZ *owner;
@end

@interface JohnVZ : NSObject <VZVirtualMachineDelegate>
@property (nonatomic, strong) VZVirtualMachine *vm;
@property (nonatomic, strong) dispatch_queue_t queue;
@property (nonatomic, strong) NSCondition *cond;
// port -> the file descriptors the guest has opened and nobody has taken yet.
@property (nonatomic, strong) NSMutableDictionary<NSNumber *, NSMutableArray<NSNumber *> *> *waiting;
// The connections, by the duplicate Go was given. Each is held until Go
// closes that duplicate and says so with vz_forget: a released
// VZVirtioSocketConnection closes the descriptor underneath it, and one held
// for the life of the machine keeps the stream open at both ends. The proxy
// accepts a connection per request, so holding them all is a leak that
// grows with every request, and the guest's side of each never ends.
@property (nonatomic, strong) NSMutableDictionary<NSNumber *, VZVirtioSocketConnection *> *connections;
@property (nonatomic, strong) NSMutableArray *listeners;
@property (nonatomic, strong) NSFileHandle *console;
@property (nonatomic, assign) BOOL stopped;
- (void)deliver:(int)fd toPort:(unsigned int)port;
@end

@implementation JohnVZPort
- (BOOL)listener:(VZVirtioSocketListener *)listener
	shouldAcceptNewConnection:(VZVirtioSocketConnection *)connection
	fromSocketDevice:(VZVirtioSocketDevice *)device {
	// The descriptor is duplicated because the connection owns the original
	// and closes it when it goes; the duplicate is what Go reads and writes,
	// and it outlives whatever the framework decides to do with its own.
	int fd = dup(connection.fileDescriptor);
	if (fd < 0) {
		return NO;
	}
	JohnVZ *owner = self.owner;
	if (!owner) {
		close(fd);
		return NO;
	}
	[owner.cond lock];
	owner.connections[@(fd)] = connection;
	[owner.cond unlock];
	[owner deliver:fd toPort:self.port];
	return YES;
}
@end

@implementation JohnVZ

- (void)deliver:(int)fd toPort:(unsigned int)port {
	[self.cond lock];
	NSNumber *key = @(port);
	NSMutableArray *q = self.waiting[key];
	if (!q) {
		q = [NSMutableArray array];
		self.waiting[key] = q;
	}
	[q addObject:@(fd)];
	[self.cond broadcast];
	[self.cond unlock];
}

// A machine that stops is a machine nothing will ever connect from, so an
// accept that is still waiting is woken rather than left to its timeout.
- (void)markStopped {
	[self.cond lock];
	self.stopped = YES;
	[self.cond broadcast];
	[self.cond unlock];
}

- (void)guestDidStopVirtualMachine:(VZVirtualMachine *)virtualMachine {
	[self markStopped];
}

- (void)virtualMachine:(VZVirtualMachine *)virtualMachine didStopWithError:(NSError *)error {
	[self markStopped];
}

- (void)virtualMachine:(VZVirtualMachine *)virtualMachine
	networkDevice:(VZNetworkDevice *)networkDevice
	attachmentWasDisconnectedWithError:(NSError *)error {
	// The sandbox has no network device. This exists so the protocol is
	// satisfied and never fires.
}

@end

// hasEntitlement asks the kernel what this process was signed with, rather
// than starting a machine and reading the failure. The difference matters
// because the failure is "Internal Virtualization error", which tells a
// developer nothing about what to do next.
static BOOL hasEntitlement(void) {
	SecTaskRef task = SecTaskCreateFromSelf(NULL);
	if (!task) {
		return NO;
	}
	CFTypeRef value = SecTaskCopyValueForEntitlement(task, CFSTR("com.apple.security.virtualization"), NULL);
	CFRelease(task);
	if (!value) {
		return NO;
	}
	BOOL ok = CFGetTypeID(value) == CFBooleanGetTypeID() && CFBooleanGetValue((CFBooleanRef)value);
	CFRelease(value);
	return ok;
}

int vz_supported(char *err, size_t errlen) {
	if (@available(macOS 12.0, *)) {
		if (!VZVirtualMachine.isSupported) {
			copyError(err, errlen, nil, @"this Mac cannot run virtual machines "
				@"(no hardware virtualization, or it is switched off for this VM)");
			return 2;
		}
		if (!hasEntitlement()) {
			copyError(err, errlen, nil, @"this build of john is not signed with the "
				@"com.apple.security.virtualization entitlement, which macOS requires "
				@"before it will start a virtual machine");
			return 3;
		}
		return 0;
	}
	copyError(err, errlen, nil, @"the sandbox needs macOS 12 or later");
	return 1;
}

void *vz_boot(const vz_config *cfg, char *err, size_t errlen) {
	if (@available(macOS 12.0, *)) {
		@autoreleasepool {
			JohnVZ *m = [[JohnVZ alloc] init];
			m.queue = dispatch_queue_create("com.popstarapps.john.vz", DISPATCH_QUEUE_SERIAL);
			m.cond = [[NSCondition alloc] init];
			m.waiting = [NSMutableDictionary dictionary];
			m.connections = [NSMutableDictionary dictionary];
			m.listeners = [NSMutableArray array];

			VZVirtualMachineConfiguration *conf = [[VZVirtualMachineConfiguration alloc] init];

			// Direct kernel boot: no firmware, no bootloader, no disk to
			// install onto. The image is the same kernel and initramfs every
			// other machine boots.
			NSURL *kernelURL = [NSURL fileURLWithPath:@(cfg->kernel)];
			VZLinuxBootLoader *boot = [[VZLinuxBootLoader alloc] initWithKernelURL:kernelURL];
			boot.initialRamdiskURL = [NSURL fileURLWithPath:@(cfg->initramfs)];
			boot.commandLine = @(cfg->cmdline);
			conf.bootLoader = boot;

			// The framework has opinions about both of these, and a machine
			// asked for more than the host allows fails validation rather
			// than being clamped, so clamp.
			unsigned long long mem = cfg->memory;
			if (mem < VZVirtualMachineConfiguration.minimumAllowedMemorySize) {
				mem = VZVirtualMachineConfiguration.minimumAllowedMemorySize;
			}
			if (mem > VZVirtualMachineConfiguration.maximumAllowedMemorySize) {
				mem = VZVirtualMachineConfiguration.maximumAllowedMemorySize;
			}
			conf.memorySize = mem;

			NSUInteger cpus = cfg->cpus;
			if (cpus < VZVirtualMachineConfiguration.minimumAllowedCPUCount) {
				cpus = VZVirtualMachineConfiguration.minimumAllowedCPUCount;
			}
			if (cpus > VZVirtualMachineConfiguration.maximumAllowedCPUCount) {
				cpus = VZVirtualMachineConfiguration.maximumAllowedCPUCount;
			}
			conf.CPUCount = cpus;

			// The guest link. Two listeners on two ports, because the guest
			// dials the host.
			VZVirtioSocketDeviceConfiguration *socketDev = [[VZVirtioSocketDeviceConfiguration alloc] init];
			conf.socketDevices = @[socketDev];

			conf.entropyDevices = @[[[VZVirtioEntropyDeviceConfiguration alloc] init]];

			// No network devices at all. That is not an omission: the sandbox's
			// only route out is the proxy, and a machine with no interface
			// cannot have one by accident.
			conf.networkDevices = @[];

			// A boot with no console is a boot nobody can debug.
			if (cfg->console) {
				NSFileHandle *log = [NSFileHandle fileHandleForWritingAtPath:@(cfg->console)];
				if (log) {
					m.console = log;
					VZVirtioConsoleDeviceSerialPortConfiguration *serial =
						[[VZVirtioConsoleDeviceSerialPortConfiguration alloc] init];
					serial.attachment = [[VZFileHandleSerialPortAttachment alloc]
						initWithFileHandleForReading:nil fileHandleForWriting:log];
					conf.serialPorts = @[serial];
				}
			}

			// The state disk. The disk is the size limit, which is the thing
			// a container could never enforce.
			//
			// Both disks sync with fsync rather than the default full sync. A
			// full sync is F_FULLFSYNC, which empties the drive's own cache
			// every time: 14ms for each flush the guest asks for, and dpkg
			// asks for several per package, so installing Node spent a minute
			// and a half after its downloads had finished. fsync is 0.65ms.
			// What it gives up is only what the drive held when the Mac
			// itself lost power; stopping the sandbox still syncs everything.
			if (cfg->disk) {
				NSError *diskErr = nil;
				VZDiskImageStorageDeviceAttachment *att = [[VZDiskImageStorageDeviceAttachment alloc]
					initWithURL:[NSURL fileURLWithPath:@(cfg->disk)] readOnly:NO
					cachingMode:VZDiskImageCachingModeAutomatic
					synchronizationMode:VZDiskImageSynchronizationModeFsync
					error:&diskErr];
				if (!att) {
					copyError(err, errlen, diskErr, @"the sandbox's state disk");
					return NULL;
				}
				NSMutableArray *disks = [NSMutableArray arrayWithObject:
					[[VZVirtioBlockDeviceConfiguration alloc] initWithAttachment:att]];

				// The environment, shared by every sandbox in this workspace
				// and read-only for all but the one building it. Read-only is
				// what makes sharing it safe: the others lay a writable layer
				// over it inside rather than writing the image everybody has.
				if (cfg->envdisk) {
					NSError *envErr = nil;
					VZDiskImageStorageDeviceAttachment *envAtt =
						[[VZDiskImageStorageDeviceAttachment alloc]
							initWithURL:[NSURL fileURLWithPath:@(cfg->envdisk)]
							readOnly:cfg->envwritable ? NO : YES
							cachingMode:VZDiskImageCachingModeAutomatic
							synchronizationMode:VZDiskImageSynchronizationModeFsync
							error:&envErr];
					if (!envAtt) {
						copyError(err, errlen, envErr, @"the workspace's environment disk");
						return NULL;
					}
					[disks addObject:[[VZVirtioBlockDeviceConfiguration alloc]
						initWithAttachment:envAtt]];
				}
				conf.storageDevices = disks;
			}

			NSError *confErr = nil;
			if (![conf validateWithError:&confErr]) {
				copyError(err, errlen, confErr, @"the machine's configuration");
				return NULL;
			}

			m.vm = [[VZVirtualMachine alloc] initWithConfiguration:conf queue:m.queue];

			__block NSError *setupErr = nil;
			dispatch_sync(m.queue, ^{
				m.vm.delegate = m;
				VZVirtioSocketDevice *dev = (VZVirtioSocketDevice *)m.vm.socketDevices.firstObject;
				if (!dev) {
					setupErr = [NSError errorWithDomain:@"john" code:1 userInfo:@{
						NSLocalizedDescriptionKey: @"the machine came up with no socket device"}];
					return;
				}
				for (unsigned int i = 0; i < cfg->nports; i++) {
					JohnVZPort *p = [[JohnVZPort alloc] init];
					p.port = cfg->ports[i];
					p.owner = m;
					VZVirtioSocketListener *l = [[VZVirtioSocketListener alloc] init];
					l.delegate = p;
					[m.listeners addObject:p];
					[m.listeners addObject:l];
					[dev setSocketListener:l forPort:cfg->ports[i]];
				}
			});
			if (setupErr) {
				copyError(err, errlen, setupErr, @"starting the machine");
				return NULL;
			}

			// start is asynchronous and reports on the machine's own queue, so
			// the wait happens on this thread and never on that one.
			dispatch_semaphore_t done = dispatch_semaphore_create(0);
			__block NSError *startErr = nil;
			dispatch_async(m.queue, ^{
				[m.vm startWithCompletionHandler:^(NSError *e) {
					startErr = e;
					dispatch_semaphore_signal(done);
				}];
			});
			dispatch_semaphore_wait(done, DISPATCH_TIME_FOREVER);
			if (startErr) {
				copyError(err, errlen, startErr, @"starting the machine");
				return NULL;
			}
			return (__bridge_retained void *)m;
		}
	}
	copyError(err, errlen, nil, @"the sandbox needs macOS 12 or later");
	return NULL;
}

int vz_accept(void *handle, unsigned int port, int timeout_ms, char *err, size_t errlen) {
	@autoreleasepool {
		JohnVZ *m = (__bridge JohnVZ *)handle;
		NSDate *deadline = [NSDate dateWithTimeIntervalSinceNow:timeout_ms / 1000.0];
		[m.cond lock];
		for (;;) {
			NSMutableArray *q = m.waiting[@(port)];
			if (q.count > 0) {
				int fd = [q[0] intValue];
				[q removeObjectAtIndex:0];
				[m.cond unlock];
				return fd;
			}
			if (m.stopped) {
				[m.cond unlock];
				copyError(err, errlen, nil, @"the machine stopped before the agent connected");
				return -1;
			}
			if (![m.cond waitUntilDate:deadline]) {
				[m.cond unlock];
				return -2;
			}
		}
	}
}

void vz_forget(void *handle, int fd) {
	@autoreleasepool {
		JohnVZ *m = (__bridge JohnVZ *)handle;
		[m.cond lock];
		VZVirtioSocketConnection *c = m.connections[@(fd)];
		[m.connections removeObjectForKey:@(fd)];
		[m.cond unlock];
		[c close];
	}
}

int vz_wait_stopped(void *handle, int timeout_ms) {
	@autoreleasepool {
		JohnVZ *m = (__bridge JohnVZ *)handle;
		NSDate *deadline = [NSDate dateWithTimeIntervalSinceNow:timeout_ms / 1000.0];
		[m.cond lock];
		while (!m.stopped && [m.cond waitUntilDate:deadline]) {
		}
		BOOL stopped = m.stopped;
		[m.cond unlock];
		return stopped ? 1 : 0;
	}
}

void vz_stop(void *handle) {
	@autoreleasepool {
		JohnVZ *m = (__bridge JohnVZ *)handle;
		dispatch_semaphore_t done = dispatch_semaphore_create(0);
		dispatch_async(m.queue, ^{
			if (m.vm.canStop) {
				[m.vm stopWithCompletionHandler:^(NSError *e) {
					dispatch_semaphore_signal(done);
				}];
			} else {
				dispatch_semaphore_signal(done);
			}
		});
		// A machine that will not stop is not a reason to hang the client;
		// releasing the handle tears the rest of it down either way.
		dispatch_semaphore_wait(done, dispatch_time(DISPATCH_TIME_NOW, 10ull * NSEC_PER_SEC));
		[m markStopped];
	}
}

void vz_release(void *handle) {
	@autoreleasepool {
		JohnVZ *m = (__bridge_transfer JohnVZ *)handle;
		[m.console closeFile];
		m.vm = nil;
	}
}
