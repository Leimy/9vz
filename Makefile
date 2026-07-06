# 9vz — 9front under Apple Virtualization.framework
#
# This Makefile only builds and signs the binary. To run the VM, invoke
# ./9vz directly with flags (see README "Usage"). The binary takes all of
# its configuration from flags, so there is no launcher target here.
#
# Dependencies are VENDORED (committed under ./vendor) and pinned by exact
# version below. We build with -mod=vendor so the build uses ONLY the
# committed tree and never reaches out to the network or silently upgrades.
# Do NOT use `go get ...@latest` here: a surprise bump of Code-Hex/vz can
# break the cgo/AppKit window code we patch. Bump deliberately by editing
# the pinned versions, running `make deps vendor`, testing, and committing
# the new vendor tree.
BIN := 9vz
INFO_PLIST := Info.plist
INFO_PLIST_LDFLAGS := -extldflags=-Wl,-sectcreate,__TEXT,__info_plist,$(INFO_PLIST)

# Pinned dependency versions. Change these on purpose, never automatically.
VZ_VERSION  := v3.7.1
SYS_VERSION := v0.46.0

all: build

# Fetch the pinned versions and refresh go.mod/go.sum. Run this only when
# intentionally changing a pinned version above; normal builds use ./vendor.
deps:
	go get github.com/Code-Hex/vz/v3@$(VZ_VERSION)
	go get golang.org/x/sys@$(SYS_VERSION)
	go mod tidy

# Materialize the committed vendor tree from go.mod/go.sum, then apply our
# local patches to the vendored third-party code (see ./patches).  Commit
# ./vendor after running this.  -mod=vendor builds then ignore the module
# cache and the network entirely.
#
# The patches adjust the Code-Hex/vz AppKit window: they disable the view's
# automaticallyReconfiguresDisplay (so the -scale scanout size actually takes
# effect) and start the window fullscreen.  go mod vendor REWRITES the vendor
# tree, so the edits must be (re)applied here every time.  patches/apply.sh
# does targeted, idempotent sed/awk edits (a unified diff proved too fragile);
# it skips edits already present and fails loudly if the expected source text
# is gone (e.g. after a vz version bump).
vendor:
	go mod vendor
	sh patches/apply.sh

# Sanity-check the dependency state: module checksums (cache) and that the
# committed ./vendor tree actually builds with -mod=vendor.  Note this builds
# the PATCHED vendor tree, so it confirms the patches still compile too.
verify:
	go mod verify
	go build -mod=vendor -o /dev/null .

# Virtualization.framework refuses unsigned clients, so build and codesign
# with the com.apple.security.virtualization entitlement in one step. Embed
# Info.plist too, so macOS TCC has an NSMicrophoneUsageDescription for -mic.
# -mod=vendor forces the build to use the committed ./vendor tree.
build:
	go build -mod=vendor -ldflags '$(INFO_PLIST_LDFLAGS)' -o $(BIN) .
	codesign -f --entitlements vz.entitlements -s - $(BIN)

clean:
	rm -f $(BIN) efistore.bin

.PHONY: all deps vendor verify build clean
