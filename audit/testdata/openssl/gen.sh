#!/bin/sh
# Regenerates the fixtures in this directory with OpenSSL: a root, an issuing CA and approver leaves
# issued with `openssl ca`, and the CRLs `openssl ca -gencrl` writes for them. The CRLs are real
# CA-software output (CRL number, authority key identifier, reason codes, invalidity dates, a hold,
# a compromise with and without a date), which is what TestOpenSSLFixtures reads. Private keys are
# generated into a temporary directory and discarded; only certificates and CRLs are kept.
#
# Usage: sh gen.sh   (needs openssl on PATH; the test does not run it)
set -eu
export SAN="URI:https://idp.bank.example/person/00000000000000000000000000000000"
out=$(cd "$(dirname "$0")" && pwd)
w=$(mktemp -d)
trap 'rm -rf "$w"' EXIT
cd "$w"
mkdir -p root issuing
: > root/index.txt; : > issuing/index.txt
echo 01 > root/serial; echo 01 > issuing/serial
echo 01 > issuing/crlnumber; echo 01 > root/crlnumber

cat > root.cnf <<CNF
[ ca ]
default_ca = CA_default
[ CA_default ]
dir = ./root
database = ./root/index.txt
new_certs_dir = ./root
serial = ./root/serial
crlnumber = ./root/crlnumber
default_md = sha256
policy = pol
unique_subject = no
copy_extensions = none
[ pol ]
commonName = supplied
[ v3_issuing ]
basicConstraints = critical,CA:TRUE,pathlen:0
keyUsage = critical,keyCertSign,cRLSign
subjectKeyIdentifier = hash
authorityKeyIdentifier = keyid
certificatePolicies = 1.3.6.1.4.1.99999.2.1
[ crl_ext ]
authorityKeyIdentifier = keyid
CNF

cat > issuing.cnf <<CNF
[ ca ]
default_ca = CA_default
[ CA_default ]
dir = ./issuing
database = ./issuing/index.txt
new_certs_dir = ./issuing
serial = ./issuing/serial
crlnumber = ./issuing/crlnumber
default_md = sha256
policy = pol
unique_subject = no
copy_extensions = none
[ pol ]
commonName = supplied
[ v3_leaf ]
basicConstraints = critical,CA:FALSE
keyUsage = critical,digitalSignature
extendedKeyUsage = 1.3.6.1.4.1.99999.1.1
certificatePolicies = 1.3.6.1.4.1.99999.2.1
authorityKeyIdentifier = keyid
subjectAltName = \${ENV::SAN}
[ crl_ext ]
authorityKeyIdentifier = keyid
[ crl_expired ]
authorityKeyIdentifier = keyid
2.5.29.60 = ASN1:GENERALIZEDTIME:20261001000000Z
CNF

# Root (self-signed) and issuing CA (signed by the root), valid around the fixtures' time.
openssl ecparam -name prime256v1 -genkey -noout -out root.key
openssl ecparam -name prime256v1 -genkey -noout -out issuing.key
openssl req -new -x509 -key root.key -sha256 -subj "/CN=Fixture Root" -set_serial 1 \
  -not_before 20260901000000Z -not_after 20270901000000Z -out root.pem \
  -addext "basicConstraints=critical,CA:TRUE" -addext "keyUsage=critical,keyCertSign,cRLSign" -addext "subjectKeyIdentifier=hash"
openssl req -new -key issuing.key -subj "/CN=Fixture Issuing" -out issuing.csr
openssl ca -config root.cnf -batch -notext -cert root.pem -keyfile root.key -in issuing.csr -out issuing.pem \
  -extensions v3_issuing -startdate 20260901000000Z -enddate 20270901000000Z

# Leaves: three approvers' certificates valid for 24 hours around 2026-10-01 12:00 UTC.
mkleaf() { # name person-number
  openssl ecparam -name prime256v1 -genkey -noout -out "$1.key"
  openssl req -new -key "$1.key" -subj "/CN=$1" -out "$1.csr"
  SAN="URI:https://idp.bank.example/person/$(printf '%032x' "$2")" \
  openssl ca -config issuing.cnf -batch -notext -cert issuing.pem -keyfile issuing.key -in "$1.csr" -out "$1.pem" \
    -extensions v3_leaf -startdate 20261001000000Z -enddate 20261002000000Z
}
mkleaf good 1
mkleaf compromised 2
mkleaf held 3
mkleaf superseded 4
mkleaf nodate 5
# A short-lived approver certificate (11:00 to 13:00 UTC) that is revoked after the decisions, below.
openssl ecparam -name prime256v1 -genkey -noout -out late.key
openssl req -new -key late.key -subj "/CN=late" -out late.csr
SAN="URI:https://idp.bank.example/person/$(printf '%032x' 6)" \
openssl ca -config issuing.cnf -batch -notext -cert issuing.pem -keyfile issuing.key -in late.csr -out late.pem \
  -extensions v3_leaf -startdate 20261001110000Z -enddate 20261001130000Z
mkleaf rem 7

# Revocations, as the CA records them.
openssl ca -config issuing.cnf -cert issuing.pem -keyfile issuing.key -revoke compromised.pem -crl_compromise 20261001113000Z
openssl ca -config issuing.cnf -cert issuing.pem -keyfile issuing.key -revoke held.pem -crl_reason certificateHold
openssl ca -config issuing.cnf -cert issuing.pem -keyfile issuing.key -revoke nodate.pem -crl_reason keyCompromise
openssl ca -config issuing.cnf -cert issuing.pem -keyfile issuing.key -revoke superseded.pem -crl_reason superseded

# OpenSSL stamps the revocation time with the wall clock; set the times the fixtures describe in the
# CA database (columns: status, expiry, revocation[,reason[,invalidity]], serial, ..., subject).
settimes() { # serial=time ...
  awk -F'\t' -v OFS='\t' -v spec="$*" '
    BEGIN { n = split(spec, kv, " "); for (i = 1; i <= n; i++) { split(kv[i], p, "="); t[p[1]] = p[2] } }
    $1 == "R" && ($4 in t) { split($3, f, ","); f[1] = t[$4]; $3 = f[1]; for (i = 2; i in f; i++) $3 = $3 "," f[i] }
    { print }' issuing/index.txt > issuing/index.new && mv issuing/index.new issuing/index.txt
}
settimes 02=261001113000Z 03=261001114000Z 04=261001114500Z 05=261001113500Z

# The CA issues a CRL every 30 minutes and each is valid for 24 hours, so the CRLs overlap (a
# predecessor's nextUpdate reaches far beyond its successor's thisUpdate), as real CAs do.
gen() { # lastupdate nextupdate extensions out [config]
  openssl ca -config "${5:-issuing.cnf}" -cert issuing.pem -keyfile issuing.key -gencrl -crl_lastupdate "$1" -crl_nextupdate "$2" -crlexts "$3" -out "$4"
}
gen 20261001115000Z 20261002115000Z crl_ext crl1.pem
gen 20261001122000Z 20261002122000Z crl_ext crl2.pem
gen 20261001125000Z 20261002125000Z crl_ext crl3.pem

# The short-lived certificate is revoked after the decisions (a key compromise reported at 12:55 with no
# invalidity date: retroactive to notBefore). The first CRL beyond its notAfter (13:00) lists it; a CA
# may then prune the entry (RFC 5280 section 3.3), simulated by taking it out of the database for the
# next CRL.
openssl ca -config issuing.cnf -cert issuing.pem -keyfile issuing.key -revoke late.pem -crl_reason keyCompromise
settimes 06=261001125500Z
gen 20261001131000Z 20261002131000Z crl_ext crl4.pem
cp issuing/index.txt issuing/index.full
grep -v '^R.*	06	' issuing/index.txt > issuing/index.new && mv issuing/index.new issuing/index.txt
gen 20261001134000Z 20261002134000Z crl_ext crl5.pem
# A CRL that keeps the entries of expired certificates (expiredCertsOnCRL), issued the next morning.
cp issuing/index.full issuing/index.txt
gen 20261002010000Z 20261003010000Z crl_expired crl6.pem

# Input a verifier must refuse, written by the same software: a SHA-1 signature, an issuing
# distribution point (a partitioned CRL), removeFromCRL in a full CRL, and a CRL that carries a
# critical delta CRL indicator (OpenSSL cannot compute a true delta, so it is only the encoding).
openssl ca -config issuing.cnf -cert issuing.pem -keyfile issuing.key -gencrl -md sha1 -crl_lastupdate 20261001115000Z -crl_nextupdate 20261002115000Z \
  -crlexts crl_ext -out bad-sha1.pem 2>/dev/null || echo "openssl would not write a SHA-1 CRL; bad-sha1.pem skipped" >&2
cat >> issuing.cnf <<'CNF'
[ crl_idp ]
authorityKeyIdentifier = keyid
issuingDistributionPoint = critical,@idp
[ idp ]
fullname = URI:http://crl.bank.example/partition1.crl
[ crl_pseudodelta ]
authorityKeyIdentifier = keyid
2.5.29.27 = critical,ASN1:INTEGER:1
CNF
gen 20261001115000Z 20261002115000Z crl_idp bad-idp.pem
gen 20261001115000Z 20261002115000Z crl_pseudodelta pseudo-delta.pem
# removeFromCRL in a full CRL (it belongs only in a delta). OpenSSL always writes version 2, so a
# version 1 CRL is built by hand in the tests.
openssl ca -config issuing.cnf -cert issuing.pem -keyfile issuing.key -revoke rem.pem -crl_reason removeFromCRL
settimes 07=261001114000Z
gen 20261001115000Z 20261002115000Z crl_ext bad-removefromcrl.pem

# The root's CRL for the issuing CA (it revokes nothing).
openssl ca -config root.cnf -cert root.pem -keyfile root.key -gencrl -crl_lastupdate 20261001115000Z -crl_nextupdate 20261008115000Z \
  -crlexts crl_ext -out rootcrl.pem

for f in root issuing good compromised held superseded nodate late; do openssl x509 -in "$f.pem" -outform pem -out "$out/$f.pem"; done
for f in crl1 crl2 crl3 crl4 crl5 crl6 rootcrl bad-idp pseudo-delta bad-removefromcrl; do cp "$f.pem" "$out/$f.pem"; done
[ -f bad-sha1.pem ] && cp bad-sha1.pem "$out/bad-sha1.pem"
echo "fixtures written to $out"
