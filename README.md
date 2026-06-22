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

Building
--------
Virtualization.framework refuses unsigned clients: the binary
must carry the com.apple.security.virtualization entitlement.
The Makefile builds and applies an ad-hoc signature; it does
nothing else (no launcher targets -- run ./9vz directly):

    make deps        # once: fetch Code-Hex/vz and x/sys
    make build       # go build && codesign --entitlements
                     #   vz.entitlements -s - 9vz
    make clean       # remove the binary and efistore.bin

Usage
-----
Run the signed binary directly, passing flags.  A normal serial
boot of a 9front guest:

    ./9vz -kernel 9vz.bin -disk 9front.raw -cmdline 'console=0
    *ncpu=1
    nobootprompt=local!/dev/sdF0/fs'

The same boot with a graphics window (virtio-gpu + USB keyboard
and mouse) in addition to the serial console:

    ./9vz -gui -kernel 9vz.bin -disk 9front.raw -cmdline 'console=0
    nobootprompt=local!/dev/sdF0/fs'

Sanity-check the VZ serial plumbing with an EFI firmware boot
(EDK2 prints its banner even with nothing bootable):

    ./9vz -efi -disk 9front.raw

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
    -cpus n         virtual cpu count (default 2)
    -mem GiB        guest memory (default 2)
    -nonet          disable the NAT network device
    -efi            boot EFI firmware instead of a direct kernel
                    (sanity-check path; EDK2 prints on the serial
                    console even with nothing bootable)
    -efistore path  EFI variable store (created if missing)
    -gui            open a graphics window: attaches a virtio-gpu
                    device (single scanout), a USB keyboard and a
                    USB absolute-coordinate pointing device, and
                    hands the VM to AppKit's run loop.  The serial
                    console still flows on stdio in parallel.  The
                    guest needs drivers for these devices to use
                    them (see "Native graphics" below).
    -width  px      graphics window/scanout width  (default 1440)
    -height px      graphics window/scanout height (default 900)

                    The default is a 16:10 laptop-native shape
                    (1440x900) rather than a 4:3 one.  The guest
                    kernel adopts the host scanout size via the
                    virtio-gpu GET_DISPLAY_INFO reply, so it
                    follows -width/-height automatically.

Mouse buttons on a laptop trackpad (-gui)
-----------------------------------------
A bare trackpad only has one (left) button, which the guest sees
as Plan 9 button 1.  An earlier host-side experiment remapped
Option-click to button 2 and Command-click to button 3 via a
CoreGraphics event tap (the drawterm trick).  It worked for
modifier-before-click but never reliably formed the button-1
chords rio needs for paste, and a session-wide tap risks
interfering with real hardware mice.  It has been rolled back:
9vz now passes all clicks straight through.

Mouse chording presently does not work through the graphics
console, but will over drawterm connections.  Feedback submitted
to Apple for review, but won't hold my breath.

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

(NOTE: newer vz64 kernels do this step to generate 9vz.bin for
you via the mkfile)

Getting a useful guest
----------------------
The serial rc prompt works out of the box.  For graphics, run
the guest as a cpu service and connect with drawterm from the
Mac (the 9front drawterm speaks rcpu/dp9ik):

    drawterm -h <guest-ip> -a <guest-ip> -u glenda

See the vz64 repository's NOTES for the full cpu/auth conversion
recipe, and remember fshalt in the guest before Ctrl-C -- hjfs
buffers writes.

Native graphics (work in progress)
----------------------------------
The -gui flag wires up the host half of a native graphics path,
so a 9front guest could eventually run rio directly in a macOS
window instead of via drawterm.  The split is deliberately
lopsided: the host side is nearly free, the guest side is a
driver port.

Status at a glance:

  [done]    host side: -gui wiring in main.go (builds and signs
            cleanly)

Host side (this repo, done):

  * virtio-gpu device with one scanout (VZ allows at most one),
    sized by -width/-height.  This is the paravirtual GPU from
    the Virtio GPU Device specification -- the same device a
    Linux guest would drive.
  * USB keyboard and USB absolute-coordinate pointing device,
    delivered to the guest as USB HID.
  * a real Cocoa window (VZVirtualMachineView) with a toolbar
    (pause/resume/power/zoom), driven by AppKit's run loop.  In
    -gui mode StartGraphicApplication owns the main OS thread;
    the serial/state loop moves to a goroutine so stdio keeps
    working.

Files
-----
    main.go            the harness (boot loaders, devices, the
                       -gui graphics/input wiring, signal and
                       state loop)
    termios_darwin.go  raw-mode ioctl constants
    vz.entitlements    com.apple.security.virtualization
    check_kernel.sh    arm64 Image header inspector
    Makefile           deps / build+sign / clean (build only --
                       run ./9vz directly)
    HISTORY.md         the original pre-port phase plan
    9front-on-vz.md    additional notes
