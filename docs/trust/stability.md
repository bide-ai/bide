# Stability and support

This page states what bide promises about versions today, derived from what the repository does. bide is pre-1.0 (the current release is listed in the [changelog](../../CHANGELOG.md)); the policy is deliberately conservative about what it promises, and it says where there is no promise.

## Versioning policy

- **Semantic Versioning, with the 0.x rule.** The changelog declares that bide adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html) and that, while pre-1.0, a minor version (0.x.0) may include breaking API changes. A patch version (0.x.y) is for fixes.
- **One version for all modules.** The core module and the published library modules (`govern`, `store/sqlite`, `store/postgres`, `mcptools`, `trace`, `codec/gcf`, the governed-event log backends and `cmd/bide`) are released together under one version, each tagged `<dir>/vX.Y.Z` ([CONTRIBUTING, Releasing](../../CONTRIBUTING.md#releasing)). Use matching versions.
- **No 1.0 date.** The project stays on 0.x until the journal and proof formats are final. The roadmap states the 1.0 work that remains ([roadmap](../ROADMAP.md)).
- **Go version.** The release binaries are built with a pinned Go release recorded in the binary (`go version -m bide`); the library requires Go 1.27.

## How breaking changes are announced

- Every user-facing change adds a changelog fragment in its pull request; the release assembles them into [CHANGELOG.md](../../CHANGELOG.md), grouped Added, Changed, Fixed, Removed, Security. A breaking change is marked **Breaking:** under Changed or Removed, names the affected identifiers, and says what a caller must do.
- Each release has curated notes in [docs/releases](../releases), which become the GitHub Release body.
- A change that alters a documented guarantee also updates [the guarantee](../GUARANTEE.md) and [known limitations](../KNOWN-LIMITATIONS.md) in the same pull request.
- Security fixes use the `Security` section. Vulnerabilities are reported privately ([SECURITY.md](../../SECURITY.md)).

## Package stability

Every package of the published modules carries one of three labels. The labels are declared in one table in `internal/tools/apiref/labels.go`, and the [SDK API reference](../reference/api/index.md) is generated with them. A test fails when this table and that one differ, or when a package has no label.

- **core**: breaking changes only in a minor release, listed under Changed in the changelog with a migration note.
- **experimental**: may change in any release.
- **building block, not yet used by approval gates**: implemented and tested, but no approval gate calls it yet, so no guarantee rests on it. Its API may change before a gate uses it.

The certificate verification core in `audit` and the certificate-bound approver in `agent` carry the label of their packages (core): approval gates count a certificate-bound decision. `audit/rfc3161` is the one building block left: no gate or command calls it.

| Package | Stability |
|---|---|
| `agent` | core |
| `agent/agenttest` | core |
| `agent/storetest` | core |
| `anchor/awss3` | experimental |
| `anchor/awss3/awss3test` | experimental |
| `approvals` | core |
| `audit` | core |
| `audit/anchorobj` | experimental |
| `audit/crlrefresh` | experimental |
| `audit/eventstoretest` | core |
| `audit/rfc3161` | building block, not yet used by approval gates |
| `audit/verify` | core |
| `chaos` | experimental |
| `codec/gcf` | core |
| `eval` | experimental |
| `flowedit` | experimental |
| `govern` | experimental |
| `govern/eventlogtest` | experimental |
| `govern/postgreslog` | experimental |
| `govern/redislog` | experimental |
| `govern/sqlitelog` | experimental |
| `mcptools` | core |
| `middleware` | core |
| `model/anthropic` | core |
| `model/gemini` | core |
| `model/modeltest` | core |
| `model/openai` | core |
| `model/provider` | core |
| `notify` | core |
| `notify/relay` | experimental |
| `plan` | experimental |
| `plan/planview` | experimental |
| `schema` | core |
| `signer/awskms` | core |
| `signer/azurekv` | core |
| `signer/gcpkms` | core |
| `signer/pkcs11` | core |
| `signer/pkcs11/pkcs11test` | core |
| `store/postgres` | core |
| `store/sqlite` | core |
| `trace` | core |

## Journal and proof formats

Formats carry their own version tags, and a reader refuses what it does not read rather than guessing.

| Artifact | Tag today | Behavior across versions |
|---|---|---|
| Journal | `bide.journal.v1-dev` | Every run's journal starts with a header naming its format. A version refuses a run whose header it does not support, or that has none, with `*agent.JournalVersionError`. **Before 1.0 the tag is not bumped when keys or record shape change, so resuming a run across pre-releases is not promised** ([known limitations, stores](../KNOWN-LIMITATIONS.md#stores)). At 1.0 the tag becomes `bide.journal.v1` and 1.0 refuses every pre-release journal |
| Store schema | integer version in the store | A store refuses a database whose schema version is newer than it knows (`ErrConfig`). Older databases gain new tables on `Open` where the release says so |
| Signed tree head | `bide.audit.sth.v5` | Domain-separated and signed together with its scheme, kind and run |
| Evidence package | `bide.audit.evidence.v6` | Verified field by field; its seal signs the SHA-256 of the package. A `bide.audit.evidence.v5` package still verifies under its original seal (a note says so) and is never produced; v4 and older are refused with `audit.ErrFormat` |
| Proof bundle, absence, runcert, grant, journal export, event inclusion | each has its own `bide.audit.*` tag (for example `proof.v3`, `runcert.v3`, `journal-export.v1`) | See the source constants in `audit/` |
| Decision receipt | `bide.audit.decision-receipt.v1` | The signed decision an approver keeps, checked on its own and against signed tree heads |
| Notification trail export | `bide.audit.notify-trail-export.v1` | Read strictly: an unknown record kind or field is an error |
| Approval decision | `bide.approval.v3` | Decisions signed under `v2` no longer verify and must be signed again; a pending run asks again rather than run on them |
| Certificate-bound approval decision | `bide.approval.v4` | The `v3` layout plus the SHA-256 of the signer's leaf certificate, with the chain in the journal record (`approver_cert`) and in a receipt (`cert`). Defined and journaled by this version; nothing counts one yet ([design](../design/design-mofn-approval.md#v4-certificate-bound-decisions-format-only)) |
| Notification payload | `bide.notification.v1` | Receivers should ignore unknown fields |

### What stays verifiable across versions

- **Pre-1.0, a verifier reads only the current format of each artifact** ([security model](../guides/security-model.md)), with one exception: an evidence package in the previous format, `bide.audit.evidence.v5`, still verifies (see [known limitations](../KNOWN-LIMITATIONS.md)). An artifact written under any other older tag fails with `audit.ErrFormat` (not the format this version reads), distinct from `audit.ErrNotVerified`.
- **Therefore, to re-verify old evidence, keep the `bide` binary that produced it, or one built from its tag.** Release binaries are reproducible from their tag (`scripts/repro.sh`), and `go install github.com/bide-ai/bide/cmd/bide@vX.Y.Z` fetches an old version. [Operations](operations.md#retention-and-archiving) lists what to archive with the evidence.
- Inclusion proofs carry the record as stored bytes and ignore fields they do not know, so a record written by a newer release still verifies under the same proof format.
- The RFC 3161 time-stamp token verifier `audit/rfc3161` (stdlib only) is a new, pre-1.0 package; its refusal reasons (`rfc3161.Reason`) are stable names, but its `Policy` and `Expected` fields may change before a gate uses it ([known limitations](../KNOWN-LIMITATIONS.md#audit-and-proofs)).
- The standalone verifier `audit/verify` (stdlib only) is cross-checked bit for bit with the SDK and can be vendored at a fixed version.
- Some tag changes ship in minor releases and are listed as **Breaking:** in the changelog. Read the changelog's Breaking entries before upgrading and, if you hold long-lived evidence, re-verify a sample with the new version before retiring the old one.

## Support window

- **There is no long-term-support branch, and no stated backport policy.** Fixes land on `main` and ship in the next release. Nothing in the repository promises a fix to an older minor.
- The practical window is therefore the latest release. For a vulnerability, the stated process is a private report, an acknowledgement, and a fix plus disclosure timeline once triaged ([SECURITY.md](../../SECURITY.md)); the repository does not state fixed response times.
- Journals are not promised to resume across pre-releases. Upgrading between releases: finish or resolve in-flight runs first, then deploy. Some upgrades are not rolling; the release notes say which (for example the v0.8.0 to v0.9.0 journal header change in [known limitations](../KNOWN-LIMITATIONS.md#stores)).

## Deprecation process

What the repository does today:

1. A replaced API is kept as a thin, documented wrapper where that is cheap, and the changelog lists it under Deprecated (for example the old pause and approval wrappers in earlier releases).
2. It is removed in a later minor release, listed under Removed with **Breaking:**, naming the replacement.
3. Pre-1.0 there is no minimum deprecation period. A transitional API may be removed in the next minor.

If you build on bide, pin exact module versions, read the Breaking entries on every upgrade, and run your own tests and a sample of your archived evidence through the new `bide audit` before rolling forward.

## Verifying a release

Release artifacts are signed, carry an SBOM and have SLSA build provenance; see [Verifying releases](verify-releases.md).
