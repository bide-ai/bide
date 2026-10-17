{{- /*
  The certificate step-ca issues to an approver after an OIDC login. Everything bide's IdentityPolicy
  looks for is decided here, from the verified ID token (.Token) and never from the request:
  - one SAN, a person URI built from the token's person_id claim, which must be 32 lowercase hex digits;
  - the login must have used MFA (the amr claim holds "mfa"), or nothing is issued;
  - key usage digitalSignature only, and the bank's approver EKU only (no serverAuth, no clientAuth);
  - the bank's certificate policy OID, asserted only by this template;
  - the CRL distribution point that the CRL's issuing distribution point names.
  Edit the five values marked CHANGE for your bank; the test in this directory uses the values below.
*/ -}}
{{- $id := .Token.person_id -}}
{{- if not (regexMatch "^[0-9a-f]{32}$" $id) }}{{ fail "the person_id claim is not 32 lowercase hex digits" }}{{ end -}}
{{- if not (has "mfa" .Token.amr) }}{{ fail "the login did not use MFA" }}{{ end -}}
{
  "subject": {},
  "sans": [{"type": "uri", "value": "https://idp.bank.example/person/{{ $id }}"}],
  "keyUsage": ["digitalSignature"],
  "unknownExtKeyUsage": ["1.3.6.1.4.1.99999.1.1"],
  "policyIdentifiers": ["1.3.6.1.4.1.99999.2.1"],
  "crlDistributionPoints": ["http://localhost:19001/1.0/crl"]
}
