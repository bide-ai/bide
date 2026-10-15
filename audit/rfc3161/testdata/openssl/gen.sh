#!/bin/sh
# Regenerates the OpenSSL test TSA vectors. Needs OpenSSL 1.1.1 or newer with the `ts` command (not
# LibreSSL). The vectors are checked in; run this only to add or replace one. Each vector is a
# root CA, a TSA certificate (critical id-kp-timeStamping as its only EKU) and a request/response
# pair for the fixed data "bide rfc3161 test vector 1".
set -eu
cd "$(dirname "$0")"
printf 'bide rfc3161 test vector 1' > data.txt

cat > tsa.cnf <<CNF
oid_section = new_oids
[ new_oids ]
tsa_policy1 = 1.2.3.4.1
[ tsa ]
default_tsa = tsa_config1
[ tsa_config1 ]
serial = ./serial
crypto_device = builtin
signer_cert = ./tsa-\$ENV::VEC.pem
certs = ./tsa-chain-\$ENV::VEC.pem
signer_key = ./tsa-\$ENV::VEC.key
signer_digest = \$ENV::DIGEST
default_policy = tsa_policy1
other_policies = tsa_policy1
digests = sha256, sha384, sha512
accuracy = \$ENV::ACC
ordering = no
tsa_name = no
ess_cert_id_chain = no
ess_cert_id_alg = sha256
[ ext_root ]
basicConstraints = critical, CA:TRUE
keyUsage = critical, keyCertSign, cRLSign
subjectKeyIdentifier = hash
[ ext_tsa ]
basicConstraints = CA:FALSE
keyUsage = critical, digitalSignature
extendedKeyUsage = critical, timeStamping
subjectKeyIdentifier = hash
authorityKeyIdentifier = keyid
CNF

gen() { # name keyspec digest accuracy
  VEC=$1; DIGEST=$3; ACC=$4; export VEC DIGEST ACC
  case "$2" in
    ec256) openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256 -out root-$1.key 2>/dev/null
           openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256 -out tsa-$1.key 2>/dev/null ;;
    ec384) openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-384 -out root-$1.key 2>/dev/null
           openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-384 -out tsa-$1.key 2>/dev/null ;;
    rsa3072) openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:3072 -out root-$1.key 2>/dev/null
             openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:3072 -out tsa-$1.key 2>/dev/null ;;
  esac
  openssl req -new -x509 -key root-$1.key -subj "/O=bide test/CN=bide test TSA root $1" -days 36500 \
    -extensions ext_root -config tsa.cnf -out root-$1.pem -$3 2>/dev/null
  openssl req -new -key tsa-$1.key -subj "/O=bide test/CN=bide test TSA $1" -out tsa-$1.csr -config tsa.cnf
  openssl x509 -req -in tsa-$1.csr -CA root-$1.pem -CAkey root-$1.key -CAcreateserial -days 36500 \
    -extfile tsa.cnf -extensions ext_tsa -out tsa-$1.pem -$3 2>/dev/null
  cat tsa-$1.pem root-$1.pem > tsa-chain-$1.pem
  echo 01 > serial
  openssl ts -query -data data.txt -sha256 -cert -out req-$1.tsq
  openssl ts -reply -config tsa.cnf -section tsa_config1 -queryfile req-$1.tsq -out resp-$1.tsr
  openssl ts -reply -in resp-$1.tsr -out token-$1.tok -token_out
  rm -f tsa-$1.csr root-$1.srl root-$1.key tsa-$1.key
}
gen ec256 ec256 sha256 secs:1
gen ec384 ec384 sha384 secs:1
gen rsa3072 rsa3072 sha256 secs:1
gen ec256wide ec256 sha256 secs:1,millisecs:500
rm -f serial tsa-chain-*.pem
