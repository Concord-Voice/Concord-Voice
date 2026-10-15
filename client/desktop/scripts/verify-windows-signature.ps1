# Verifies the Authenticode signature of every Windows installer the release leg produced.
# Called by the "Verify Windows signature" step of build-desktop.yml. The body below was moved out
# of that step verbatim: Semgrep's protected scan cannot parse PowerShell inside a run: script, and
# any scanner error fails the required check.

$ErrorActionPreference = 'Stop'

$setups = @(Get-ChildItem -Path 'client/desktop/out/make' -Filter '*Setup.exe' -Recurse)
if ($setups.Count -eq 0) {
  Write-Error "::error::No Setup.exe found under out/make"
  exit 1
}
Write-Host "Verifying $($setups.Count) Setup.exe file(s)"
foreach ($exe in $setups) {
  Write-Host "::group::Verifying $($exe.FullName)"
  $sig = Get-AuthenticodeSignature -FilePath $exe.FullName
  if ($sig.Status -ne 'Valid') {
    Write-Error "::error::$($exe.Name) Authenticode status is '$($sig.Status)' (expected 'Valid')"
    exit 1
  }
  # Anchored CN-only match, aligned with electron-updater semantics
  $cn = ($sig.SignerCertificate.Subject -split ',\s*' | Where-Object { $_ -like 'CN=*' }) -replace '^CN=', ''
  if ($cn -ne 'Concord Voice LLC') {
    Write-Error "::error::$($exe.Name) signer CN is '$cn' (expected 'Concord Voice LLC'). Keep in sync with ALLOWED_WINDOWS_PUBLISHERS in src/shared/allowedWindowsPublishers.ts."
    exit 1
  }
  if (-not $sig.TimeStamperCertificate) {
    Write-Error "::error::$($exe.Name) has no RFC 3161 timestamp — signature will become invalid on cert rotation"
    exit 1
  }
  # Issuer pinning: pin to Microsoft Artifact Signing issuer
  $issuerCN = ($sig.SignerCertificate.Issuer -split ',\s*' | Where-Object { $_ -like 'CN=*' }) -replace '^CN=', ''
  if ($issuerCN -notmatch '^Microsoft ID Verified CS ') {
    Write-Error "::error::$($exe.Name) issuer CN is '$issuerCN' (expected 'Microsoft ID Verified CS *'). Cert profile may have changed."
    exit 1
  }
  Write-Host "OK: $($exe.Name) — CN=$cn, issuer=$issuerCN, timestamped"
  Write-Host "::endgroup::"
}
