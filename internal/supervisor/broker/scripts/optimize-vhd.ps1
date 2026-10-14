# optimize-vhd: compact the dynamic VHDX at $a.path, the caller's own
# data disk, checked and pinned by the broker while the VM of its
# directory is off. Quick mode drops the blocks the guest discarded
# (SCSI UNMAP: the guest's fstrim) and moves the rest down, so the file
# holds only what the disk holds; it mounts nothing and scans no block,
# so a disk with no partition table (ext4 over the whole disk) needs
# nothing more. The file's size before and after is the answer.
$before = (Get-Item -LiteralPath $a.path).Length
Optimize-VHD -Path $a.path -Mode Quick
$after = (Get-Item -LiteralPath $a.path).Length
@{ size_before = [int64]$before; size_after = [int64]$after } | ConvertTo-Json -Compress
