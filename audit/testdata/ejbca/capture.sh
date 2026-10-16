#!/bin/sh
# Captures a two-tier approver PKI and its CRLs, including true delta CRLs, from EJBCA Community
# Edition running in a container (keyfactor/ejbca-ce). The workflow .github/workflows/crl-fixtures.yml
# starts the container and runs this; it can also be run by hand against any such container:
#
#   docker run -d --name ejbca -p 8080:8080 -e TLS_SETUP_ENABLED=simple keyfactor/ejbca-ce:latest
#   (wait for it to start)  sh capture.sh ejbca ./out
#
# The PKI is the one the approver identity design asks of a bank: an offline RSA-3072 root
# ("FixtureRoot") and an ECDSA P-256 issuing CA ("FixtureApprover") under it, both asserting the approver
# policy OID, and an "Approver" certificate profile (the bank EKU, the policy, digitalSignature, 24 hours,
# EJBCA's default ten minute notBefore offset; see ProfileGen.java) with a URI subject alternative name.
# The issuing CA has a one hour CRL period and a ten minute delta period. Four approver certificates are
# issued and walked through: a base CRL; key compromise, certificate hold and superseded revocations;
# a delta; the release of the hold; a delta carrying removeFromCRL; a new base; a delta against the new
# base. (The script waits eleven minutes after creating the CAs; see below.) The root's CRL (for the issuing CA, no entries) is captured too. The CRLs are copied out in PEM
# with the CA and end-entity certificates, and the EJBCA version is recorded. Private keys are not copied.
set -eu
ctr=${1:-ejbca}
out=${2:-out}
root=FixtureRoot
ca=FixtureApprover
here=$(cd "$(dirname "$0")" && pwd)
mkdir -p "$out"
ej() { docker exec "$ctr" /opt/keyfactor/bin/ejbca.sh "$@"; }
lib='/opt/keyfactor/ejbca/dist/ejbca-ejb-cli/lib/*'
policy=1.3.6.1.4.1.99999.2.1

# The Approver certificate profile and end entity profile, written by EJBCA's own classes.
docker cp "$here/ProfileGen.java" "$ctr:/tmp/ProfileGen.java"
docker exec "$ctr" sh -c "rm -rf /tmp/pimp && mkdir /tmp/pimp && cd /tmp && java -cp '$lib' ProfileGen.java /tmp/pimp/certprofile_Approver-1001.xml /tmp/pimp/entityprofile_Approver-2001.xml 1001" >/dev/null
ej ca importprofiles -d /tmp/pimp >/dev/null

ej ca init --caname "$root" --dn "CN=$root" --tokenType soft --tokenPass foo123 --keyspec 3072 --keytype RSA \
  -s SHA256WithRSA --policy "$policy" -v 3650 >/dev/null
rootid=$(ej ca info --caname "$root" | sed -n 's/^.*CA ID: //p')
ej ca init --caname "$ca" --dn "CN=$ca" --tokenType soft --tokenPass foo123 --keyspec P-256 --keytype EC \
  -s SHA256WithECDSA --policy "$policy" -v 1825 --signedby "$rootid" >/dev/null
# One hour base CRLs, ten minute deltas, no automatic issuing (the script issues each CRL), entries of
# expired certificates pruned (the default).
ej ca editca --caname "$ca" --field CRLPeriod --value 3600000 >/dev/null
ej ca editca --caname "$ca" --field CRLIssueInterval --value 0 >/dev/null
ej ca editca --caname "$ca" --field CRLOverlapTime --value 600000 >/dev/null
ej ca editca --caname "$ca" --field deltaCRLPeriod --value 600000 >/dev/null

# EJBCA stamps a certificate's notBefore ten minutes before issuing it (the profile's offset) but never
# earlier than the issuing CA's own notBefore, so a CA created seconds ago would clamp it. Wait until the
# issuing CA is older than the offset, so the captured certificates show the CA's real behaviour.
sleep "${CA_AGE_SECONDS:-660}"

# The approvers generate their own ECDSA P-256 keys (the key stays in a temporary directory, never in the
# output) and EJBCA issues from their certificate requests.
keys=$(mktemp -d)
trap 'rm -rf "$keys"' EXIT
for n in 1 2 3 4; do
  ej ra addendentity --username "$ca-u$n" --password foo123 --dn "CN=u$n" --caname "$ca" --type 1 --token USERGENERATED \
    --eeprofile Approver --certprofile Approver \
    --altname "uri=https://idp.bank.example/person/$(printf '%032x' "$n")" >/dev/null
  openssl ecparam -name prime256v1 -genkey -noout -out "$keys/u$n.key"
  openssl req -new -key "$keys/u$n.key" -subj "/CN=u$n" -out "$keys/u$n.csr"
  docker cp "$keys/u$n.csr" "$ctr:/tmp/u$n.csr"
  ej createcert "$ca-u$n" foo123 "/tmp/u$n.csr" "/tmp/u$n.pem" >/dev/null
  docker cp "$ctr:/tmp/u$n.pem" "$out/u$n.pem"
done

crl() { # name caname [-delta]
  name=$1; cn=$2; shift 2
  ej ca createcrl --caname "$cn" "$@" >/dev/null
  ej ca getcrl --caname "$cn" "$@" -pem -f "/tmp/$name.pem" >/dev/null
  docker cp "$ctr:/tmp/$name.pem" "$out/$name.pem"
}

crl rootcrl "$root"
crl base1 "$ca"
ej ra revokeendentity --username "$ca-u1" -r 1 >/dev/null   # keyCompromise
ej ra revokeendentity --username "$ca-u2" -r 6 >/dev/null   # certificateHold
ej ra revokeendentity --username "$ca-u3" -r 4 >/dev/null   # superseded
crl delta1 "$ca" -delta
crl base2 "$ca"
ej ra unrevokeendentity --username "$ca-u2" >/dev/null      # the hold is released
crl delta2 "$ca" -delta                                     # carries removeFromCRL for u2
crl base3 "$ca"
crl delta3 "$ca" -delta

for pair in "root:$root" "ca:$ca"; do
  ej ca getcacert --caname "${pair#*:}" -f "/tmp/${pair%%:*}.pem" >/dev/null
  docker cp "$ctr:/tmp/${pair%%:*}.pem" "$out/${pair%%:*}.pem"
done
docker exec "$ctr" /opt/keyfactor/bin/version.sh > "$out/VERSION" 2>&1 || true
docker inspect --format '{{.Image}}' "$ctr" >> "$out/VERSION"
echo "captured into $out"
