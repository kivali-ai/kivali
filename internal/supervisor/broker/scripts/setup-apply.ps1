# setup-apply: register the guest agent's vsock port (1024) as a
# Hyper-V socket service, when it is not. The key is under HKLM, which
# only an administrator writes: `broker install` (elevated) registers
# it, and the service, which is no administrator, can only say so when
# it is missing (an elevated console broker still writes it). Fixed key,
# fixed value; nothing comes from the caller.
$key = 'HKLM:\SOFTWARE\Microsoft\Windows NT\CurrentVersion\Virtualization\GuestCommunicationServices\00000400-facb-11e6-bd58-64006a7986d3'
if (-not (Test-Path -LiteralPath $key)) {
    try {
        New-Item -Path $key -Force | Out-Null
        New-ItemProperty -LiteralPath $key -Name 'ElementName' -Value 'Kivali guest agent (vsock 1024)' -PropertyType String -Force | Out-Null
    } catch {
        throw "the guest agent's Hyper-V socket service is not registered, and only an administrator can register it: run `kivali-supervisor broker install` (or reinstall Kivali)"
    }
}
@{} | ConvertTo-Json -Compress
