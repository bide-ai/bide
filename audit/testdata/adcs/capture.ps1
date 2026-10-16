# Captures certificates and CRLs, including true delta CRLs, from Microsoft Active Directory Certificate
# Services running as a standalone root CA on a Windows Server. The workflow
# .github/workflows/crl-fixtures.yml runs this on windows-latest; it can be run by hand on any Windows
# Server in an elevated PowerShell. The fixtures are checked in so the tests run offline.
#
# It installs the role, creates the CA with the default publication settings of a standalone root CA
# changed only to switch delta CRLs on, issues four certificates, and walks them through a base CRL;
# revocations (key compromise, certificate hold, superseded); a delta; the release of the hold; a delta;
# a new base; a delta. It records the registry values that govern CRL publication and the CA's
# CRL-related defaults, so what the fixtures reflect is on record. Private keys stay on the machine.
param([string]$Out = 'out')

$ErrorActionPreference = 'Stop'
New-Item -ItemType Directory -Force -Path $Out | Out-Null
$Out = (Resolve-Path $Out).Path
$caName = 'Fixture ADCS Root'

Install-WindowsFeature ADCS-Cert-Authority -IncludeManagementTools | Out-Null
Install-AdcsCertificationAuthority -CAType StandaloneRootCa -CACommonName $caName `
  -CryptoProviderName 'ECDSA_P256#Microsoft Software Key Storage Provider' -KeyLength 256 -HashAlgorithmName SHA256 `
  -ValidityPeriod Years -ValidityPeriodUnits 5 -Force | Out-Null

# Record the defaults of a standalone root CA before touching anything.
$reg = 'HKLM:\SYSTEM\CurrentControlSet\Services\CertSvc\Configuration\' + $caName
$names = 'CRLPeriod', 'CRLPeriodUnits', 'CRLDeltaPeriod', 'CRLDeltaPeriodUnits', 'CRLOverlapPeriod', 'CRLOverlapUnits', 'CRLFlags', 'ValidityPeriod', 'ValidityPeriodUnits'
$defaults = foreach ($n in $names) { '{0} = {1}' -f $n, (Get-ItemProperty -Path $reg -Name $n).$n }
$defaults | Set-Content -Path (Join-Path $Out 'DEFAULTS.txt')

# Turn delta CRLs on: a base CRL every day, a delta every 30 minutes.
certutil -setreg CA\CRLPeriod 'Days' | Out-Null
certutil -setreg CA\CRLPeriodUnits 1 | Out-Null
certutil -setreg CA\CRLDeltaPeriod 'Minutes' | Out-Null
certutil -setreg CA\CRLDeltaPeriodUnits 30 | Out-Null
Restart-Service certsvc
Start-Sleep -Seconds 10
$config = "$env:COMPUTERNAME\$caName"

function Save-Pem([byte[]]$bytes, [string]$label, [string]$path) {
  $b64 = [Convert]::ToBase64String($bytes, 'InsertLineBreaks')
  Set-Content -Path $path -Value ("-----BEGIN $label-----`n$b64`n-----END $label-----") -Encoding ascii
}

# Four requests, approved and retrieved.
$serials = @{}
foreach ($n in 1..4) {
  $inf = Join-Path $Out "u$n.inf"
  @"
[NewRequest]
Subject = "CN=u$n"
KeyAlgorithm = ECDSA_P256
KeyLength = 256
RequestType = PKCS10
MachineKeySet = TRUE
"@ | Set-Content -Path $inf
  $req = Join-Path $Out "u$n.req"
  certreq -new -f $inf $req | Out-Null
  $submit = certreq -config $config -submit $req 2>&1 | Out-String
  if ($submit -notmatch 'RequestId:\s*(\d+)') { throw "no request id in: $submit" }
  $id = $Matches[1]
  certutil -config $config -resubmit $id | Out-Null
  $cer = Join-Path $Out "u$n.cer"
  certreq -config $config -retrieve $id $cer | Out-Null
  $cert = New-Object System.Security.Cryptography.X509Certificates.X509Certificate2 $cer
  $serials[$n] = $cert.SerialNumber
  Save-Pem $cert.RawData 'CERTIFICATE' (Join-Path $Out "u$n.pem")
  Remove-Item $inf, $req, $cer
}

# The CA certificate.
$caCer = Join-Path $Out 'ca.cer'
certutil -config $config -ca.cert $caCer | Out-Null
$ca = New-Object System.Security.Cryptography.X509Certificates.X509Certificate2 $caCer
Save-Pem $ca.RawData 'CERTIFICATE' (Join-Path $Out 'ca.pem')
Remove-Item $caCer

$enroll = Join-Path $env:SystemRoot 'System32\CertSrv\CertEnroll'
function Publish-Crl([string]$name, [bool]$delta) {
  if ($delta) { certutil -config $config -CRL delta | Out-Null } else { certutil -config $config -CRL | Out-Null }
  Start-Sleep -Seconds 2
  $file = if ($delta) { "$caName+.crl" } else { "$caName.crl" }
  $bytes = [IO.File]::ReadAllBytes((Join-Path $enroll $file))
  Save-Pem $bytes 'X509 CRL' (Join-Path $Out "$name.pem")
}

Publish-Crl 'base1' $false
certutil -config $config -revoke $serials[1] 1 | Out-Null   # keyCompromise
certutil -config $config -revoke $serials[2] 6 | Out-Null   # certificateHold
certutil -config $config -revoke $serials[3] 4 | Out-Null   # superseded
Publish-Crl 'delta1' $true
Publish-Crl 'base2' $false
certutil -config $config -revoke $serials[2] unrevoke | Out-Null   # the hold is released
Publish-Crl 'delta2' $true
Publish-Crl 'base3' $false
Publish-Crl 'delta3' $true

(Get-ComputerInfo -Property OsName, OsVersion | Format-List | Out-String) | Set-Content -Path (Join-Path $Out 'VERSION')
Write-Host "captured into $Out"
