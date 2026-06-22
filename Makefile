# 9vz — 9front under Apple Virtualization.framework
#
# This Makefile only builds and signs the binary. To run the VM, invoke
# ./9vz directly with flags (see README "Usage"). The binary takes all of
# its configuration from flags, so there is no launcher target here.
BIN := 9vz

all: build

deps:
	go get github.com/Code-Hex/vz/v3@latest
	go get golang.org/x/sys/unix@latest
	go mod tidy

# Virtualization.framework refuses unsigned clients, so build and codesign
# with the com.apple.security.virtualization entitlement in one step.
build:
	go build -o $(BIN) .
	codesign --entitlements vz.entitlements -s - $(BIN)

clean:
	rm -f $(BIN) efistore.bin

.PHONY: all deps build clean
