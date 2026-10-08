# new-vhd: a new dynamic VHDX at $a.path of $a.size_bytes. $a.path is
# in the broker's own VM directory, which the caller cannot write, so
# nothing there is a link the caller planted; a file left there by an
# earlier attempt that failed is replaced. The broker then moves the
# disk into the caller's directory and grants the caller full control
# of it, in Go, through a handle on the moved file (Files.PlaceFile).
# A creation that fails takes its partial file with it, so the VM
# directory of a team that never got a VM holds nothing the caller
# cannot delete.
if (Test-Path -LiteralPath $a.path) { Remove-Item -LiteralPath $a.path -Force }
try {
    New-VHD -Path $a.path -SizeBytes $a.size_bytes -Dynamic | Out-Null
} catch {
    Remove-Item -LiteralPath $a.path -Force -ErrorAction SilentlyContinue
    throw
}
@{} | ConvertTo-Json -Compress
