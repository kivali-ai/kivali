# stop-vm: stop the VM $a.name, gracefully through the shutdown
# integration service ($a.graceful), else by turning it off. A VM still
# in a transition (Starting, Stopping, Saving...) refuses the operation
# "in its current state" (the spike hit it on a turn-off right after a
# start); while the VM is in a state that is not settled, the stop is
# retried for up to a minute. The state, not the message (which is
# localised), decides.
$settled = @('Off', 'Running', 'Paused', 'Saved', 'OffCritical', 'RunningCritical', 'PausedCritical', 'SavedCritical')
$deadline = (Get-Date).AddSeconds(60)
while ($true) {
    try {
        if ($a.graceful) {
            Stop-VM -Name $a.name -Confirm:$false
        } else {
            Stop-VM -Name $a.name -TurnOff -Force -Confirm:$false
        }
        break
    } catch {
        $vm = Get-VM -Name $a.name
        if ($vm.State -eq 'Off') { break }
        if ((Get-Date) -gt $deadline -or $settled -contains [string]$vm.State) { throw }
        Start-Sleep -Seconds 2
    }
}
$vm = Get-VM -Name $a.name
@{ id = $vm.Id.ToString(); state = $vm.State.ToString() } | ConvertTo-Json -Compress
