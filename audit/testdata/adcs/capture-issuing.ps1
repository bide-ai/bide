# Part two of the AD CS capture: the issuing CA, a standalone subordinate installed from the PFX the root
# part produced (an enterprise CA and its certificate templates need a domain, which a CI runner does not
# have; a standalone CA takes the same extensions from the request). It issues four approver
# certificates with the bank EKU, the approver policy and a URI subject alternative name, and walks
# them through a base CRL; revocations (key compromise, certificate hold, superseded); a delta; the
# release of the hold; a delta; a new base; a delta. Validity is 24 hours; the CA's default clock skew
# back-dates notBefore. Private keys stay on the machine.
param([string]$Out = 'out', [string]$In = 'root')
. (Join-Path $PSScriptRoot 'common.ps1')
New-Item -ItemType Directory -Force -Path $Out | Out-Null
$Out = (Resolve-Path $Out).Path
$In = (Resolve-Path $In).Path
$issuingName = 'Fixture ADCS Approver CA'

Import-Certificate -FilePath (Join-Path $In 'root.cer') -CertStoreLocation Cert:\LocalMachine\Root | Out-Null
# The root's CRL, where chain building on this machine finds it (the certificates' CRL distribution point
# is a file:// path on the root's machine).
certutil -addstore -f Root (Join-Path $In 'rootcrl.pem') | Out-Null
$pw = ConvertTo-SecureString (Get-Content (Join-Path $In 'pfx-password.txt')).Trim() -AsPlainText -Force
Install-WindowsFeature ADCS-Cert-Authority -IncludeManagementTools | Out-Null
Install-AdcsCertificationAuthority -CAType StandaloneSubordinateCa -CertFile (Join-Path $In 'issuing.pfx') -CertFilePassword $pw -Force | Out-Null
# An offline root's revocation server is not reachable from here: the CA ignores an offline CRL.
certutil -setreg CA\CRLFlags +CRLF_REVCHECK_IGNORE_OFFLINE | Out-Null
Save-Defaults $issuingName (Join-Path $Out 'DEFAULTS.txt')

# 24 hour certificates; a base CRL every day and a delta every 30 minutes; request extensions honoured.
certutil -setreg CA\ValidityPeriod 'Hours' | Out-Null
certutil -setreg CA\ValidityPeriodUnits 24 | Out-Null
certutil -setreg CA\CRLPeriod 'Days' | Out-Null
certutil -setreg CA\CRLPeriodUnits 1 | Out-Null
certutil -setreg CA\CRLDeltaPeriod 'Minutes' | Out-Null
certutil -setreg CA\CRLDeltaPeriodUnits 30 | Out-Null
certutil -setreg policy\EditFlags +EDITF_ENABLEREQUESTEXTENSIONS | Out-Null
try { Restart-Service certsvc } catch {
  Get-WinEvent -LogName Application -MaxEvents 30 -ErrorAction SilentlyContinue | Where-Object { $_.ProviderName -match 'CertSvc|Certificate' } | Format-List TimeCreated, Message | Out-String | Write-Host
  certutil -verify (Join-Path $In 'issuing-by-root.pem') | Write-Host
  throw
}
Start-Sleep -Seconds 10
$config = "$env:COMPUTERNAME\$issuingName"

$serials = @{}
foreach ($n in 1..4) {
  $uri = 'https://idp.bank.example/person/{0:x32}' -f $n
  $inf = Join-Path $Out "u$n.inf"
  @"
[Version]
Signature="`$Windows NT`$"
[NewRequest]
Subject = "CN=u$n"
KeyAlgorithm = RSA
KeyLength = 2048
HashAlgorithm = SHA256
RequestType = PKCS10
MachineKeySet = TRUE
KeyUsage = 0x80
[Extensions]
2.5.29.17 = "{text}"
_continue_ = "URL=$uri"
2.5.29.32 = "{hex}"
_continue_ = "$PolicyHex"
2.5.29.37 = "{hex}"
_continue_ = "$EkuHex"
"@ | Set-Content -Path $inf -Encoding ascii
  $req = Join-Path $Out "u$n.req"
  certreq -new -f $inf $req | Out-Null
  $cer = Join-Path $Out "u$n.cer"
  $cert = Get-Issued $config $req $cer
  $serials[$n] = $cert.SerialNumber
  Save-Pem $cert.RawData 'CERTIFICATE' (Join-Path $Out "u$n.pem")
  Remove-Item $inf, $req, $cer
  Remove-Item (Join-Path $Out "u$n.rsp") -ErrorAction SilentlyContinue
}

$enroll = Join-Path $env:SystemRoot 'System32\CertSrv\CertEnroll'
$caFile = Get-ChildItem -Path $enroll -Filter '*.crt' | Select-Object -First 1
if (-not $caFile) { throw "no CA certificate in $enroll" }
$ca = New-Object System.Security.Cryptography.X509Certificates.X509Certificate2 $caFile.FullName
Save-Pem $ca.RawData 'CERTIFICATE' (Join-Path $Out 'ca.pem')
Copy-Item (Join-Path $In 'root.pem') (Join-Path $Out 'root.pem')
Copy-Item (Join-Path $In 'rootcrl.pem') (Join-Path $Out 'rootcrl.pem')

function Publish-Crl([string]$name, [bool]$delta) {
  if ($delta) { certutil -config $config -CRL delta | Out-Null } else { certutil -config $config -CRL | Out-Null }
  Start-Sleep -Seconds 2
  $file = if ($delta) { "$issuingName+.crl" } else { "$issuingName.crl" }
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

certutil -dump (Join-Path $Out 'u1.pem') | Set-Content -Path (Join-Path $Out 'leaf-dump.txt')
(Get-ComputerInfo -Property OsName, OsVersion | Format-List | Out-String) | Set-Content -Path (Join-Path $Out 'VERSION')
Write-Host "captured into $Out"
