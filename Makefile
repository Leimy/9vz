# 9vz — 9front under Apple Virtualization.framework
BIN := 9vz

all: build

deps:
	go get github.com/Code-Hex/vz/v3@latest
	go get golang.org/x/sys/unix@latest
	go mod tidy

build:
	go build -o $(BIN) .
	codesign --entitlements vz.entitlements -s - $(BIN)

# Phase 1: direct kernel load. KERNEL/DISK set on the command line, e.g.
#   make run KERNEL=./9virt DISK=./9front.raw CMDLINE='console=0'
run: build
	./$(BIN) -kernel $(KERNEL) -disk $(DISK) -cmdline '$(CMDLINE)'

# Sanity check: EFI firmware boot. EDK2 banner on the serial console
# proves the VZ serial plumbing works before we blame 9front.
run-efi: build
	./$(BIN) -efi -disk $(DISK)

clean:
	rm -f $(BIN) efistore.bin

.PHONY: all deps build run run-efi clean
