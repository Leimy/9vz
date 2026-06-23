# 9front on Apple Virtualization.framework

## A port log

*June 2026 — Dave, with Claude (Fable 5 and Opus 4.6)*

## What this is

This project produced a working port of the 9front kernel that boots as a
first-class guest virtual machine under Apple's Virtualization.framework on
Apple Silicon, plus a small Go program (`9vz`) that hosts it. The guest boots
to a serial console, brings up networking, runs a CPU listener, and accepts
drawterm connections from the host. As far as a reasonable search could
establish, nobody had done this before: 9front has run on Macs under QEMU
and UTM for years, but never directly under Apple's own VM framework.

The work splits into three artifacts: a host-side harness (`9vz`, ~250 lines
of Go), a kernel port (`sys/src/9/vz64`, a modified copy of the existing
arm64 qemu-virt port), and one genuinely new driver (`uartvz.c`, a
virtio-console UART for Plan 9 — the first one in the tree).

## Which virtualization is this, exactly

Apple ships two layers, and the names invite confusion:

**Hypervisor.framework** is the low-level layer — roughly Apple's KVM. It
exposes raw CPU virtualization: create vCPUs, map guest physical memory,
trap exits. It provides no devices, no bootloader, no machine model. QEMU's
"hvf" accelerator sits on this; when you run `qemu-system-aarch64 -accel
hvf`, QEMU provides the entire machine (its "virt" board, PL011 UART, GIC,
virtio devices) and Hypervisor.framework just makes the CPU fast.

**Virtualization.framework** ("VZ") is the high-level layer, built on top of
Hypervisor.framework. It is Apple's complete machine model: it provides the
bootloader, the device tree, virtio-pci devices (block, net, console,
entropy, vsock, fs), a GICv3 interrupt controller, and a PL031 RTC. This is
what Apple's `container` project, UTM's "Apple Virtualization" backend,
Lima, and Tart use. It is fast, minimal, and opinionated — and its opinions
are all shaped by one assumption: the guest is Linux (or macOS).

**This project runs 9front under Virtualization.framework** — the high
layer — using the Code-Hex/vz Go bindings rather than Swift. The
significance of choosing VZ over QEMU+HVF is that the guest now runs on
Apple's own lightweight machine model with no emulator process, the same
substrate Apple's container stack uses — which matters for where this
project goes next (see "What's next").

The friction, and therefore the entire story of this port, comes from VZ's
Linux assumption. The existing 9front arm64 port targets QEMU's virt board.
VZ's machine looks similar from a distance — arm64, virtio, GIC — and
differs in every load-bearing detail.

## The hardware contract

We established VZ's actual machine model empirically, by booting Alpine
Linux under our own harness and reading `/proc/iomem`,
`/proc/device-tree`, and `/proc/interrupts`. That survey produced the
contract the port is written against:

| Resource        | QEMU virt (old port)        | Virtualization.framework      |
|-----------------|------------------------------|-------------------------------|
| RAM base        | 0x4000_0000                  | 0x7000_0000                   |
| Kernel load     | 0x4010_0000                  | 0x7010_0000                   |
| DTB             | fixed at RAM base            | pointer in x0, placed high in RAM |
| UART            | PL011 at 0x0900_0000         | **none — no UART exists at all** |
| Console         | PL011                        | virtio-console PCI (0x1AF4:0x1043) |
| GIC             | v3 at 0x0800_0000 (GICR +0xa0000) | v3: GICD 0x1000_0000, GICR 0x1001_0000 |
| RTC             | PL031                        | PL031 at 0x2005_0000          |
| PCI ECAM        | 0x3F00_0000                  | 0x4000_0000                   |
| PCI mem32 window| 0x1000_0000                  | 0x5000_0000–0x6FFD_FFFF       |
| PCI INTx        | 4 shared lines, swizzled     | per-slot: slot *d* → SPI 32+*d* |
| INTL config reg | written by QEMU              | **garbage — no firmware writes it** |

Two of those rows are the port's defining problems. VZ has no UART of any
kind, so a Plan 9 kernel — whose console is a UART by deep assumption — has
no way to speak until a virtio-console driver exists. And the PCI Interrupt
Line register is uninitialized garbage because direct kernel boot means no
firmware ever runs to program it; drivers that trust `pcidev->intl` (all of
them) get nonsense.

## The kernel port (vz64)

The port copies `sys/src/9/arm64` and changes the following.

**Memory layout.** The arm64 port encodes physical addresses through a
single elegant identity: PA = VA − KZERO, used by the early assembly, the
page-table builders, and KADDR/PADDR. You don't retarget the RAM base by
changing a constant — you slide the kernel's virtual window so the identity
lands where the RAM actually is. We moved KZERO from 0xFFFFFFFF_80000000 to
0xFFFFFFFF_00000000, putting VDRAM at 0xFFFFFFFF_70000000 (PA 0x7000_0000),
the device window at KZERO+PHYSIO so the identity holds for MMIO too, and
the vmap arena in the top 256MB of the address space. Consequence: the
guest is capped at 2GB RAM until someone rearranges the top of the window.

**DTB delivery.** The old port read its device tree from a fixed address —
the start of RAM, where QEMU puts it. VZ passes a DTB pointer in x0 per the
Linux boot protocol and places the blob near the *top* of RAM. The entry
code already saved x0; the port threads it through `main(uintptr dtb)` into
`bootargsinit(dtb)`. Because the DTB lives outside the region the early MMU
setup mapped, `mmu0init` now maps all of RAM up front instead of just the
kernel image. A pleasant side effect: the harness's `-cmdline` flag lands in
the DTB's /chosen/bootargs, which `bootargsinit` parses as plan9.ini — so
the boot configuration travels with the boot command.

**Interrupts and devices.** The GICv3 driver needed only a redistributor
rebase. The RTC is the same PL031 silicon at a new address. The PCI code
got VZ's ECAM and BAR window, and two interrupt changes: route per-slot
SPIs per VZ's interrupt-map, and — because no firmware exists — overwrite
every device's bogus `intl` field after the bus scan with the correct
SPI 32+slot value, which transparently fixes every driver that trusts it
(sdvirtio10, ethervirtio10, and the new console driver alike).

**The console driver (`uartvz.c`).** The genuinely new code: a
virtio-console driver presenting as a Plan 9 UART. It runs in two phases.
At `uartconsinit` time — before PCI exists — it installs a null UART whose
output lands in a 16KB ring buffer. Later, during `links()`, after the PCI
scan, `uartvzlink()` probes for the virtio-console device, performs the
virtio 1.0 handshake (PCI capability walk, feature negotiation, rx/tx
virtqueue setup), switches the console to real I/O, and flushes the ring
buffer — so the entire early boot log arrives in one burst the moment the
console comes alive. TX is polled (synchronous, panic-safe); RX is
interrupt-driven into the normal kernel input queue. On the host side, VZ
bridges this device to the harness's stdin/stdout, so the terminal running
`9vz` simply is the machine's console.

**The self-describing image.** The kernel's first 64 bytes are now a Linux
arm64 Image header, embedded directly in `l.s` exactly the way Linux's
head.S does it: code0 is a branch over the header, the magic "ARM\x64"
sits at offset 56, and VZLinuxBootLoader accepts the file as-is. The build
produces `9vz.u` (a uImage, still bootable by U-Boot under QEMU); stripping
the 64-byte uImage wrapper with `dd` yields the VZ-bootable image directly.

## The harness (9vz)

A single-file Go program over the Code-Hex/vz bindings. It configures a
VZLinuxBootLoader (kernel, optional initrd, command line) or optionally EFI
firmware, attaches a raw disk as virtio-blk, NAT virtio-net, entropy, a
vsock device (idle for now, reserved for the future control plane), and a
virtio-console serial port wired to the terminal in raw mode. Dependencies
(the Code-Hex/vz bindings and x/sys) are vendored and pinned, so the build is
`go build -mod=vendor` plus `codesign` with the
`com.apple.security.virtualization` entitlement -- all wrapped by `make build`.
The vendored vz bindings carry local edits (applied by patches/apply.sh
during `make vendor`): the VZVirtualMachineView's automaticallyReconfiguresDisplay
is turned off so the `-scale` HiDPI scanout actually sticks, and an attempt
to make the window start in a real fullscreen Space. The fullscreen attempt
is NOT WORKING YET -- today the toggle only produces a maximized window over
the current desktop, not a dedicated fullscreen Space; see the README
("Fullscreen window") and the working notes
(/usr/dave/9vz-audio-and-fullscreen.md, section (a)). Ctrl-C requests a stop;
twice forces exit.

The same harness boots stock Linux — which is not a side feature but a
debugging instrument: it's how the hardware survey was done, and it remains
the fastest way to interrogate VZ's machine model when something disagrees.

## The debugging war: how it actually went

The port compiled quickly and then hung in total silence, and the path from
there to a login prompt is the part worth remembering.

**Silence is ambiguous.** A VM with no console reports only three states:
running, stopped, error — and "running" describes both a healthy kernel and
a CPU spinning through recursive exception loops with no vectors installed.
The first breakthrough was converting those three states into a telemetry
channel: tiny hand-assembled bare-metal probes (no toolchain — instructions
encoded by hand in a Python script) that signal by *choosing* a state.
PSCI SYSTEM_OFF over HVC produces "stopped"; a deliberate load from
unmapped PA 0 produces "error"; a WFE spin stays "running". Three probes
established that code executes, PSCI works over both conduits, and the
image loads where expected. The same trick went into the kernel as
`vzstop()` — a one-line flare that turned "it hangs" into a binary search.

**The DTB fault.** First real bug found by bisection: reading the DTB at
its new top-of-RAM location faulted because early boot only mapped the
kernel image — and the fault happened *before* `trapinit()`, so no vectors,
recursive abort, silent spin. Fixed by mapping all of RAM in `mmu0init`.

**The off-by-64.** The deep one. The original workflow wrapped the kernel
with an external tool that *prepended* the 64-byte arm64 header. VZ loads
the file at base + text_offset — so the header landed at the kernel's
linked address and every actual kernel byte sat 64 bytes later than the
linker believed. PC-relative code ran perfectly; every absolute data access
(Plan 9 addresses globals through SB) read its neighbor's bytes. The
symptoms were spectacular and incoherent: a lock variable "containing
garbage" (it was reading the adjacent string constant), a targeted
zero-this-one-variable hack that worked (it zeroed through the same shifted
address all other code used — self-consistent), and a bulk
zero-the-whole-data-segment attempt that made things worse (it wiped
shifted *initialized* data — fmt tables, strings). A parallel debugging
session attributed the lock failures to LSE atomics and exclusive-monitor
behavior; the real cause was beneath all of it. The disciplined notes from
that session — especially the paradox that targeted zeroing worked where
bulk zeroing didn't — are what made the off-by-64 the only surviving
theory. A placement probe then proved VZ rejects unaligned text_offsets,
killing the wrap-side fix and forcing the correct one: embed the header in
the image so file layout and link layout are the same thing.

The general lesson, suitable for framing: when a non-relocatable kernel
boots under a loader built for relocatable ones, *prove where you are
loaded before believing anything else*, and when symptoms look like memory
corruption with self-consistent workarounds, suspect that all addresses are
wrong by a constant rather than that memory is broken.

## Current state and rough edges

Working: boot to console, disk (virtio-blk root filesystem), networking
(virtio-net, DHCP), interactive serial console with the full early boot log
delivered retroactively, CPU listener, drawterm in from the host.

Rough edges, in rough priority order: the harness traps Ctrl-C as "stop the
VM" rather than passing it through (Plan 9's interrupt key is DEL, which
mostly hides this); the console getc path busy-waits (fine for rdb, not
pretty); guest RAM is capped at 2GB by the address-space layout.

SMP works up to 10 CPU cores on my M5 Macbook Air.

The -gui native graphics path works (the guest paints, takes keyboard, and
takes single mouse clicks).  The one unresolved limitation is multi-button
mouse CHORDING on the native console: Apple's only non-macOS pointing device
is a USB digitizer that drops a held button the instant a second is pressed,
so acme-style cut/paste chords never form there.  This was traced to the
device at three layers across two guest OSes (it is not a 9vz or 9front bug);
drawterm still chords correctly because it bypasses the device.  Full writeup:
https://gist.github.com/Leimy/bc02c6fc56c1a76020139f44496b003a  (see also the
README "Mouse buttons / chording" section).

## Native graphics

The harness grew a `-gui` mode that attaches a virtio-gpu device (one
scanout), a USB keyboard and a USB pointing device, and hands the VM to
AppKit's run loop so it appears in a real macOS window — all from the
Code-Hex/vz bindings, which already ship the Cocoa `VZVirtualMachineView`
and a `StartGraphicApplication` entry point. The serial console keeps
flowing on stdio in parallel. This is the host half of letting a 9front
guest run rio natively instead of over drawterm.

The split is lopsided in the host's favor. The Mac side is wiring, done.
The guest side is a driver port, which is working pretty well now.

## What's next

The original motivation, before this became a porting epic: Apple's
`container` project runs each Linux container in a lightweight VZ VM with a
guest agent speaking gRPC over vsock — a design that is one
9P-shaped-realization away from elegance. The harness already attaches a
vsock device. The natural next move is a guest-side service exporting a 9P
filesystem over virtio-vsock — control plane as namespace, processes
spawned by writing to ctl files, the host mounting the guest — which would
make 9front guests composable infrastructure on macOS rather than a
curiosity. Given existing work structuring agent capabilities as 9P
services, the pieces are already on the bench.

And independently of all that: a `uartvirtio10` console driver and a VZ
machine port are upstream-worthy contributions to 9front. The mailing list
would plausibly enjoy the screenshot.

## State of graphics (mostly done)

Native graphics is up: the vz64 virtio-gpu driver paints, and `/dev/kbd` and
`/dev/mouse` work for keyboard and single clicks.  The host path (`-gui` in
`main.go`) plus the guest driver were validated against both Linux and 9front
guests.  The virtio 1.0 handshake in the guest GPU driver was cribbed from
`uartvz.c`, as planned.

What remains, in priority order:

1. Multi-button CHORDING on the native console -- the one open item.  This is
   an Apple Virtualization.framework limitation (the virtual USB digitizer
   drops a held button when a second is pressed), not a missing driver; it is
   not fixable in 9vz or 9front.  See the "Native graphics" /
   "Current state" notes above and the gist.  The achievable workaround is
   guest-side modifier synthesis (Option/Command -> button 2/3 into
   /dev/mousein), not yet implemented.
2. HiDPI ergonomics: handled host-side via `-scale` (smaller scanout,
   upscaled); see the README.  No guest change needed.  A real fullscreen
   Space to go with it is attempted but NOT WORKING YET (it only maximizes a
   window over the current desktop); see the README "Fullscreen window" and
   the working notes (/usr/dave/9vz-audio-and-fullscreen.md, section (a)).

Note: a stray `-lobjc` duplicate-library linker *warning* during the build
comes from the upstream bindings' cgo LDFLAGS, not this tree; it is harmless.
