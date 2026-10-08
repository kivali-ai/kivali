# get-vm: the VM named $a.name as the broker's policy needs to see it,
# or {"exists": false}. $a.name is checked (kivali- and 8 hex digits)
# before this runs, so it holds no wildcard. With $a.state_only (a
# boolean), only what a state query needs: no cmdlet per device and no
# Get-VHD per disk, since the backend polls this while the VM runs.
$vm = Get-VM -Name $a.name -ErrorAction SilentlyContinue
if (-not $vm) {
    @{ exists = $false } | ConvertTo-Json -Compress
    return
}
if ($a.state_only -eq $true) {
    @{
        exists             = $true
        id                 = $vm.Id.ToString()
        state              = $vm.State.ToString()
        notes              = [string]$vm.Notes
        memory_assigned_mb = [int64]($vm.MemoryAssigned / 1MB)
    } | ConvertTo-Json -Compress
    return
}
$disks = @(Get-VMHardDiskDrive -VM $vm | ForEach-Object {
    $parent = ''
    if ($_.Path) {
        try { $parent = [string](Get-VHD -Path $_.Path).ParentPath } catch { $parent = '' }
    }
    @{ controller = [int]$_.ControllerNumber; location = [int]$_.ControllerLocation; path = [string]$_.Path; parent = $parent }
})
$mem = Get-VMMemory -VM $vm
$com = Get-VMComPort -VM $vm -Number 1
$nic = @(Get-VMNetworkAdapter -VM $vm) | Select-Object -First 1
@{
    exists             = $true
    id                 = $vm.Id.ToString()
    state              = $vm.State.ToString()
    notes              = [string]$vm.Notes
    path               = [string]$vm.Path
    cpus               = [int]$vm.ProcessorCount
    memory_startup_mb  = [int64]($mem.Startup / 1MB)
    memory_max_mb      = [int64]($mem.Maximum / 1MB)
    memory_assigned_mb = [int64]($vm.MemoryAssigned / 1MB)
    dynamic_memory     = [bool]$mem.DynamicMemoryEnabled
    mac                = [string]$nic.MacAddress
    switch             = [string]$nic.SwitchName
    com_pipe           = [string]$com.Path
    disks              = $disks
} | ConvertTo-Json -Compress -Depth 5
