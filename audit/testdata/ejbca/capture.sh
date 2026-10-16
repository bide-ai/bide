#!/bin/sh
# Captures certificates and CRLs, including true delta CRLs, from EJBCA Community Edition running in a
# container (keyfactor/ejbca-ce). The workflow .github/workflows/crl-fixtures.yml starts the container
# and runs this; it can also be run by hand against any such container:
#
#   docker run -d --name ejbca -p 8080:8080 -e TLS_SETUP_ENABLED=simple keyfactor/ejbca-ce:latest
#   (wait for it to start)  sh capture.sh ejbca ./out
#
# It creates a CA with a one hour CRL period and a ten minute delta period, issues four end-entity
# certificates, and walks them through: a base CRL; key compromise, certificate hold and superseded
# revocations; a delta; the release of the hold; a delta carrying removeFromCRL; a new base; a delta
# against the new base. The CRLs are copied out in PEM with the CA and end-entity certificates, and the
# EJBCA version is recorded. Private keys are not copied.
set -eu
ctr=${1:-ejbca}
out=${2:-out}
ca=${CA_NAME:-FixtureCA}
mkdir -p "$out"
ej() { docker exec "$ctr" /opt/keyfactor/bin/ejbca.sh "$@"; }

ej ca init --caname "$ca" --dn "CN=$ca" --tokenType soft --tokenPass foo123 --keyspec P-256 --keytype EC \
  -s SHA256WithECDSA --policy null -v 3650 >/dev/null
# One hour base CRLs, ten minute deltas, no automatic issuing (the script issues each CRL), entries of
# expired certificates pruned (the default).
ej ca editca --caname "$ca" --field CRLPeriod --value 3600000 >/dev/null
ej ca editca --caname "$ca" --field CRLIssueInterval --value 0 >/dev/null
ej ca editca --caname "$ca" --field CRLOverlapTime --value 600000 >/dev/null
ej ca editca --caname "$ca" --field deltaCRLPeriod --value 600000 >/dev/null

for n in 1 2 3 4; do
  ej ra addendentity --username "$ca-u$n" --password foo123 --dn "CN=u$n" --caname "$ca" --type 1 --token P12 \
    --altname "uri=https://idp.bank.example/person/$(printf '%032x' "$n")" >/dev/null
  ej ra setclearpwd "$ca-u$n" foo123 >/dev/null
done
ej batch >/dev/null

crl() { # name [-delta]
  name=$1; shift
  ej ca createcrl --caname "$ca" "$@" >/dev/null
  ej ca getcrl --caname "$ca" "$@" -pem -f "/tmp/$name.pem" >/dev/null
  docker cp "$ctr:/tmp/$name.pem" "$out/$name.pem"
}

crl base1
ej ra revokeendentity --username "$ca-u1" -r 1 >/dev/null   # keyCompromise
ej ra revokeendentity --username "$ca-u2" -r 6 >/dev/null   # certificateHold
ej ra revokeendentity --username "$ca-u3" -r 4 >/dev/null   # superseded
crl delta1 -delta
crl base2
ej ra unrevokeendentity --username "$ca-u2" >/dev/null      # the hold is released
crl delta2 -delta                                     # carries removeFromCRL for u2
crl base3
crl delta3 -delta

ej ca getcacert --caname "$ca" -f /tmp/ca.pem >/dev/null
docker cp "$ctr:/tmp/ca.pem" "$out/ca.pem"
for n in 1 2 3 4; do
  docker cp "$ctr:/opt/keyfactor/p12/$ca-u$n.p12" "$out/u$n.p12"
  openssl pkcs12 -in "$out/u$n.p12" -passin pass:foo123 -nokeys -clcerts -out "$out/u$n.pem" 2>/dev/null
  rm -f "$out/u$n.p12"
done
docker exec "$ctr" /opt/keyfactor/bin/version.sh > "$out/VERSION" 2>&1 || true
docker inspect --format '{{.Image}}' "$ctr" >> "$out/VERSION"
echo "captured into $out"
