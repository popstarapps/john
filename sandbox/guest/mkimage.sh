#!/bin/sh
#
# Builds the guest image: the kernel john's sandbox boots, and an initramfs
# holding an init, a shell and the agent.
#
#   sandbox/guest/mkimage.sh <outdir>
#
# What comes out is two files — kernel and initramfs.cpio.gz — and they are
# what every machine boots, whichever hypervisor is underneath. Alpine supplies
# the kernel and busybox because a sandbox wants the smallest Linux that can
# still run a project's build, and Alpine is that with a package index behind
# it. The agent is the john binary itself, built for the guest, so the two ends
# of the guest link are always the same release.
#
# Everything here runs as an ordinary user. Nothing is mounted, no loop device
# is claimed and no package is installed on the machine doing the building,
# because a build that needs root is a build that cannot run in CI.
set -eu

# The guest's architecture is the HOST's, not the host operating system's. The
# machines john ships virtualize rather than emulate, so an Apple Silicon Mac
# runs an arm64 guest and an x86-64 Windows or Linux machine runs an amd64 one.
# Five client platforms, two guest images.
#
# It defaults to this machine because that is what a developer running the
# tests wants; a release builds both explicitly.
OUT=${1:?usage: mkimage.sh <outdir> [arch]}
ARCH=${2:-${ARCH:-$(uname -m)}}
ALPINE=${ALPINE:-3.24.1}
# The branch is derived from the release rather than tracking latest-stable.
# They are the same directory today and they stop being the same the day Alpine
# cuts 3.25: latest-stable would quietly start serving a different kernel, a
# different busybox and a different libc to a build of john that nobody
# changed. A guest image is the one thing in the product that every developer
# runs identically, so it moves when somebody moves it.
BRANCH=${BRANCH:-v${ALPINE%.*}}
CDN=${CDN:-https://dl-cdn.alpinelinux.org/alpine}
GO=${GO:-go}
# REFRESH_PINS=1 re-resolves what the image is built from and rewrites the pins
# for this architecture instead of enforcing them. `make guest-pins` is that,
# for both. Nothing else should set it: a build that can rewrite its own
# expectations is not pinned to anything.
REFRESH_PINS=${REFRESH_PINS:-0}

case "$ARCH" in
	aarch64|arm64) ALPINE_ARCH=aarch64; GOARCH=arm64 ;;
	x86_64|amd64)  ALPINE_ARCH=x86_64;  GOARCH=amd64 ;;
	*) echo "mkimage: unsupported architecture $ARCH" >&2; exit 2 ;;
esac

here=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
repo=$(CDPATH= cd -- "$here/../.." && pwd)
# Absolute from here on. This script changes directory while it unpacks the
# kernel's modules, so a relative output directory stops resolving halfway
# through — and the caller passing a relative one is the normal case.
mkdir -p "$OUT"
OUT=$(CDPATH= cd -- "$OUT" && pwd)
work="$OUT/build"
mkdir -p "$work"

# The pins: every file this image is built out of, by sha256.
#
# Pinning the version is not enough on its own. Alpine's branch directory holds
# whatever the branch currently is — a point release replaces the tarballs, a
# package bump replaces the .apk — so a build in June and a build in September
# can name the same version and produce different images. The checksums are
# what make "every developer's sandbox is identical" true across releases, and
# they turn a mirror that serves something else into a build that stops rather
# than an image nobody can account for.
pins="$here/alpine.pins"
: > "$work/pins.new"

sha256_of() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | cut -d" " -f1
	else
		shasum -a 256 "$1" | cut -d" " -f1
	fi
}

# pinned <kind> <name> prints the sha256 this architecture expects, or nothing.
pinned() {
	[ -f "$pins" ] || return 0
	awk -v a="$ALPINE_ARCH" -v k="$1" -v n="$2" \
		'$1 == a && $2 == k && $3 == n { print $4 }' "$pins"
}

# fetch <url> <dest> <kind> <name> downloads what is not already here and
# refuses anything that is not what this release was built from.
fetch() {
	if [ ! -f "$2" ]; then
		echo "fetching $4"
		curl -fsS -o "$2" "$1"
	fi
	got=$(sha256_of "$2")
	if [ "$REFRESH_PINS" = 1 ]; then
		echo "$ALPINE_ARCH $3 $4 $got" >> "$work/pins.new"
		return 0
	fi
	want=$(pinned "$3" "$4")
	if [ -z "$want" ]; then
		echo "mkimage: $4 is not pinned in $(basename "$pins") for $ALPINE_ARCH." >&2
		echo "  Alpine has moved on, or the image needs something new. Look at what" >&2
		echo "  changed, then record it deliberately:  make guest-pins" >&2
		exit 1
	fi
	if [ "$got" != "$want" ]; then
		echo "mkimage: $4 is not the file this release was built from." >&2
		echo "  expected $want" >&2
		echo "  got      $got" >&2
		echo "  The mirror is serving something else. Do not refresh the pins to make" >&2
		echo "  this go away without knowing why it changed." >&2
		exit 1
	fi
}

netboot="alpine-netboot-$ALPINE-$ALPINE_ARCH.tar.gz"
fetch "$CDN/$BRANCH/releases/$ALPINE_ARCH/$netboot" "$work/$netboot" file "$netboot"
rm -rf "$work/netboot" && mkdir -p "$work/netboot"
tar xzf "$work/$netboot" -C "$work/netboot"

kver=$(basename "$(ls -d "$work"/netboot/boot/dtbs-virt 2>/dev/null || true)" >/dev/null 2>&1 || true)
kver=$(ls "$work"/netboot/boot/config-*-virt | head -1 | sed 's/.*config-//')
echo "kernel $kver"

# Alpine ships the kernel as an EFI "zboot" image: a PE wrapper around a
# compressed Linux image. Booting it needs firmware, and a hypervisor that
# loads a kernel directly needs the image inside. Unwrap it.
"${PYTHON:-python3}" - "$work/netboot/boot/vmlinuz-virt" "$work/kernel" <<'PY'
import gzip, lzma, struct, subprocess, sys
src, dst = sys.argv[1], sys.argv[2]
data = open(src, 'rb').read()
if data[0x38:0x3c] == b'ARM\x64' or data[:4] == b'\x1f\x8b':
    open(dst, 'wb').write(data)          # already a bootable image
    sys.exit(0)
if data[4:8] != b'zimg':
    open(dst, 'wb').write(data)
    sys.exit(0)
off, size = struct.unpack_from('<II', data, 8)
comp = data[0x18:0x38].split(b'\x00')[0].decode()
payload = data[off:off + size]
if comp == 'gzip':
    raw = gzip.decompress(payload)
elif comp in ('lzma', 'xz'):
    raw = lzma.decompress(payload)
elif comp == 'zstd':
    raw = subprocess.run(['zstd', '-d', '-c'], input=payload, capture_output=True, check=True).stdout
else:
    raise SystemExit('mkimage: unknown kernel compression ' + comp)
open(dst, 'wb').write(raw)
PY

# busybox is the shell and the coreutils inside the sandbox — `run` needs a
# shell, and a project's build needs the rest.
mini="alpine-minirootfs-$ALPINE-$ALPINE_ARCH.tar.gz"
fetch "$CDN/$BRANCH/releases/$ALPINE_ARCH/$mini" "$work/$mini" file "$mini"

root="$work/initramfs"
rm -rf "$root"
mkdir -p "$root/bin" "$root/sbin" "$root/dev" "$root/proc" "$root/sys" "$root/tmp" "$root/lib/modules" "$root/etc" "$root/root"
# Alpine's busybox is linked against musl, so the loader has to come with it.
# On musl the loader is libc, so it is one file and not a set of them; the
# libc.musl symlink beside it is what everything else links against by name.
tar xzf "$work/$mini" -C "$root" ./bin/busybox ./lib/ld-musl-"$ALPINE_ARCH".so.1 ./lib/libc.musl-"$ALPINE_ARCH".so.1
[ -x "$root/bin/busybox" ] || { echo "mkimage: no busybox in the minirootfs" >&2; exit 1; }

# The modules the agent cannot come up without.
#
# FUSE, because the workspace is a FUSE mount. vsock, because that is how the
# agent reaches the host on every machine john ships — virtio-vsock under
# Virtualization.framework and KVM, hv_sock under Hyper-V. And a block driver
# with ext4 and its dependencies, because the sandbox's own writes live on a
# disk and the disk is what makes the size limit real.
#
# There are two block drivers for the same reason there are two socket
# transports: Hyper-V has no virtio at all, and gives a guest its disks over
# VMBus through hv_storvsc as a SCSI bus — so virtio_blk finds nothing there
# and hv_storvsc finds nothing anywhere else. scsi_transport_fc and sd_mod come
# with it, and so does the SCSI core — scsi_common and scsi_mod — which
# Alpine's x86_64 kernel builds in and its aarch64 kernel ships as modules.
# Without them on aarch64 every boot logged sixty lines of "Unknown symbol" as
# hv_storvsc and sd_mod failed to load.
#
# overlay, because the environment disk is shared and read-only and a session
# still has every right to install something into it — so the sandbox lays a
# writable layer of its own over it.
#
# And virtio-rng, because a machine's entropy device is no use to a guest that
# cannot see it: without the driver the kernel's random pool starts empty, and
# the first thing that asks for randomness — mke2fs, generating the filesystem
# UUID — blocks for seconds waiting on it.
#
# All of them are modules in Alpine's kernel rather than built in, so a guest
# without them boots perfectly and then cannot open the link, mount its disk,
# or find any entropy, all of which look like nothing at all. They are small; carry all three transports
# and let init load what the machine turns out to have.
mods="$work/modules"
rm -rf "$mods" && mkdir -p "$mods"
( cd "$mods" && unsquashfs -q -f -d . "$work/netboot/boot/modloop-virt" >/dev/null )
# A module this kernel builds in is there without being copied, and listed in
# modules.builtin; one that is neither copied nor built in is a guest that
# boots and cannot do its job, so the build stops.
builtin=$(find "$mods" -name modules.builtin | head -1)
for m in fuse vsock vmw_vsock_virtio_transport_common vmw_vsock_virtio_transport hv_sock \
	virtio_blk scsi_common scsi_mod scsi_transport_fc hv_storvsc sd_mod \
	crc16 mbcache jbd2 ext4 overlay rng-core virtio-rng; do
	found=$(find "$mods" -name "$m.ko" | head -1)
	if [ -n "$found" ]; then
		cp "$found" "$root/lib/modules/"
	elif [ -n "$builtin" ] && grep -q "/$m.ko\$" "$builtin"; then
		: # built into this kernel
	else
		echo "mkimage: $m is neither a module nor built into this kernel" >&2
		exit 1
	fi
done
[ -f "$root/lib/modules/fuse.ko" ] || { echo "mkimage: fuse.ko is not optional" >&2; exit 1; }
[ -f "$root/lib/modules/ext4.ko" ] || { echo "mkimage: ext4.ko is not optional" >&2; exit 1; }

# mke2fs, so the guest can make a filesystem on the blank disk the machine
# hands it the first time it boots.
#
# It comes from Alpine's own packages rather than from the build machine,
# because the build machine is usually a Mac and has no mke2fs for Linux — and
# because a filesystem made by whatever happened to be installed is not a
# reproducible image. busybox is not an option: Alpine builds it without any of
# the ext mkfs applets.
#
# The versions are pinned like everything else, and the package index is only
# read when the pins are being refreshed. Alpine's branch repository holds one
# version of each package and replaces it in place, so resolving the closure on
# every build is how an image quietly acquires a different mke2fs than the one
# a release was tested with.
apkindex="$work/APKINDEX"
if [ "$REFRESH_PINS" = 1 ] && [ ! -f "$apkindex" ]; then
	echo "fetching the package index"
	curl -fsS -o "$work/APKINDEX.tar.gz" "$CDN/$BRANCH/main/$ALPINE_ARCH/APKINDEX.tar.gz"
	tar xzOf "$work/APKINDEX.tar.gz" APKINDEX > "$apkindex"
fi

# The shared libraries a package needs are its own business, and Alpine already
# records them: every package lists the sonames it needs and the sonames it
# provides. Following that is a dozen lines and it is right; naming the
# libraries by hand works until a version bump adds one, and then it fails
# inside the guest as a relocation error nobody is watching for.
pkgclosure() {
	awk -v roots="$*" '
	BEGIN { RS = ""; FS = "\n" }
	{
		p = ""; v = ""; d = ""; pr = ""
		for (i = 1; i <= NF; i++) {
			if ($i ~ /^P:/) p = substr($i, 3)
			else if ($i ~ /^V:/) v = substr($i, 3)
			else if ($i ~ /^D:/) d = substr($i, 3)
			else if ($i ~ /^p:/) pr = substr($i, 3)
		}
		ver[p] = v; dep[p] = d
		n = split(pr, a, " ")
		for (i = 1; i <= n; i++) { t = a[i]; sub(/=.*/, "", t); prov[t] = p }
		prov[p] = p
	}
	END {
		n = split(roots, r, " ")
		for (i = 1; i <= n; i++) { queue[++tail] = r[i]; seen[r[i]] = 1 }
		while (++head <= tail) {
			pk = queue[head]
			m = split(dep[pk], dd, " ")
			for (j = 1; j <= m; j++) {
				t = dd[j]
				# !foo is a conflict, and musl is already in the initramfs.
				if (t ~ /^!/ || t !~ /^so:/ || t ~ /^so:libc\.musl/) continue
				sub(/=.*/, "", t)
				o = prov[t]
				if (o != "" && !(o in seen)) { seen[o] = 1; queue[++tail] = o }
			}
		}
		for (i = 1; i <= tail; i++) {
			pk = queue[i]
			if (ver[pk] == "") { print "MISSING " pk > "/dev/stderr"; exit 1 }
			print pk "-" ver[pk]
		}
	}' "$apkindex"
}

# Refreshing resolves the closure from the index; an ordinary build takes the
# exact packages the pins name, so it makes no request the pins do not cover.
if [ "$REFRESH_PINS" = 1 ]; then
	packages=$(pkgclosure e2fsprogs)
else
	packages=$(awk -v a="$ALPINE_ARCH" '$1 == a && $2 == "apk" { print $3 }' "$pins" |
		sed 's/\.apk$//')
	[ -n "$packages" ] || {
		echo "mkimage: no packages pinned for $ALPINE_ARCH; run: make guest-pins" >&2
		exit 1
	}
fi

# The packages themselves are kept in the repository, in apk/<arch>, beside
# the pins that name them. The release tarballs can come from the mirror
# forever, because Alpine keeps releases/. The branch repository does not: it
# holds only the newest build of each package, so the day Alpine bumps one,
# the pinned file stops existing anywhere, and every build without a warm
# cache fails, a new clone and CI first. That happened to libblkid within
# three weeks of the pins being written. The files are about half a megabyte
# per architecture. Keeping them is what makes a pin something a build can
# still meet a year later.
#
# An ordinary build takes them from there and never asks the mirror for a
# package. A refresh takes them from the mirror and writes the directory
# again, so the files and the pins always change in the same commit.
vendored="$here/apk/$ALPINE_ARCH"
apkroot="$work/apk"
rm -rf "$apkroot" && mkdir -p "$apkroot"
if [ "$REFRESH_PINS" = 1 ]; then
	rm -rf "$work/apk.new" && mkdir -p "$work/apk.new"
fi
for pkg in $packages; do
	if [ "$REFRESH_PINS" = 1 ]; then
		# Never a cached copy: a refresh records what the mirror serves now.
		rm -f "$work/$pkg.apk"
	elif [ -f "$vendored/$pkg.apk" ]; then
		cp "$vendored/$pkg.apk" "$work/$pkg.apk"
	fi
	fetch "$CDN/$BRANCH/main/$ALPINE_ARCH/$pkg.apk" "$work/$pkg.apk" apk "$pkg.apk"
	if [ "$REFRESH_PINS" = 1 ]; then
		cp "$work/$pkg.apk" "$work/apk.new/$pkg.apk"
	fi
	# An apk is a gzipped tar with signature members in front of it, which tar
	# reports and then carries on past.
	tar xzf "$work/$pkg.apk" -C "$apkroot" 2>/dev/null || true
done

mkdir -p "$root/sbin" "$root/etc"
cp "$apkroot/sbin/mke2fs" "$root/sbin/mke2fs"
cp "$apkroot/etc/mke2fs.conf" "$root/etc/mke2fs.conf"
# The loader looks in /lib; Alpine splits its libraries across /lib and
# /usr/lib. Copying rather than symlinking keeps the initramfs one flat tree
# with no /usr to set up.
find "$apkroot/lib" "$apkroot/usr/lib" -maxdepth 1 -name '*.so.*' -type f 2>/dev/null |
	while read -r so; do cp "$so" "$root/lib/"; done
find "$apkroot/lib" "$apkroot/usr/lib" -maxdepth 1 -name '*.so.*' -type l 2>/dev/null |
	while read -r so; do
		target=$(readlink "$so")
		[ -f "$root/lib/$target" ] && ln -sf "$target" "$root/lib/$(basename "$so")"
	done
[ -x "$root/sbin/mke2fs" ] || { echo "mkimage: no mke2fs" >&2; exit 1; }

# The agent is john itself, built for the guest, from the same source and the
# same release as the john the developer runs — so the two ends of the guest
# link cannot skew.
#
# The john_guest tag builds only what the agent uses. The backend, the web
# console, the terminal UI and SQLite are all unreachable from here, so the
# linker drops them and 33 MB becomes 4. That matters because the initramfs is
# unpacked into the sandbox's memory on every start, and because the one place
# john deliberately runs untrusted code should carry as little as it can.
#
# Static on purpose: the initramfs has no dynamic loader for a Go binary, and
# the guest should not need one.
echo "building the agent for linux/$GOARCH"
( cd "$repo" && CGO_ENABLED=0 GOOS=linux GOARCH="$GOARCH" "$GO" build \
	-tags john_guest -ldflags "-s -w -X main.version=${VERSION:-dev}" \
	-o "$root/bin/john" . )

cp "$here/init" "$root/init"
chmod +x "$root/init" "$root/bin/busybox" "$root/bin/john" "$root/sbin/mke2fs"

( cd "$root" && find . -print0 | cpio --null -o -H newc --quiet | gzip -9 ) > "$OUT/initramfs.cpio.gz"

# The kernel ships compressed because it travels inside the john binary and
# because it is the one part of the image that is not already compressed —
# 34 MB becomes 10. The client decompresses it once, on first use.
gzip -9 -c "$work/kernel" > "$OUT/kernel.gz"

# In refresh mode the run itself is the record: everything it fetched, with the
# hash it actually got, replacing this architecture's half of the pins and
# leaving the other architecture's alone (make guest-pins does both).
if [ "$REFRESH_PINS" = 1 ]; then
	{
		# The header is the comment block the file opens with, and only that:
		# taking every "#" line carried stray ones into the body and grew the
		# file by a copy of them each refresh.
		if [ -f "$pins" ]; then
			awk '!/^#/ { exit } { print }' "$pins"
		fi
		echo "alpine $ALPINE"
		echo "branch $BRANCH"
		echo
		if [ -f "$pins" ]; then
			# "#     make guest-pins ALPINE=3.25.0" is four fields too.
			awk -v a="$ALPINE_ARCH" 'NF == 4 && $1 !~ /^#/ && $1 != a' "$pins"
		fi
		sort "$work/pins.new"
	} > "$work/pins.out"
	mv "$work/pins.out" "$pins"
	rm -rf "$vendored" && mkdir -p "$(dirname "$vendored")"
	mv "$work/apk.new" "$vendored"
	echo "pins:      $pins (refreshed for $ALPINE_ARCH)"
	echo "packages:  $vendored"
fi

echo "kernel:    $OUT/kernel.gz   ($(du -h "$OUT/kernel.gz" | cut -f1))"
echo "initramfs: $OUT/initramfs.cpio.gz   ($(du -h "$OUT/initramfs.cpio.gz" | cut -f1))"
