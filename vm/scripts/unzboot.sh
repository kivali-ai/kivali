#!/bin/sh
# Turns Alpine's /boot/vmlinuz-virt into the kernel image the build
# ships for <arch>, and checks it is a kernel for that architecture.
#
# arm64: the kernel is an EFI_ZBOOT image (a PE image whose header
# names a compressed payload); the payload is unpacked into the raw
# arm64 Image that Virtualization.framework's VZLinuxBootLoader loads.
# zboot header: "MZ" at 0, "zimg" at 4, payload offset (le32) at 8,
# payload size (le32) at 12, compression name at 24.
#
# amd64: the kernel is a bzImage with the EFI stub (EFI_STUB=y, no
# zboot), which the unified kernel image wraps as is; it is copied.
#
# usage: unzboot.sh <vmlinuz> <Image> <arch>
set -eu
in=$1
out=$2
arch=$3

magic=$(dd if="$in" bs=1 skip=4 count=4 2>/dev/null)
if [ "$magic" != "zimg" ]; then
  echo "unzboot: $in is not an EFI_ZBOOT image; copying as is" >&2
  cp "$in" "$out"
else
  off=$(od -A n -t u4 -j 8 -N 4 "$in" | tr -d ' ')
  size=$(od -A n -t u4 -j 12 -N 4 "$in" | tr -d ' ')
  comp=$(dd if="$in" bs=1 skip=24 count=8 2>/dev/null | tr -d '\000')
  if [ "$comp" != "gzip" ]; then
    echo "unzboot: unsupported payload compression '$comp'" >&2
    exit 1
  fi
  tail -c +"$((off + 1))" "$in" | head -c "$size" | gunzip -c > "$out"
fi

case "$arch" in
  arm64)
    # A raw arm64 Image carries "ARM\x64" at offset 0x38.
    sig=$(dd if="$out" bs=1 skip=56 count=3 2>/dev/null)
    want=ARM
    ;;
  amd64)
    # An x86 boot protocol image (bzImage) carries "HdrS" at 0x202.
    sig=$(dd if="$out" bs=1 skip=514 count=4 2>/dev/null)
    want=HdrS
    ;;
  *)
    echo "unzboot: unsupported architecture '$arch'" >&2
    exit 1
    ;;
esac
if [ "$sig" != "$want" ]; then
  echo "unzboot: $out has no $arch kernel magic ('$want')" >&2
  exit 1
fi
echo "unzboot: $arch kernel, $(wc -c < "$out") bytes"
