# start-vm: start the VM $a.name.
Start-VM -Name $a.name
$vm = Get-VM -Name $a.name
@{ id = $vm.Id.ToString(); state = $vm.State.ToString(); memory_assigned_mb = [int64]($vm.MemoryAssigned / 1MB) } | ConvertTo-Json -Compress
