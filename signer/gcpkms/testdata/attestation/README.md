Test data for attestation verification.

- `google-owner-root.pem`: Google's Cloud HSM owner root ("Hawksbill Root v1 prod"), copied from
  GoogleCloudPlatform/python-docs-samples kms/attestations/verify_attestation_chains.py (Apache-2.0);
  identical to https://www.gstatic.com/cloudhsm/roots/global_1498867200.pem.
  SHA-256 fingerprint 46:B5:FD:35:1D:56:A0:72:1C:A0:AF:CD:17:31:C0:F7:B7:4E:39:41:EB:81:8B:FD:0E:C3:6E:29:DF:0D:E0:95.
- `marvell-root.pem`: Marvell (Cavium) LiquidSecurity root ("localca.liquidsecurity.cavium.com"), the
  `liquid_security_certificate.crt` of Marvell's liquid_security_certificate.zip. SHA-256 fingerprint
  97:57:57:F0:D7:66:40:E0:3D:14:76:0F:8F:C9:E3:A5:58:26:FA:78:07:B2:C3:92:F7:80:1A:95:BD:69:CC:28.
  It expired on 2025-11-16; the verifier does not enforce validity periods (see the key custody guide).
- `cloudhsm-v2.att.gz`, `cloudhsm-v2.chain.pem`, `cloudhsm-v2.pub.pem`, `cloudhsm-v2.version.json`: a real
  Cloud HSM key version, produced on 2026-10-14 in the throwaway Google Cloud project `bide-attest-1ced6e`
  (key ring `bide-attest`, key `bide-p256`, version 1, location `us-east1`): EC_SIGN_P256_SHA256, protection
  level HSM, attestation format CAVIUM_V2_COMPRESSED. How: `gcloud kms keys versions describe 1 --key bide-p256
  --keyring bide-attest --location us-east1 --attestation-file att.dat` (the attestation, `att.dat` here as
  `cloudhsm-v2.att.gz`, and the JSON description, `version.json`), `gcloud kms keys versions get-certificate-chain 1
  ...` (four certificates), and the public key from `gcloud kms keys versions get-public-key`. The key version has
  been scheduled for destruction, so the files are a fixed snapshot. They contain no secret: an attestation is a
  signed public statement, the chain is certificates, the key is a public key, and the description is metadata.
  The resource name (with the project id) is hashed into the attestation's key ID, so it cannot be changed.
- `peculiar-v1.att.gz.b64` and `peculiar-v1.chain.pem`: a version 1 attestation (base64 of the gzip
  content) and its certificate chain, from PeculiarVentures/attestation packages/marvell/src/validator.spec.ts
  (MIT License, Copyright PeculiarVentures). The attested object is a public key object with label
  `app_key`, so it passes the signature and chain checks and fails the key policy (it is a public-key object, and extractable).
