# Captures a two-tier approver PKI and its CRLs, including true delta CRLs, from Microsoft Active Directory
# Certificate Services on one Windows Server (a machine runs one CA at a time, so the two tiers run one after
# the other). The workflow .github/workflows/crl-fixtures.yml runs this on windows-2025; it can be run by
# hand on any Windows Server in an elevated PowerShell. The fixtures are checked in so the tests run offline.
#
# Phase one is the offline root: a standalone root CA (RSA-3072, SHA-256, a certificate policy from
# CAPolicy.inf) issues the issuing CA's certificate for a key generated on this machine (the key never
# leaves it and is never exported), then publishes its CRL. The root CA is then uninstalled. Phase two
# installs the issuing CA, a standalone subordinate (an enterprise CA and its templates need an Active
# Directory domain, which a CI runner does not have; a standalone CA takes the same extensions from the
# request), on that certificate and key. It issues four approver certificates (ECDSA P-256, the bank EKU, the
# approver policy, a URI subject alternative name, 24 hours) and walks them through a base CRL; revocations
# (key compromise, certificate hold, superseded); a delta; the release of the hold; a delta; a new base; a
# delta. Only certificates, CRLs and settings are written to the output; no private key or password.
param([string]$Out = 'out')
. (Join-Path $PSScriptRoot 'common.ps1')
New-Item -ItemType Directory -Force -Path $Out | Out-Null
$Out = (Resolve-Path $Out).Path
$rootName = 'Fixture ADCS Root'
$issuingName = 'Fixture ADCS Approver CA'

# ---- Phase one: the offline root ----

# The root asserts the approver policy, so the issuing CA's policy nests under it.
@"
[Version]
Signature="`$Windows NT`$"
[PolicyStatementExtension]
Policies=ApproverPolicy
[ApproverPolicy]
OID=1.3.6.1.4.1.99999.2.1
"@ | Set-Content -Path (Join-Path $env:SystemRoot 'CAPolicy.inf') -Encoding ascii

Install-WindowsFeature ADCS-Cert-Authority -IncludeManagementTools | Out-Null
Install-AdcsCertificationAuthority -CAType StandaloneRootCa -CACommonName $rootName `
  -CryptoProviderName 'RSA#Microsoft Software Key Storage Provider' -KeyLength 3072 -HashAlgorithmName SHA256 `
  -ValidityPeriod Years -ValidityPeriodUnits 10 -Force | Out-Null
Save-Defaults $rootName (Join-Path $Out 'DEFAULTS-root.txt')
# A standalone CA takes the request's extensions only when told to.
certutil -setreg policy\EditFlags +EDITF_ENABLEREQUESTEXTENSIONS | Out-Null
Restart-Service certsvc
Start-Sleep -Seconds 10
$config = "$env:COMPUTERNAME\$rootName"

# The issuing CA's request: a CA key, basicConstraints cA pathLen 0, keyCertSign and cRLSign, the policy.
$inf = Join-Path $Out 'issuing.inf'
@"
[Version]
Signature="`$Windows NT`$"
[NewRequest]
Subject = "CN=$issuingName"
KeyAlgorithm = RSA
KeyLength = 3072
HashAlgorithm = SHA256
ProviderName = "Microsoft Software Key Storage Provider"
RequestType = PKCS10
MachineKeySet = TRUE
KeyUsage = 0x86
[Extensions]
2.5.29.19 = "{hex}"
_continue_ = "$CaBasicHex"
2.5.29.32 = "{hex}"
_continue_ = "$PolicyHex"
Critical = 2.5.29.19
"@ | Set-Content -Path $inf -Encoding ascii
$req = Join-Path $Out 'issuing.req'
certreq -new -f $inf $req | Out-Null
$cer = Join-Path $Out 'issuing.cer'
$issuing = Get-Issued $config $req $cer
certreq -accept $cer | Out-Null
$mine = Get-ChildItem Cert:\LocalMachine\My | Where-Object { $_.Subject -eq "CN=$issuingName" } | Select-Object -First 1
if (-not $mine) { throw 'the issued CA certificate did not bind to its key' }
$issuingThumb = $mine.Thumbprint
Save-Pem $issuing.RawData 'CERTIFICATE' (Join-Path $Out 'issuing-by-root.pem')
Remove-Item $inf, $req, $cer
Remove-Item (Join-Path $env:SystemRoot 'CAPolicy.inf')

# The root's CRL, published after it issued the CA certificate (no entries).
$enroll = Join-Path $env:SystemRoot 'System32\CertSrv\CertEnroll'
certutil -config $config -CRL | Out-Null
Start-Sleep -Seconds 2
Save-Pem ([IO.File]::ReadAllBytes((Join-Path $enroll "$rootName.crl"))) 'X509 CRL' (Join-Path $Out 'rootcrl.pem')
$caFile = Get-ChildItem -Path $enroll -Filter '*.crt' | Select-Object -First 1
if (-not $caFile) { throw "no CA certificate in $enroll" }
$root = New-Object System.Security.Cryptography.X509Certificates.X509Certificate2 $caFile.FullName
Save-Pem $root.RawData 'CERTIFICATE' (Join-Path $Out 'root.pem')
Copy-Item $caFile.FullName (Join-Path $Out 'root.cer')
certutil -dump (Join-Path $Out 'issuing-by-root.pem') | Set-Content -Path (Join-Path $Out 'issuing-dump.txt')
(Get-ComputerInfo -Property OsName, OsVersion | Format-List | Out-String) | Set-Content -Path (Join-Path $Out 'VERSION-root.txt')

# ---- Between the phases: the root CA is removed so that this machine can host the issuing CA ----
Uninstall-AdcsCertificationAuthority -Force | Out-Null
Uninstall-WindowsFeature ADCS-Cert-Authority | Out-Null
Remove-Item -Recurse -Force (Join-Path $env:SystemRoot 'System32\CertLog\*') -ErrorAction SilentlyContinue
Remove-Item -Force (Join-Path $env:SystemRoot 'System32\CertSrv\CertEnroll\*') -ErrorAction SilentlyContinue
Install-WindowsFeature ADCS-Cert-Authority -IncludeManagementTools | Out-Null

# ---- Phase two: the issuing CA ----
Import-Certificate -FilePath (Join-Path $Out 'root.cer') -CertStoreLocation Cert:\LocalMachine\Root | Out-Null
# The root's CRL, where chain building on this machine finds it (the certificates' CRL distribution point
# is a file:// path on the root's machine).
certutil -addstore -f Root (Join-Path $Out 'rootcrl.pem') | Out-Null
Install-AdcsCertificationAuthority -CAType StandaloneSubordinateCa -CertificateID $issuingThumb -Force | Out-Null
Save-Defaults $issuingName (Join-Path $Out 'DEFAULTS.txt')
certutil -getreg CA\CSP | Set-Content -Path (Join-Path $Out 'csp.txt')
# An offline root's revocation server is not reachable from here: the CA ignores an offline CRL.
certutil -setreg CA\CRLFlags +CRLF_REVCHECK_IGNORE_OFFLINE | Out-Null

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
  certutil -verify (Join-Path $Out 'issuing-by-root.pem') | Write-Host
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
KeyAlgorithm = ECDSA_P256
KeyLength = 256
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
  if ($cert.SignatureAlgorithm.FriendlyName -match 'sha1') { throw "the CA signed with $($cert.SignatureAlgorithm.FriendlyName)" }
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
Remove-Item (Join-Path $Out 'root.cer')
Write-Host "captured into $Out"
