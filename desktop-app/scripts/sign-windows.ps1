# Signs one file of Kivali Desktop's Windows release. tauri runs this as
# its custom sign command (desktop-app/Makefile, build-bundle) with the
# file's path as the one argument: the supervisor sidecar, Kivali.exe,
# NSIS's plugin DLLs, the uninstaller and the installer. The Windows
# SDK's signtool signs through Azure Artifact Signing: Microsoft's dlib
# fetches a short-lived certificate from the account's certificate
# profile for each signature and timestamps it with Microsoft's
# authority, so the build holds no key and no certificate.
# docs/developers/releasing.md, "Secrets", says where each value comes
# from.
#
#   ARTIFACT_SIGNING_CLIENT    the unpacked Microsoft.ArtifactSigning.Client
#                              package (bin\x64\Azure.CodeSigning.Dlib.dll)
#   ARTIFACT_SIGNING_ENDPOINT  the account's regional endpoint, such as
#                              https://eus.codesigning.azure.net
#   ARTIFACT_SIGNING_ACCOUNT   the Artifact Signing account name
#   ARTIFACT_SIGNING_PROFILE   the certificate profile name
#   AZURE_TENANT_ID, AZURE_CLIENT_ID, AZURE_CLIENT_SECRET
#                              the app registration that holds the
#                              Artifact Signing Certificate Profile Signer
#                              role; the dlib reads them itself
#
# signtool is the newest installed Windows SDK's; the dlib needs
# 10.0.22621 or later.

param([Parameter(Mandatory = $true)][string]$File)

foreach ($name in 'ARTIFACT_SIGNING_CLIENT', 'ARTIFACT_SIGNING_ENDPOINT', 'ARTIFACT_SIGNING_ACCOUNT', 'ARTIFACT_SIGNING_PROFILE', 'AZURE_TENANT_ID', 'AZURE_CLIENT_ID', 'AZURE_CLIENT_SECRET') {
  if (-not [Environment]::GetEnvironmentVariable($name)) {
    Write-Host "sign-windows: $name is not set"
    exit 1
  }
}

$dlib = Join-Path $env:ARTIFACT_SIGNING_CLIENT 'bin\x64\Azure.CodeSigning.Dlib.dll'
if (-not (Test-Path $dlib)) {
  Write-Host "sign-windows: $dlib not found (ARTIFACT_SIGNING_CLIENT is the unpacked Microsoft.ArtifactSigning.Client package)"
  exit 1
}

$kits = Join-Path ${env:ProgramFiles(x86)} 'Windows Kits\10\bin'
$signtool = Get-ChildItem -Path (Join-Path $kits '10.0.*\x64\signtool.exe') -ErrorAction SilentlyContinue |
  Sort-Object { [Version]$_.Directory.Parent.Name } -Descending |
  Select-Object -First 1
if (-not $signtool) {
  Write-Host "sign-windows: no signtool.exe under $kits (a Windows SDK, 10.0.22621 or later)"
  exit 1
}

# The dlib's account, profile and credential choice: the AZURE_*
# variables alone (EnvironmentCredential), so a wrong value fails at
# once instead of falling through to other credential sources.
$metadata = Join-Path ([System.IO.Path]::GetTempPath()) "kivali-artifact-signing-$PID.json"
@{
  Endpoint               = $env:ARTIFACT_SIGNING_ENDPOINT
  CodeSigningAccountName = $env:ARTIFACT_SIGNING_ACCOUNT
  CertificateProfileName = $env:ARTIFACT_SIGNING_PROFILE
  ExcludeCredentials     = @(
    'ManagedIdentityCredential', 'WorkloadIdentityCredential', 'SharedTokenCacheCredential',
    'VisualStudioCredential', 'VisualStudioCodeCredential', 'AzureCliCredential',
    'AzurePowerShellCredential', 'AzureDeveloperCliCredential', 'InteractiveBrowserCredential'
  )
} | ConvertTo-Json | Set-Content -Path $metadata -Encoding ascii

Write-Host "sign-windows: signing $File with $($signtool.FullName)"
& $signtool.FullName sign /fd SHA256 /tr http://timestamp.acs.microsoft.com /td SHA256 /dlib $dlib /dmdf $metadata $File
$code = $LASTEXITCODE
Remove-Item -Path $metadata -ErrorAction SilentlyContinue
if ($code -ne 0) {
  Write-Host "sign-windows: signtool exited with $code"
  exit $code
}

# The signature itself is the check: the certificate lives three days
# and the timestamp is what keeps the signature valid past that.
$signature = Get-AuthenticodeSignature -FilePath $File
if ($signature.Status -ne 'Valid') {
  Write-Host "sign-windows: $File is not signed after signing: $($signature.Status), $($signature.StatusMessage)"
  exit 1
}
Write-Host "sign-windows: signed as $($signature.SignerCertificate.Subject)"
