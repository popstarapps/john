# john — one executable: client, backend, standalone.
#
# Daily:
#   make build        ./john for this machine, with the guest image it boots
#                     (just checking it compiles? `go build .` is enough)
#   make test         every test; hypervisor-gated tests self-skip
#
# Before tagging a release:
#   make test-race    the tests under the race detector
#   make test-machine the sandbox tests inside a real booted kernel (qemu-system-
#                     <arch> on PATH adds a QEMU run to the platform's machine)
#   make e2e          the first-run scenario end to end in a real terminal,
#                     reviewed by `claude -p` (needs ANTHROPIC_API_KEY)
#
# Releasing (on a Mac):
#   git tag -a v0.1.0 -m "…" && git push origin v0.1.0
#   make publish      checks, tests, runs `make release`, uploads to GitHub
#   make release      the same build into dist/ without uploading, to try out
#
# Rarely:
#   make clients      just the gzipped client downloads the backend embeds
#   make guest-image  the Linux images the sandbox boots, for every CPU (curl,
#                     unsquashfs, cpio; `make build` makes this machine's only)
#   make guest-pins   move the guest image to a new Alpine, on purpose
#   make console      the web console into console/dist (needs node)
#   make dev-console  the console dev server against a local backend

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS  = -ldflags "-X main.version=$(VERSION)"

# One Mac builds every platform. The macOS client needs cgo for
# Virtualization.framework and codesign for the entitlement macOS requires
# before it will start a virtual machine; the Apple SDK cross-compiles amd64 from
# arm64. Windows (vmcompute.dll) and Linux (libkrun, opened with dlopen) are
# pure Go — see sandbox/libkrun_linux.go for what that costs on Linux.
CLIENT_PLATFORMS = darwin/arm64 darwin/amd64 windows/amd64 linux/amd64 linux/arm64

# Ad hoc by default, which runs the sandbox only on the Mac that built it.
# Builds for other Macs need a real identity (and notarizing, not done here):
#	make release CODESIGN_ID="Developer ID Application: Your Name (TEAMID)"
CODESIGN_ID ?= -
ENTITLE = codesign --entitlements sandbox/machine_vz.plist -f -s

EMBED_DIR   = backend/embedded_clients
GUEST_EMBED = sandbox/embedded_guest
GUEST_DIR   = build/guest
# Guests virtualize rather than emulate, so the image follows the host CPU:
# two images serve five platforms.
GUEST_ARCHES = aarch64 x86_64
HOST_GUEST_ARCH = $(if $(filter arm64 aarch64,$(shell uname -m)),aarch64,x86_64)
# mkimage.sh holds the default; naming it here makes `make guest-pins ALPINE=…` work.
ALPINE ?= 3.24.1

# Shell helpers for the recipes below.
#   embed_guest <arch>              put that image where -tags embed_guest finds it
#   xbuild <os> <arch> <tags> <out> cross-build one platform, signing macOS
HELPERS = \
  embed_guest() { rm -rf $(GUEST_EMBED) && mkdir -p $(GUEST_EMBED) && \
    cp $(GUEST_DIR)/$$1/kernel.gz $(GUEST_DIR)/$$1/initramfs.cpio.gz $(GUEST_EMBED)/; }; \
  xbuild() { \
    if [ $$1 = darwin ]; then \
      case $$2 in arm64) cc="clang -arch arm64" ;; *) cc="clang -arch x86_64" ;; esac; \
      CGO_ENABLED=1 CC="$$cc" GOOS=$$1 GOARCH=$$2 go build -tags $$3 $(LDFLAGS) -o $$4 . && \
      codesign --entitlements sandbox/machine_vz.plist --options runtime -f -s $(CODESIGN_ID) $$4; \
    else \
      CGO_ENABLED=0 GOOS=$$1 GOARCH=$$2 go build -tags $$3 $(LDFLAGS) -o $$4 .; \
    fi; }

.PHONY: build test test-race e2e guest-image guest-pins test-machine console clients release publish dev-console clean

# Only this machine's guest image: the other one is for other CPUs.
build: console
	@$(call mkimages,$(HOST_GUEST_ARCH),)
	@$(HELPERS); set -e; \
	embed_guest $(HOST_GUEST_ARCH); \
	go build -tags embed_guest $(LDFLAGS) -o john .; \
	rm -rf $(GUEST_EMBED); \
	if [ "$$(uname -s)" = Darwin ]; then $(ENTITLE) $(CODESIGN_ID) john; fi
	@echo "built ./john with the $(HOST_GUEST_ARCH) guest image — see readme.md"

# The console tests check the browser half of the trace format against bytes
# the Go writer produced.
test:
	go test ./...
	@if [ -d console/node_modules ]; then cd console && npm test --silent; \
	 else echo "skipping console tests (run 'npm ci' in console/ first)"; fi

# The first-run scenario, end to end, against the newest full build in dist/:
# standalone, a model, the sandbox, a React app built with Node and npm through
# approved network access, a restart, and apply. Needs ANTHROPIC_API_KEY; see
# e2e/main.go. `make e2e E2E="-build"` builds the release first.
e2e:
	go run ./e2e $(E2E)

test-race:
	go test -race ./...

# $(call mkimages,<arches>,<env>): run mkimage.sh for each guest architecture.
mkimages = for a in $(1); do \
	  echo "== guest image for $$a"; \
	  $(2) VERSION=$(VERSION) sandbox/guest/mkimage.sh $(GUEST_DIR)/$$a $$a || exit 1; \
	done

guest-image:
	@$(call mkimages,$(GUEST_ARCHES),)

# Re-resolves the package closure and rewrites sandbox/guest/alpine.pins and
# sandbox/guest/apk; the diff it leaves is the sandbox's changelog and belongs
# in a commit of its own.
guest-pins:
	@$(call mkimages,$(GUEST_ARCHES),REFRESH_PINS=1 ALPINE=$(ALPINE))
	@echo
	@git --no-pager diff --stat -- sandbox/guest/alpine.pins sandbox/guest/apk || true
	@git status --short -- sandbox/guest/apk || true

# The whole sandbox package, on every machine this host can start: the
# platform's own, plus QEMU when it is on PATH (it emulates, so it also runs on a
# build VM without nested virtualization). The test binary is built and signed
# by hand because `go test` cannot give it the virtualization entitlement.
test-machine: guest-image
	@$(HELPERS); set -e; \
	img=$(abspath $(GUEST_DIR))/$(HOST_GUEST_ARCH); \
	gunzip -kf $$img/kernel.gz; \
	dir=$$(mktemp -d); bin=$$dir/sandbox.test; \
	embed_guest $(HOST_GUEST_ARCH); \
	go test -c -tags embed_guest -o $$bin ./sandbox; \
	rm -rf $(GUEST_EMBED); \
	if [ "$$(uname -s)" = Darwin ]; then $(ENTITLE) - $$bin; fi; \
	if qemu=$$(command -v qemu-system-$(HOST_GUEST_ARCH)); then \
	  export JOHN_QEMU=$$qemu JOHN_QEMU_KERNEL=$$img/kernel \
	         JOHN_QEMU_INITRAMFS=$$img/initramfs.cpio.gz; \
	else \
	  echo "test-machine: no qemu-system-$(HOST_GUEST_ARCH) on PATH; running the platform's machine only"; \
	fi; \
	JOHN_GUEST_KERNEL=$$img/kernel JOHN_GUEST_INITRAMFS=$$img/initramfs.cpio.gz \
	  $$bin -test.v; \
	rm -rf $$dir

# File targets, so npm ci runs when the lockfile moves and vite when a source
# does. vite empties dist/, and console/embed.go needs the directory to exist
# even in a clone that never ran node, hence the .gitkeep — which vite copies
# back in from console/public/ on every build.
CONSOLE_SRC = console/index.html console/vite.config.ts console/tsconfig.json \
              $(shell find console/src console/public -type f 2>/dev/null)

console: console/dist/index.html

console/node_modules: console/package-lock.json console/package.json
	cd console && npm ci --no-audit --no-fund
	@touch console/node_modules

console/dist/index.html: console/node_modules $(CONSOLE_SRC)
	cd console && npm run build
	@touch console/dist/.gitkeep

# One client per platform, each carrying its CPU's guest image and no clients
# of its own (which is what keeps this from being circular), gzipped to embed.
clients: console guest-image
	@if [ "$$(uname -s)" != Darwin ]; then case "$(CLIENT_PLATFORMS)" in \
	  *darwin*) echo "make clients: macOS clients need a Mac (cgo and codesign). Elsewhere:"; \
	            echo "    make clients CLIENT_PLATFORMS='windows/amd64 linux/amd64 linux/arm64'"; exit 1 ;; \
	  *) echo "make clients: not on a Mac; building $(CLIENT_PLATFORMS) only." ;; \
	esac; fi
	rm -rf $(EMBED_DIR)
	mkdir -p $(EMBED_DIR)
	@$(HELPERS); set -e; for p in $(CLIENT_PLATFORMS); do \
	  os=$${p%/*}; arch=$${p#*/}; ext=; [ $$os = windows ] && ext=.exe; \
	  case $$arch in arm64) ga=aarch64 ;; *) ga=x86_64 ;; esac; \
	  echo "  client $$os/$$arch (guest image $$ga)"; \
	  embed_guest $$ga; \
	  out=$(EMBED_DIR)/john-$$os-$$arch$$ext; \
	  xbuild $$os $$arch embed_guest $$out; \
	  gzip -f $$out; \
	done
	@rm -rf $(GUEST_EMBED)

# dist/ is exactly what `make publish` uploads: a `full` build per platform
# (client and backend, serving every client as a download, with its own CPU's
# guest image so it runs sandboxes itself), the same `clientonly` bytes it
# serves, and SHA256SUMS.
RELEASE_PLATFORMS ?= $(CLIENT_PLATFORMS)

release: clients
	rm -rf dist; mkdir -p dist
	@$(HELPERS); set -e; for p in $(RELEASE_PLATFORMS); do \
	  os=$${p%/*}; arch=$${p#*/}; ext=; [ $$os = windows ] && ext=.exe; \
	  case $$arch in arm64) ga=aarch64 ;; *) ga=x86_64 ;; esac; \
	  echo "  full $$os/$$arch (guest image $$ga)"; \
	  embed_guest $$ga; \
	  xbuild $$os $$arch embed_clients,embed_guest dist/john-$(VERSION)-full-$$os-$$arch$$ext; \
	done
	@rm -rf $(GUEST_EMBED)
	@set -e; for f in $(EMBED_DIR)/john-*.gz; do \
	  b=$$(basename $$f .gz); \
	  cp $$f dist/john-$(VERSION)-clientonly-$${b#john-}.gz; \
	done
	@cd dist && if command -v sha256sum >/dev/null 2>&1; then sha256sum john-*; \
	  else shasum -a 256 john-*; fi > SHA256SUMS
	@echo "built: dist/ — $(words $(RELEASE_PLATFORMS)) full builds (each serving all client"
	@echo "       downloads), $(words $(CLIENT_PLATFORMS)) clientonly builds, and SHA256SUMS"

# The version is baked into the binaries, so a release must be built from a
# clean tree at a tag that is already on origin (GitHub would otherwise make the
# tag itself, from whatever its default branch is). The gates run before the
# long build; existing releases are never overwritten, since someone may have
# the old bytes. test-race and test-machine are yours to run before tagging.
#
#	make publish NOTES=notes.md        write the release notes yourself
#	make publish DRAFT=--draft         upload, but leave it unpublished
#	make publish GITHUB_REPO=me/fork   somewhere other than the real repo
GITHUB_REPO ?= popstarapps/john

publish:
	@fail() { echo "make publish: $$*"; exit 1; }; \
	 command -v gh >/dev/null 2>&1 || fail "needs the GitHub CLI: brew install gh, then gh auth login"; \
	 gh auth status >/dev/null 2>&1 || fail "gh is not logged in. Run: gh auth login"; \
	 case "$(VERSION)" in *-dirty) fail "the tree has uncommitted changes; commit or stash them" ;; esac; \
	 git describe --tags --exact-match >/dev/null 2>&1 || \
	   fail "HEAD is not a tag (version would be $(VERSION)). git tag -a v0.1.0 -m '…' && git push origin v0.1.0"; \
	 git ls-remote --exit-code --tags origin "$(VERSION)" >/dev/null 2>&1 || \
	   fail "the tag $(VERSION) is not on origin yet. git push origin $(VERSION)"; \
	 ! gh release view "$(VERSION)" --repo $(GITHUB_REPO) >/dev/null 2>&1 || \
	   fail "$(GITHUB_REPO) already has a release $(VERSION); tag a new version instead"; \
	 echo "publishing $(VERSION) to $(GITHUB_REPO)"
	$(MAKE) test
	$(MAKE) release
	gh release create "$(VERSION)" --repo $(GITHUB_REPO) --title "$(VERSION)" --verify-tag $(DRAFT) \
	  $(if $(NOTES),--notes-file "$(NOTES)",--generate-notes) \
	  dist/*
	@echo "published: https://github.com/$(GITHUB_REPO)/releases/tag/$(VERSION)"

dev-console:
	cd console && npm run dev

clean:
	rm -rf john dist $(EMBED_DIR) $(GUEST_EMBED) $(dir $(GUEST_DIR)) console/dist
	@mkdir -p console/dist && touch console/dist/.gitkeep
