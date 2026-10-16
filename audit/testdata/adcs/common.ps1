# Helpers shared by capture-root.ps1 and capture-issuing.ps1.
$ErrorActionPreference = 'Stop'

# DER of the approver identity design's extensions, written out because certreq's text forms for them vary
# by Windows version: the bank EKU 1.3.6.1.4.1.99999.1.1 and the approver policy 1.3.6.1.4.1.99999.2.1.
$EkuHex = '30 0c 06 0a 2b 06 01 04 01 86 8d 1f 01 01'
$PolicyHex = '30 0e 30 0c 06 0a 2b 06 01 04 01 86 8d 1f 02 01'
# basicConstraints cA=TRUE, pathLenConstraint=0
$CaBasicHex = '30 06 01 01 ff 02 01 00'

function Save-Pem([byte[]]$bytes, [string]$label, [string]$path) {
  $b64 = [Convert]::ToBase64String($bytes, 'InsertLineBreaks')
  Set-Content -Path $path -Value ("-----BEGIN $label-----`n$b64`n-----END $label-----") -Encoding ascii
}

function Save-Defaults([string]$caName, [string]$path) {
  $reg = 'HKLM:\SYSTEM\CurrentControlSet\Services\CertSvc\Configuration\' + $caName
  $names = 'CRLPeriod', 'CRLPeriodUnits', 'CRLDeltaPeriod', 'CRLDeltaPeriodUnits', 'CRLOverlapPeriod', 'CRLOverlapUnits', 'CRLFlags', 'ValidityPeriod', 'ValidityPeriodUnits', 'ClockSkewMinutes'
  $lines = foreach ($n in $names) { '{0} = {1}' -f $n, (Get-ItemProperty -Path $reg -Name $n -ErrorAction SilentlyContinue).$n }
  $lines | Set-Content -Path $path
}

# Submit a request to a CA, approve it, and return the issued certificate as an X509Certificate2.
function Get-Issued([string]$config, [string]$req, [string]$cerPath) {
  $submit = certreq -config $config -submit $req 2>&1 | Out-String
  if ($submit -notmatch 'RequestId:\s*(\d+)') { throw "no request id in: $submit" }
  $id = $Matches[1]
  certutil -config $config -resubmit $id | Out-Null
  certreq -config $config -retrieve $id $cerPath | Out-Null
  if (-not (Test-Path $cerPath)) { throw "request $id was not issued" }
  return New-Object System.Security.Cryptography.X509Certificates.X509Certificate2 $cerPath
}
