# new-vhd: a new dynamic VHDX at $a.path of $a.size_bytes: the team's
# data disk, in the broker's own VM directory, which the caller cannot
# write, so nothing there is a link the caller planted. The broker has
# already deleted a disk it replaces. A creation that fails takes its
# partial file with it.
if (Test-Path -LiteralPath $a.path) { throw "$($a.path) exists" }
try {
    New-VHD -Path $a.path -SizeBytes $a.size_bytes -Dynamic | Out-Null
} catch {
    Remove-Item -LiteralPath $a.path -Force -ErrorAction SilentlyContinue
    throw
}
@{} | ConvertTo-Json -Compress
