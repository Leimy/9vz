# Historical note

This is the original planning document for 9vz, written before any
of it worked -- when "does a 9front kernel boot under
Virtualization.framework at all" was an open question.  It is
preserved unedited for the record; the predictions aged
interestingly (the console risk was real, the ECAM fear mostly
wasn't, and "boots to a prompt: pour something nice" happened).
For current documentation see README.md.

---

# 9vz — 9front under Apple Virtualization.framework

An experiment: boot 9front/arm64 as a guest under macOS's native
Virtualization.framework (the layer beneath Apple's `container` project),
using the Code-Hex/vz Go bindings. As far as we can tell, nobody has done
this. End goal: 9front guests as first-class lightweight VMs on Apple
Silicon, with a 9P-over-vsock control plane.

## Prerequisites (on the Mac)

    xcode-select --install        # Apple CLT (clang, codesign)
    brew install go qemu          # qemu for the baseline + qemu-img

## Phase 0 — baseline (known-good QEMU boot)

Before touching VZ, confirm the 9front arm64 image boots at all on this
machine via the canonical path: QEMU + HVF + U-Boot, per 9front FQA 3.3.1
(http://fqa.9front.org/fqa3.html).

1. Get the arm64 qcow2 from http://9front.org/iso/ (gunzip it).
2. Build U-Boot for qemu_arm64 (needs an aarch64 cross toolchain; easiest
   inside any Linux container/VM — `make qemu_arm64_defconfig && make`).
3. Boot roughly:

       qemu-system-aarch64 -M virt,accel=hvf -cpu host -smp 2 -m 4096 \
         -bios u-boot.bin \
         -device virtio-net-pci-non-transitional,netdev=net0 \
         -netdev user,id=net0,hostfwd=tcp::17019-:17019,hostfwd=tcp::17567-:567 \
         -drive if=none,id=vd0,file=9front.arm64.qcow2 \
         -device virtio-blk-pci-non-transitional,drive=vd0 \
         -nographic -serial stdio

Serial console only; graphics later via drawterm.

## Phase 0.5 — extract and inspect the kernel

VZ's direct-boot loader needs the kernel as a separate file.

    qemu-img convert -f qcow2 -O raw 9front.arm64.qcow2 9front.raw
    hdiutil attach -imagekey diskimage-class=CRawDiskImage 9front.raw

Mount the FAT (9fat/dos) partition, copy out the kernel (the big file
next to plan9.ini; note plan9.ini's contents — that's our bootargs
reference), then:

    ./check_kernel.sh ./<kernelfile>

If it reports the arm64 Image header, Phase 1 is live. If not, we either
prepend the 64-byte header ourselves or jump to Phase 2.

## Phase 1 — direct kernel boot under VZ

    make deps          # once
    make build
    ./9vz -kernel ./<kernelfile> -disk ./9front.raw -cmdline ''

Watch stderr/serial. Outcomes:

- **Silence**: see Known risks #1 (console) before concluding the kernel
  didn't boot. Run `make run-efi DISK=9front.raw` — if the EDK2 firmware
  banner appears on your terminal, serial plumbing works and the silence
  is the guest's.
- **Early prints, then panic**: jackpot — that's a device-discovery
  mismatch we can chase in 9front kernel source (DTB parsing, ECAM
  placement, virtio probing).
- **Boots to a prompt**: pour something nice and screenshot it for the
  9front mailing list.

## Phase 2 — fallback: EFI + U-Boot

If direct boot is rejected or unworkable: VZEFIBootLoader runs EDK2,
which can chain-load U-Boot built as an EFI payload, recreating the
QEMU boot chain. `-efi -efistore efistore.bin` is already wired up.

## Known risks

1. **Console.** VZ exposes only virtio-console serial devices — there is
   no PL011 UART like QEMU's virt machine provides. If the 9front virt
   kernel only speaks PL011, it may boot fine and say nothing. Likely
   first kernel patch: a virtio-console driver or early-print path.
2. **Device tree differences.** VZ generates its own DTB; 9front's virt
   kernel was written against QEMU's. ECAM placement is a documented
   sore spot even across QEMU versions (FQA notes a highmem-ecam
   workaround), so expect PCIe probe issues.
3. **Bootargs delivery.** Direct boot passes the command line via DTB
   /chosen; 9front normally reads plan9.ini from the FAT partition.
   Unclear which the virt kernel honors — plan9.ini on disk may carry us.

## Files

    main.go            VZ harness (direct-kernel + EFI boot paths)
    termios_darwin.go  raw-mode ioctl constants
    check_kernel.sh    arm64 Image header inspector
    vz.entitlements    com.apple.security.virtualization (codesign needs it)
    Makefile           deps / build+sign / run / run-efi

## Later

- vsock device is already attached: the hook for a 9P control plane —
  guest exports a 9P fs over virtio-vsock, host mounts it. The whole
  point, eventually.
- virtio-fs shares, virtio-gpu (9front has no driver; drawterm instead).
