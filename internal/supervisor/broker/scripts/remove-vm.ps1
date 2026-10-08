# remove-vm: remove the VM $a.name (turned off first) and then its
# differencing child $a.child, which the broker checked is its own
# ("" leaves it). Never the data disk. The broker removes its VM
# directory itself afterwards, in Go (Files.RemoveVMDir).
$vm = Get-VM -Name $a.name
if ($vm.State -ne 'Off') { Stop-VM -VM $vm -TurnOff -Force -Confirm:$false }
Remove-VM -VM $vm -Force
if ($a.child -and (Test-Path -LiteralPath $a.child)) {
    Remove-Item -LiteralPath $a.child -Force
}
@{} | ConvertTo-Json -Compress
