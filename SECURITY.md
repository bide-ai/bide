# Security Policy

## Reporting a vulnerability

Please report security issues privately, not in a public issue.

- **Preferred:** GitHub [private vulnerability reporting](https://github.com/bide-ai/bide/security/advisories/new) (the repo's Security tab, "Report a vulnerability").
- **Or email:** dayna@blackwell-systems.com.

You will get an acknowledgement, and a fix plus disclosure timeline once the report is triaged. bide is Apache-2.0, maintained by Dayna Blackwell (bide-ai).

## Verifying releases

Release archives and `checksums.txt` are signed keylessly with cosign (GitHub OIDC), each archive has an SPDX SBOM that is signed too, and each release publishes an SBOM of its Go module set. Archives, SBOMs and checksums also have SLSA v1 build provenance attestations (Build Level 3, made by a separate release-builder repository), checked with `gh attestation verify`. See [Verifying releases](docs/trust/verify-releases.md) for the commands. Release binaries also rebuild byte for byte from their tag (`scripts/repro.sh`).

## Trust documentation

For security and risk reviewers: the [threat model](docs/trust/threat-model.md) (assets, actors, what bide prevents, detects and does not address), [stability and support](docs/trust/stability.md), a [comparison with Temporal, LangGraph and idempotency keys](docs/trust/comparison.md), and [operations guidance](docs/trust/operations.md) (key custody, retention, backups, clocks, database roles).

## Scope

The cryptographic guarantees cover integrity, authenticity, tamper-evidence, and non-repudiation, not confidentiality (journal leaves are not encrypted). See [docs/guides/security-model.md](docs/guides/security-model.md) for the exact scope before reporting.
