# Part one of the AD CS capture: the offline root. Installs a standalone root CA with a certificate
# policy and an RSA-3072 key, then issues the issuing CA's certificate (with the key generated here and
# exported for the second part, which runs on another machine like the issuing CA of a real two-tier
# PKI), and publishes the root's CRL after that issuance. Outputs: root.pem, rootcrl.pem, issuing.pfx
# (the issuing CA's key, for the second part only; never checked in), DEFAULTS-root.txt.
param([string]$Out = 'out')
. (Join-Path $PSScriptRoot 'common.ps1')
New-Item -ItemType Directory -Force -Path $Out | Out-Null
$Out = (Resolve-Path $Out).Path
$rootName = 'Fixture ADCS Root'
$issuingName = 'Fixture ADCS Approver CA'

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
$pfxPassword = 'fixture-' + [Guid]::NewGuid().ToString('N')
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
Exportable = TRUE
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
Export-PfxCertificate -Cert $mine -FilePath (Join-Path $Out 'issuing.pfx') -Password (ConvertTo-SecureString $pfxPassword -AsPlainText -Force) | Out-Null
Set-Content -Path (Join-Path $Out 'pfx-password.txt') -Value $pfxPassword -Encoding ascii
Save-Pem $issuing.RawData 'CERTIFICATE' (Join-Path $Out 'issuing-by-root.pem')
Remove-Item $inf, $req, $cer

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
Write-Host "captured the root into $Out"
