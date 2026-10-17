#!/bin/sh
# Renew an approver's certificate: make a request for the key, have the CA's OIDC login issue the
# certificate, assemble the chain bide approve -cert reads, and check it. Run it when the certificate has
# less than the time you need left (bide approve -cert refuses under -cert-min-left, 60 seconds); an 8 hour
# certificate is renewed once a working day.
#
# The key never leaves where it is held. For a key file the request is made by bide keygen and the key is
# new each time; for a key in an HSM or a KMS, set KEY_REF to its reference and the request is made by
# bide-hsm csr, signed by the key where it is held. No secret is read or printed by this script; the login
# happens in the browser step makes.
#
# Environment (all required unless marked):
#   APPROVER     the approver id in the policy file (ops)
#   POLICY       the certificate policy file bide approve -identity-policy takes
#   PROVISIONER  the name of the CA's OIDC provisioner (sso)
#   OUT          a directory for the files (created, mode 700)
#   ISSUING_CA   the PEM file of the issuing CA certificate, to put behind the leaf
#   PARAMS       the CA timings file bide identity check takes (-params); not needed with SKIP_CHECK
#   EXPECT       the gate's recorded policy (-expect), or NO_EXPECT=1 to compare nothing (partial)
#   CRL_DIR      a bide identity fetch-crl archive for the check, or FETCH=1 to fetch (the CA must be reachable)
#   SKIP_CHECK   (optional) set to skip the pre-flight check, to try the CA before the gate's files exist
#   KEY_ALG      (optional) ecdsa-p256 (default) or ed25519, for a key file
#   KEY_REF      (optional) a bide-hsm key reference: use bide-hsm csr instead of a key file
#   CN           (optional) the placeholder name step ca sign needs in the request (default: $APPROVER)
#   STEP, BIDE, BIDE_HSM  (optional) the commands to run (defaults: step, bide, bide-hsm)
set -eu

: "${APPROVER:?}" "${POLICY:?}" "${PROVISIONER:?}" "${OUT:?}" "${ISSUING_CA:?}"
STEP=${STEP:-step}
BIDE=${BIDE:-bide}
BIDE_HSM=${BIDE_HSM:-bide-hsm}
CN=${CN:-$APPROVER}

umask 077
mkdir -p "$OUT"
cd "$OUT"
stamp=$(date -u +%Y%m%dT%H%M%SZ)
req="$APPROVER-$stamp.csr"
crt="$APPROVER-$stamp.crt"

if [ -n "${KEY_REF:-}" ]; then
	"$BIDE_HSM" csr -key "$KEY_REF" -policy "$POLICY" -as "$APPROVER" -cn "$CN" -out "$req"
	key_flag="-key $KEY_REF"
else
	key="$APPROVER-$stamp.key"
	"$BIDE" keygen -out "$key" -alg "${KEY_ALG:-ecdsa-p256}" -csr "$req" -cn "$CN" >/dev/null
	key_flag="-key $PWD/$key"
fi

# The CA's OIDC provisioner opens the login; the request supplies only the public key.
"$STEP" ca sign --provisioner "$PROVISIONER" "$req" "$crt"
cat "$crt" "$ISSUING_CA" >"$APPROVER-chain.pem"

if [ -z "${SKIP_CHECK:-}" ]; then
	: "${PARAMS:?}"
	if [ -n "${NO_EXPECT:-}" ]; then expect="-no-expect"; else expect="-expect ${EXPECT:?}"; fi
	if [ -n "${FETCH:-}" ]; then status="-fetch"; else status="-crl-dir ${CRL_DIR:?}"; fi
	# shellcheck disable=SC2086
	"$BIDE" identity check -policy "$POLICY" -params "$PARAMS" $expect $status -cert "$APPROVER-chain.pem" -as "$APPROVER"
fi

echo "chain: $PWD/$APPROVER-chain.pem"
echo "use:   bide approve ... -as $APPROVER $key_flag -cert $PWD/$APPROVER-chain.pem -identity-policy $POLICY"
