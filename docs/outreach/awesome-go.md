# awesome-go submission (prepared, not submitted)

This is a working note for the maintainer. Nothing here has been opened on
[avelino/awesome-go](https://github.com/avelino/awesome-go). Checked on 2026-10-13 against that
repository's `CONTRIBUTING.md`, `.github/PULL_REQUEST_TEMPLATE.md` and `README.md` on `main`.
Re-check all three just before submitting: the list and its CI rules change often (its merged PRs
that week include CI changes).

## Verdict

Not ready. Two gaps block a clean submission, and one of them cannot be closed before 2027-02-23.

| Requirement | Source | bide today | Status |
|---|---|---|---|
| At least 5 months of history since the first commit | Quality standards; CI "Repository maturity" (warning) | First commit 2026-09-23 (`555c0303`); the repo was created 2026-09-24. Five months is 2027-02-23. | **Gap, time only** |
| Open source license | Quality standards; CI warning | Apache-2.0, detected by GitHub | Met |
| `go.mod` at the repository root | CI, blocking | `module github.com/bide-ai/bide` | Met |
| At least one `vX.Y.Z` release tag | CI, blocking | Root-module tags `v0.1.0` to `v0.15.0` | Met |
| pkg.go.dev link loads | CI, blocking | `pkg.go.dev/github.com/bide-ai/bide` and `.../agent` both return 200 and show the module and the package | Met |
| Coverage report link in the PR body | PR template; CI "Coverage link" (warning) | No coverage service is wired: no Codecov or Coveralls upload in `.github/workflows`, no badge in the README. `app.codecov.io/gh/bide-ai/bide` returns 200 but that is the service's app shell, not a report. | **Gap** |
| Coverage of 80% or more (90% for data packages), and the tests are read | Quality standards; manual review | `go test -cover -short ./...` on the root module, 2026-10-13: `agent` 91.3%, `plan` 91.2%, `audit` 88.7%, `audit/verify` 89.1%, `schema` 96.5%, `middleware` 97.6%, `model/openai` 97.6%, `model/anthropic` 94.2%, `chaos` 97.4%. Below 80%: `plan/planview` 50.0%, `model/modeltest` 18.6% (a test helper), and internal tool packages. Packages with no tests (examples, `agent/agenttest`) report 0%. The nested modules (`store/sqlite`, `store/postgres` and others) were not measured here. | Met for the core; needs a published number |
| README in English | Quality standards; CI warning | Yes (translations exist under `docs/i18n`) | Met |
| Go-style doc comments on public API | Quality standards | Enforced by `doccheck`, which has an empty allow list | Met |
| CI configured, tests required before merge | PR template (recommended) | GitHub Actions, merge queue, required checks | Met |
| Maintained, issues answered within about 2 weeks | Quality standards | Pushed daily | Met |
| Stable or progressing toward stable | Quality standards | v0.x, in a stabilization phase | Met, but a reviewer may ask |
| Category has at least 3 items | CI, blocking | The Artificial Intelligence category has 37 entries | Met |
| Go Report Card | Not in the current guidelines | The report page responds; no grade was confirmed | Optional |

### Closing the gaps

1. **Age.** Wait. Submitting before 2027-02-23 gets at best a warning and a manual rejection. In the
   meantime, keep releasing and answering issues.
2. **Coverage report.** Upload a merged coverage profile from CI to Codecov (or Coveralls) on pushes
   to `main`, add the badge to the README, and use the service's URL in the PR body. The root module
   and the nested modules need one merged profile or separate flags. Decide first whether the
   headline number should include the nested store modules. Report whatever the service says; do not
   quote a number it does not show.
3. **Optional.** Run `go vet ./...` and `staticcheck` once before submitting, as the guide suggests.

## Category and entry

**Category: Artificial Intelligence** ("Libraries for building programs that leverage AI."). bide is
an agent library, and that category already holds the Go agent frameworks (`agent-sdk-go`, `golem`,
`langgraphgo`, `trpc-agent-go`, `Aetheris`). Workflow Frameworks was the alternative: it holds
durable-execution engines (`Temporal`, `durable-go`, `GopherFlow`), and bide's plan flows would fit
there. Its identity is agents, so the first category is the better match.

The list is ordered without regard to case. The entry goes after `ai-gateway` and before
`chromem-go`:

```md
- [bide](https://github.com/bide-ai/bide) - Library for durable AI agents that resumes from a journal after a crash, runs side effects at most once, and halts when an outcome is unknown.
```

The description states what the library does, ends with a period, and uses no superlatives. The
link text is the repository name, as the CI check requires.

A neighbor to expect a question about: `Aetheris` ("AI Agent execution runtime with event sourcing,
checkpoint recovery, and At-Most-Once execution guarantee"). A reviewer may ask how they differ.
The bide README and `docs/GUARANTEE.md` state exactly what bide guarantees and under which
conditions; answer from those, not from adjectives.

## Pull request

**Title** (the list's recent PRs read "Add gotermfx to Other Software."):

```text
Add bide to Artificial Intelligence
```

**Body** (the template's links, then its checklist; fill in the coverage link once it exists):

```md
## Required links

- [x] Forge link (github.com, gitlab.com, etc): https://github.com/bide-ai/bide
- [x] pkg.go.dev: https://pkg.go.dev/github.com/bide-ai/bide
- [x] Coverage service link: https://app.codecov.io/gh/bide-ai/bide   <!-- only once uploads exist -->

## Pre-submission checklist

- [x] I have read the [Contribution Guidelines](https://github.com/avelino/awesome-go/blob/main/CONTRIBUTING.md#contribution-guidelines)
- [x] I have read the [Quality Standards](https://github.com/avelino/awesome-go/blob/main/CONTRIBUTING.md#quality-standards)

## Repository requirements

- [x] The repo has a `go.mod` file and at least one SemVer release (`vX.Y.Z`).
- [x] The repo has an open source license.
- [x] The repo documentation has a pkg.go.dev link.
- [x] The repo documentation has a coverage service link.
- [x] The repo has a continuous integration process (GitHub Actions, etc.).
- [x] CI runs tests that must pass before merging.

## Pull Request content

- [x] This PR adds/removes/changes **only one** package.
- [x] The package has been added in **alphabetical order**.
- [x] The link text is the **exact project name**.
- [x] The description is clear, concise, non-promotional, and **ends with a period**.
- [x] The link in README.md matches the forge link above.

## Category quality

- [x] The packages around my addition still meet the Quality Standards.
```

Before ticking the last box, open the entries next to the new one (`ai-gateway`, `chromem-go`, and a
few more in the category) and check their pkg.go.dev pages, licenses and recent activity. The
template asks the submitter to do this, and says to list any removals with a reason. If one of them
no longer meets the standards, say so in the PR body instead of ticking the box.
Do not remove other projects' entries just to make room.

The PR changes only `README.md`. Do not touch any other file.
