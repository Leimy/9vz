#!/bin/sh
# check_kernel.sh — does this kernel carry the Linux arm64 Image header?
# VZLinuxBootLoader will only load images with the magic "ARM\x64" at
# byte offset 56. The 9front Raspberry Pi kernels mimic this header so
# the Pi firmware will boot them; this script tells us whether the
# virt/qemu kernel does too.
#
# usage: ./check_kernel.sh /path/to/kernel
set -e
f="$1"
[ -n "$f" ] && [ -r "$f" ] || { echo "usage: $0 kernelfile" >&2; exit 2; }

magic=$(dd if="$f" bs=1 skip=56 count=4 2>/dev/null | xxd -p)
echo "magic @56:   0x$magic (want 41524d64 = \"ARM\\x64\")"

hexle() { # little-endian u64 at offset $1
	dd if="$f" bs=1 skip="$1" count=8 2>/dev/null | xxd -p |
	sed 's/../& /g' | awk '{ for (i=NF; i>0; i--) printf "%s", $i; print "" }'
}

if [ "$magic" = "41524d64" ]; then
	echo "text_offset: 0x$(hexle 8)"
	echo "image_size:  0x$(hexle 16)"
	echo "flags:       0x$(hexle 24)"
	echo
	echo "VERDICT: has the arm64 Image header — VZLinuxBootLoader should load it."
else
	echo
	echo "VERDICT: no arm64 Image header — direct boot will be rejected."
	echo "Options: prepend a header (64 bytes, doc: linux/Documentation/arch/arm64/booting.rst),"
	echo "or fall back to -efi + U-Boot."
fi
