# setup-status: whether Hyper-V answers, its Default Switch exists and
# the guest agent's vsock port (1024) is registered as a Hyper-V socket
# service. Get-WindowsOptionalFeature needs an administrator, so the
# role is judged by its module and management service answering.
$hv = $false
if (Get-Command Get-VM -ErrorAction SilentlyContinue) {
    try { Get-VMHost | Out-Null; $hv = $true } catch { $hv = $false }
}
$sw = $false
if ($hv) { $sw = [bool](Get-VMSwitch -Name 'Default Switch' -ErrorAction SilentlyContinue) }
$key = 'HKLM:\SOFTWARE\Microsoft\Windows NT\CurrentVersion\Virtualization\GuestCommunicationServices\00000400-facb-11e6-bd58-64006a7986d3'
$vs = Test-Path -LiteralPath $key
@{ hyperv = $hv; default_switch = $sw; vsock_service = $vs } | ConvertTo-Json -Compress
