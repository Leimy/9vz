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
The Makefile builds and applies an ad-hoc signature.

Dependencies are VENDORED (committed under ./vendor) and pinned
to exact versions in the Makefile (VZ_VERSION, SYS_VERSION).
Normal builds use -mod=vendor, so they read only the committed
tree -- no network, no surprise upgrades:

    make build       # go build -mod=vendor && codesign
    make clean       # remove the binary and efistore.bin

The vendored Code-Hex/vz code is patched locally by
patches/apply.sh (targeted awk edits, not a unified diff) to
disable the view's automaticallyReconfiguresDisplay (needed for a
readable -gui console; see "HiDPI / tiny fonts" below) and to try
to start the window fullscreen (NOT fully working yet; see
"Fullscreen window").  The edits are applied as part of
`make vendor`, and are idempotent.

You only need the steps below when DELIBERATELY changing a
pinned dependency version (edit VZ_VERSION / SYS_VERSION first):

    make deps        # fetch the pinned versions, refresh go.{mod,sum}
    make vendor      # go mod vendor + patches/apply.sh
    make verify      # confirm the vendor tree builds
    # then commit the regenerated ./vendor and patched files

Do NOT use `go get ...@latest`: a surprise bump of Code-Hex/vz
can break the patched window code.  Bump on purpose only.

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
    -width  px      graphics width (default 1440): sets the initial
    -height px      window size AND, divided by -scale, the guest
                    scanout (height default 900).  The window tries
                    to start fullscreen (see "Fullscreen window";
                    not fully working yet), so these mainly fix the
                    display aspect ratio and the
                    guest pixel grid.  The default is a 16:10
                    laptop-native shape (1440x900).  The guest kernel
                    adopts the host scanout size via the virtio-gpu
                    GET_DISPLAY_INFO reply, so it follows these
                    automatically.

    -scale f        HiDPI / font scaling for -gui (default 1).
                    The guest draws 1:1 into the scanout's PIXEL
                    grid and has no notion of display DPI, so on a
                    Retina panel everything (rio's font, the Alpine
                    text console) looks tiny.  -scale shrinks the
                    guest scanout to width/scale x height/scale; the
                    VZVirtualMachineView then upscales each guest
                    pixel.  -scale 2 doubles the on-screen font;
                    -scale 1.5 is a gentler bump.  Values below 1
                    are clamped to 1 (they would only make the font
                    smaller).  Requires the vendored-vz patch that
                    disables automaticallyReconfiguresDisplay -- see
                    "HiDPI / tiny fonts" below.

Mouse buttons / chording on the -gui console
---------------------------------------------
The GUI itself works on every guest tested (9front rio and Linux
both paint, take keyboard, and take single clicks).  The ONE
remaining limitation is multi-button CHORDING on the Plan 9 /
9front native console -- e.g. acme's hold-button-1-add-button-2
to cut, hold-1-add-3 to paste.  Single clicks (buttons 1, 2, 3)
all work; the chord just never forms.

This is NOT a 9vz bug, a 9front bug, or a trackpad limitation.
The only pointing device Apple's Virtualization.framework offers
a non-macOS guest is VZUSBScreenCoordinatePointingDevice, a USB
HID digitizer, and that device DROPS a held button the instant a
second is pressed: a 1->2 chord reports buttons 1 then 2 (never
3 = 1|2), a 1->3 reports 1 then 4 (never 5 = 1|4).  We confirmed
this at three independent layers -- 9front /dev/mouse, Alpine
Linux raw /dev/hidraw bytes (no Plan 9 code in the path), and the
device's own HID report descriptor -- so the held button is gone
before any guest software sees it.  drawterm chords correctly
because it bypasses this device entirely and ships the full
button level itself.

Full writeup, traces, and reproduction:
  https://gist.github.com/Leimy/bc02c6fc56c1a76020139f44496b003a

Consequences:
  * Hardware-button chords on the native -gui console: not
    achievable while Apple's digitizer drops a held button.
  * drawterm remains the proven path for true chords.
  * An earlier host-side CoreGraphics event tap that remapped
    Option/Command-click to buttons 2/3 was tried and ROLLED BACK
    (it never reliably formed the 1-3 paste chord and a
    session-wide tap risks interfering with real hardware mice).
    9vz now passes every click straight through, untouched.
  * The achievable fix is guest-side modifier synthesis
    (Option/Command -> button 2/3 written to /dev/mousein, which
    the kernel ORs with the digitizer's button 1), mirroring
    drawterm's flagsChanged: logic.  Not done here.

HiDPI / tiny fonts on the -gui console
--------------------------------------
On a Retina Mac the -gui window looks fine but the text is
uncomfortably small -- most painfully a Linux guest's (e.g.
Alpine's) framebuffer text console, but also rio's font.

Why it happens.  The guest paints 1:1 into the virtio-gpu
scanout's PIXEL grid.  A guest has no idea the host panel is
HiDPI: it just draws, say, an 8x16 glyph as 8x16 pixels.  The
VZVirtualMachineView presents that scanout in the window, and on
a Retina panel a 1440x900 pixel grid is squeezed into a small
physical area, so every glyph is physically tiny.  There is no
per-display DPI / backing-scale-factor knob that VZ exposes to a
non-macOS guest, and the guest text console has no runtime font
scaler, so we cannot ask either side to "use a 2x font".

The lever that does exist.  We can change how many guest pixels
map onto the window.  -scale f makes the guest render a SMALLER
scanout (width/f x height/f); the host view then stretches each
guest pixel into an f-by-f block.  A glyph the guest still draws
as 8x16 guest-pixels now covers 16x32 screen-pixels at -scale 2
-- twice as big.  The trade-off is fewer guest pixels (less
desktop area) and nearest-neighbour upscaling (slightly blocky),
which is the expected cost of faking HiDPI for a DPI-unaware
guest.

    ./9vz -efi -gui -disk alpine.raw -scale 2     # big, readable
    ./9vz -efi -gui -disk alpine.raw -scale 1.5   # gentler bump

Why an earlier -scale did nothing (and how it was fixed).  Setting
the scanout smaller is necessary but NOT sufficient: upstream
Code-Hex/vz turns on the view's automaticallyReconfiguresDisplay
(macOS 14+), which dynamically resizes the GUEST display to the
view's backing-pixel size and silently overrides the configured
scanout.  So no matter how small a scanout 9vz asked for, the view
immediately told the guest to render at the window's full Retina
pixel count -- the font stayed tiny and -scale appeared to have no
effect.  The vendored vz copy is now patched to set
automaticallyReconfiguresDisplay = NO (by patches/apply.sh), so the
fixed scanout sticks and is upscaled.  This is why dependencies are
vendored: the fix lives in a local edit over the binding.

This is purely a host-side scanout/window-sizing concern; no guest
or kernel change is involved.  For a 9front guest you can
alternatively (or additionally) pick a larger rio font, since rio
is a real graphics environment; -scale helps most for guests whose
console font is fixed (e.g. the Alpine text console).

Fullscreen window
------------------
GOAL: the -gui window should start in a REAL macOS fullscreen --
its own dedicated Space (virtual desktop), menu bar and Dock
hidden -- because the windowed VZ view interacts poorly (title
bar, manual resize, partial-screen pointer mapping).  Combined
with the scanout fix above, the fixed low-res scanout is then
upscaled to fill the screen (big and readable) instead of the
guest being reconfigured back to native Retina density.

STATUS: NOT WORKING YET.  Today the toggle fires but only produces
a big window layered OVER the current desktop (an in-place
"maximize"), not a real fullscreen Space -- a bit odd.  Two earlier
bugs were fixed and ARE in place: the window is made
FullScreenPrimary-eligible (without that collection behavior
-toggleFullScreen: silently no-ops), and the toggle is fired from
applicationDidFinishLaunching AFTER the app is activated (toggling
before the window is key in an active, regular-policy app also
no-ops).  Those got us from "nothing happens" to "wrong thing
happens".

The remaining suspect is VZApplication's hand-rolled
nextEventMatchingMask run loop, which does not drive the native
fullscreen Space transition.  A candidate fix (let AppKit own the
loop via [super run]) is applied in the vendored edit but is
UNTESTED -- it must be verified on the Mac, including that all VM
exit/teardown paths still quit cleanly.  Full analysis and the
fallback options live in the working notes
(/usr/dave/9vz-audio-and-fullscreen.md, section (a)).

To keep a plain window for a single run, set the environment
variable:

    9VZ_NOFULLSCREEN=1 ./9vz -efi -gui -disk alpine.raw

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

A distro Linux arm64 kernel (e.g. Alpine's alpine-vmlinuz-virt,
used for the cross-guest HID test) is usually a small stub plus a
GZIP-compressed Image, which hides the arm64 header so
VZLinuxBootLoader rejects it.  fixer.py finds the gzip stream
inside the vmlinuz and inflates it to the raw Image:

    ./fixer.py alpine-vmlinuz-virt Image-virt   # args optional
    ./check_kernel.sh Image-virt                # should show the header
    ./9vz -kernel Image-virt ...

(With no args it defaults to alpine-vmlinuz-virt -> Image-virt.)
The .raw disk images, the downloaded vmlinuz, and the extracted
Image-virt are build inputs/outputs, not source -- they are kept
out of git by .gitignore; fixer.py (the recipe) is committed.

Getting a useful guest
----------------------
The serial rc prompt works out of the box.  For graphics, run
the guest as a cpu service and connect with drawterm from the
Mac (the 9front drawterm speaks rcpu/dp9ik):

    drawterm -h <guest-ip> -a <guest-ip> -u glenda

See the vz64 repository's NOTES for the full cpu/auth conversion
recipe, and remember fshalt in the guest before Ctrl-C -- hjfs
buffers writes.

Native graphics
---------------
The -gui flag wires up a native graphics path: a 9front (or
Linux) guest runs directly in a macOS window instead of via
drawterm.  This WORKS -- the guest paints, takes keyboard input,
and takes single mouse clicks on every guest tested.

The one outstanding limitation is multi-button mouse CHORDING on
the Plan 9 / 9front native console (see "Mouse buttons / chording
on the -gui console" above): Apple's virtual USB digitizer drops a
held button when a second is pressed, so acme-style chords never
form on the native console.  Everything else about the GUI is
functional; chords still work over drawterm.

Status at a glance:

  [done]    host side: -gui wiring in main.go (builds and signs
            cleanly)
  [done]    guest paints, keyboard works, single clicks work
  [open]    multi-button chording on the native console (Apple
            digitizer limitation; not fixable in 9vz/9front --
            see the chording section and the gist)

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
    fixer.py           extract a bootable Image from a gzip-wrapped
                       distro vmlinuz (e.g. Alpine) for -kernel boot
    Makefile           deps / vendor / verify / build+sign / clean
                       (build only -- run ./9vz directly)
    vendor/            committed, pinned third-party deps; built
                       with -mod=vendor
    patches/apply.sh   local edits applied over ./vendor by
                       `make vendor` (vz view: no display
                       auto-reconfigure; fullscreen-eligibility +
                       post-activation toggle + AppKit-owned run
                       loop -- real fullscreen Space not working
                       yet, see "Fullscreen window")
    HISTORY.md         the original pre-port phase plan
    9front-on-vz.md    additional notes
