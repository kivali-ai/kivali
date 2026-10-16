# Signs one file of Kivali Desktop's Windows release. tauri runs this as
# its custom sign command (desktop-app/Makefile, build-bundle) with the
# file's path as the one argument: the supervisor sidecar, Kivali.exe,
# the uninstaller and the installer. SSL.com's CodeSignTool makes the
# signature through the eSigner cloud signing service, timestamped, so
# the key stays in SSL.com's HSM and the build holds no certificate
# file. docs/developers/releasing.md, "Secrets", says where each value
# comes from.
#
#   CODE_SIGN_TOOL_PATH    the unpacked CodeSignTool: the folder holding
#                          CodeSignTool.bat, jar\ and jdk-*\
#   ESIGNER_USERNAME       the SSL.com account
#   ESIGNER_PASSWORD       its password
#   ESIGNER_TOTP_SECRET    the eSigner signing credential's TOTP secret
#   ESIGNER_CREDENTIAL_ID  the credential id; needed only when the
#                          account holds more than one eSigner certificate
#
# CodeSignTool's own Java runtime and jar are run directly rather than
# through CodeSignTool.bat, which passes its arguments through cmd.exe
# unquoted: a password holding & or % would break there.
#
# tauri hands over NSIS's own plugin DLLs as well, copied aside under an
# x86-unicode folder for the installer to embed. Every signature counts
# against the eSigner plan, so those are left as NSIS ships them.

param([Parameter(Mandatory = $true)][string]$File)

if ($File -match '[\\/]x86-unicode[\\/]') {
  Write-Host "sign-windows: NSIS plugin left unsigned: $File"
  exit 0
}

foreach ($name in 'CODE_SIGN_TOOL_PATH', 'ESIGNER_USERNAME', 'ESIGNER_PASSWORD', 'ESIGNER_TOTP_SECRET') {
  if (-not [Environment]::GetEnvironmentVariable($name)) {
    Write-Host "sign-windows: $name is not set"
    exit 1
  }
}

$java = Get-ChildItem -Path (Join-Path $env:CODE_SIGN_TOOL_PATH 'jdk-*\bin\java.exe') | Select-Object -First 1
$jar = Get-ChildItem -Path (Join-Path $env:CODE_SIGN_TOOL_PATH 'jar\code_sign_tool-*.jar') | Select-Object -First 1
if (-not $java -or -not $jar) {
  Write-Host "sign-windows: no CodeSignTool under $env:CODE_SIGN_TOOL_PATH (expected jdk-*\bin\java.exe and jar\code_sign_tool-*.jar)"
  exit 1
}

$arguments = @(
  '-jar', $jar.FullName, 'sign',
  "-username=$env:ESIGNER_USERNAME",
  "-password=$env:ESIGNER_PASSWORD",
  "-totp_secret=$env:ESIGNER_TOTP_SECRET",
  "-input_file_path=$File",
  '-override=true'
)
if ($env:ESIGNER_CREDENTIAL_ID) {
  $arguments += "-credential_id=$env:ESIGNER_CREDENTIAL_ID"
}

Write-Host "sign-windows: signing $File"
& $java.FullName @arguments
if ($LASTEXITCODE -ne 0) {
  Write-Host "sign-windows: CodeSignTool exited with $LASTEXITCODE"
  exit $LASTEXITCODE
}

# CodeSignTool reports some failures in its output alone, so the
# signature itself is the check.
$signature = Get-AuthenticodeSignature -FilePath $File
if ($signature.Status -ne 'Valid') {
  Write-Host "sign-windows: $File is not signed after signing: $($signature.Status), $($signature.StatusMessage)"
  exit 1
}
Write-Host "sign-windows: signed as $($signature.SignerCertificate.Subject)"
