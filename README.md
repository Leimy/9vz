9vz - boot 9front under Apple Virtualization.framework
=======================================================

9vz is a small Go program that boots a 9front/arm64 guest as a
first-class lightweight VM on Apple silicon, using macOS's native
Virtualization.framework (VZ) through the Code-Hex/vz bindings.
No QEMU, no firmware, no U-Boot: VZLinuxBootLoader loads the
kernel directly, and the guest talks virtio for everything.

It is the host-side half of a pair.  The guest-side half is the
vz64 9front kernel (separate repository), a native port that
carries the Linux arm64 Image header VZ requires and speaks VZ's
hardware model (virtio console/blk/net, GICv3, ARM virtual
timer, PSCI).

This tool was generated with Fable and Claude Opus (Anthropic),
working with a human who ran every build and boot.  The original
phase-plan that preceded the working port is preserved in
HISTORY.md for the historical record -- it was written when "does
it boot at all" was an open question.

Requirements
------------
  * Apple silicon Mac with a macOS recent enough for the VZ
    APIs used (Ventura or later recommended)
  * Go toolchain
  * Apple command line tools (for codesign)

Virtualization.framework refuses unsigned clients: the binary
must carry the com.apple.security.virtualization entitlement.
The Makefile handles this with an ad-hoc signature:

    make deps        # once: fetch Code-Hex/vz and x/sys
    make build       # go build && codesign --entitlements
                     #   vz.entitlements -s - 9vz

Usage
-----
    ./9vz -kernel 9vz.bin -disk 9front.raw -cmdline 'console=0
    *ncpu=1
    nobootprompt=local!/dev/sdF0/fs'

Flags:

    -kernel path    guest kernel for direct boot (must carry the
                    arm64 Image header; see check_kernel.sh)
    -initrd path    optional initrd (unused by 9front)
    -cmdline str    kernel command line, delivered via the device
                    tree /chosen node.  For 9front this carries
                    the plan9.ini-style bootargs; newlines
                    separate entries.
    -disk path      RAW disk image attached as virtio-blk
                    (convert qcow2 with qemu-img convert first)
    -ro             attach the disk read-only
    -cpus n         virtual cpu count (default 2; the vz64 kernel
                    is single-cpu for now -- pass *ncpu=1 in the
                    cmdline until SMP lands)
    -mem GiB        guest memory (default 2)
    -nonet          disable the NAT network device
    -efi            boot EFI firmware instead of a direct kernel
                    (sanity-check path; EDK2 prints on the serial
                    console even with nothing bootable)
    -efistore path  EFI variable store (created if missing)

Ctrl-C once requests a graceful stop; twice forces exit.

What it wires up
----------------
main.go builds a VZVirtualMachineConfiguration with:

  * Boot loader: VZLinuxBootLoader(kernel, cmdline[, initrd]),
    or VZEFIBootLoader with a variable store under -efi.
  * Serial console: a virtio-console device attached to stdin/
    stdout via NewFileHandleSerialPortAttachment.  The terminal
    is switched to a raw-ish mode (no echo, no canonicalization,
    ISIG kept so Ctrl-C still reaches 9vz); restored on exit.
    In the guest this appears as a virtio-console PCI device
    (1AF4:1043) -- the vz64 kernel's uartvz driver is its
    console.
  * Storage: virtio-blk backed by the -disk raw image.  The
    vz64 kernel's sdvirtio10 letters virtio-blk disks from 'F',
    so the first disk is /dev/sdF0 in the guest.
  * Network: virtio-net on a NAT attachment with a random
    locally-administered MAC.  Apple's NAT runs DHCP and DNS on
    a shared subnet (typically 192.168.64.0/24).  The HOST is
    192.168.64.1 -- the guest is some other address on that /24
    (find it with arp -a on the Mac, or cat /net/ipifc/0/status
    in the guest).  Host and guest can reach each other; the
    outside world cannot reach the guest.
  * Entropy: virtio-entropy.
  * vsock: a virtio-socket device, attached for the eventual
    9P-over-vsock control plane (unused so far).

After config validation it starts the VM and sits in a loop
multiplexing OS signals against the VM state channel, printing
state transitions (running/stopped/error) to stderr while the
guest serial flows on stdout.

Kernel image requirements
-------------------------
VZLinuxBootLoader only accepts images carrying the Linux arm64
Image header (magic "ARM\x64" at offset 56).  Inspect with:

    ./check_kernel.sh kernelfile

The vz64 9front kernel embeds this header natively; its build
produces a U-Boot uImage (9vz.u), so strip the 64-byte uImage
wrapper before booting:

    dd if=9vz.u of=9vz.bin bs=64 skip=1
    ./9vz -kernel 9vz.bin ...

For kernels that lack the header, mkarm64hdr.py can prepend one
(that was the original bring-up path; the embedded header made
it obsolete -- the script refuses to double-wrap and will exit
if the magic is already present).

Getting a useful guest
----------------------
The serial rc prompt works out of the box.  For graphics, run
the guest as a cpu service and connect with drawterm from the
Mac (the 9front drawterm speaks rcpu/dp9ik):

    drawterm -h <guest-ip> -a <guest-ip> -u glenda

See the vz64 repository's NOTES for the full cpu/auth conversion
recipe, and remember fshalt in the guest before Ctrl-C -- hjfs
buffers writes.

Files
-----
    main.go            the harness (boot loaders, devices, signal
                       and state loop)
    termios_darwin.go  raw-mode ioctl constants
    vz.entitlements    com.apple.security.virtualization
    check_kernel.sh    arm64 Image header inspector
    mkarm64hdr.py      legacy header prepender (obsolete for the
                       vz64 kernel, kept for other kernels)
    Makefile           deps / build+sign / run / run-efi
    HISTORY.md         the original pre-port phase plan
    9front-on-vz.md    additional notes
