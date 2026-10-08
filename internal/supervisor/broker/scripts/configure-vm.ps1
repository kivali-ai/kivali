# configure-vm: make the VM $a.name exist, off, with exactly this
# configuration. Every path in $a is canonical and was checked by the
# broker as the caller (docs/developers/supervisor.md, "The broker"); the VM
# directory is the broker's own.
$vm = Get-VM -Name $a.name -ErrorAction SilentlyContinue
if (-not $vm) {
    $vm = New-VM -Name $a.name -Generation 2 -NoVHD -Path $a.vm_dir -SwitchName $a.switch
    # The owner first: a VM whose later steps fail is still the caller's
    # to fix or remove.
    Set-VM -VM $vm -Notes $a.notes
}
if ($vm.State -ne 'Off') { throw "$($a.name) is $($vm.State), not Off" }

# The root disk: a differencing child of the release's root.vhdx, made
# again when its parent is another file or no longer matches it.
if (Test-Path -LiteralPath $a.child) {
    $keep = $false
    try {
        $v = Get-VHD -Path $a.child
        $keep = ([string]$v.ParentPath -eq $a.root) -and (Test-VHD -Path $a.child)
    } catch { $keep = $false }
    if (-not $keep) {
        Get-VMHardDiskDrive -VM $vm | Remove-VMHardDiskDrive
        Remove-Item -LiteralPath $a.child -Force
    }
}
if (-not (Test-Path -LiteralPath $a.child)) {
    New-VHD -Path $a.child -ParentPath $a.root -Differencing | Out-Null
}

# Both drives attached again on every configuration, root at SCSI 0:0
# and data at 0:1, so Hyper-V grants the VM's account access to the
# files as they are now (a rollback replaces data.vhdx).
Get-VMHardDiskDrive -VM $vm | Remove-VMHardDiskDrive
Add-VMHardDiskDrive -VM $vm -ControllerType SCSI -ControllerNumber 0 -ControllerLocation 0 -Path $a.child
Add-VMHardDiskDrive -VM $vm -ControllerType SCSI -ControllerNumber 0 -ControllerLocation 1 -Path $a.data
$root = Get-VMHardDiskDrive -VM $vm -ControllerType SCSI -ControllerNumber 0 -ControllerLocation 0
Set-VMFirmware -VM $vm -EnableSecureBoot Off -FirstBootDevice $root

$nic = @(Get-VMNetworkAdapter -VM $vm) | Select-Object -First 1
Connect-VMNetworkAdapter -VMNetworkAdapter $nic -SwitchName $a.switch
Set-VMNetworkAdapter -VMNetworkAdapter $nic -StaticMacAddress $a.mac
Set-VMComPort -VM $vm -Number 1 -Path $a.com_pipe
Set-VMMemory -VM $vm -DynamicMemoryEnabled $true -MinimumBytes 1GB -StartupBytes $a.memory_bytes -MaximumBytes $a.memory_bytes
Set-VMProcessor -VM $vm -Count $a.cpus
Set-VM -VM $vm -CheckpointType Disabled -AutomaticCheckpointsEnabled $false -AutomaticStartAction Nothing -AutomaticStopAction ShutDown -Notes $a.notes

# The shutdown integration service, found by its id (its name is
# localised), so a graceful Stop-VM and the host's shutdown run the
# guest's clean shutdown.
$ic = Get-VMIntegrationService -VM $vm | Where-Object { $_.Id -like '*9F8233AC-BE49-4C79-8EE3-E7E1985B2077' }
if (-not $ic) { $ic = Get-VMIntegrationService -VM $vm -Name 'Shutdown' }
Enable-VMIntegrationService -VMIntegrationService $ic

$vm = Get-VM -Name $a.name
@{ id = $vm.Id.ToString(); state = $vm.State.ToString() } | ConvertTo-Json -Compress
