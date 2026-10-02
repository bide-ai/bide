# gsm machine gate

The `gsm machine gate` workflow (`.github/workflows/gsm-gate.yml`) checks every gsm
machine bide's programs make (the `examples/govern` programs) with the two checkers extracted from gsm's Coq proof
(normalization-confluence `coq/extraction`): `checker` decides the property
`Build` checks on a machine's step tables, `astchecker` on its rules. It is an
interim gate, in CI only, until an in-process gate lands in gsm.

## What it checks

`machines.txt` lists every program that makes gsm machines (today the
`examples/govern` programs), every machine each one makes, and its expected
verdict (`certified`, `certified-tables`, `rejected`, `synthesized`,
`not-built`; the file explains each). The workflow runs each program once, with
no arguments, built with `-tags gsmgate`, which records every machine the program makes: each `Build`
result, including each component a federation builds, and each synthesized or
compositional machine. gsm's `internal/cmd/gsmgate` then runs both checkers on
the records. The job fails if:

- a checker rejects a machine listed as accepted, refuses its input, or crashes;
- a checker disagrees with `Build` (verifies a machine `Build` rejects);
- a program makes a machine `machines.txt` does not list, or a listed machine is
  not made;
- the scan (`gsmgate -scan`) finds, in the non-test code of any bide module that
  imports gsm (type-checked, so under any import name and as a function or method
  value), a package outside `machines.txt`'s programs that refers to a gsm
  function or method making a machine; a program that is not a main package or
  reads flags, its arguments or the environment (another run could make other
  machines); a gsm import a build constraint hides from the type check; or a
  document (`.md`, `.mdx`, `.rst`, `.adoc`, `.txt`, `.html`) showing a gsm machine
  without an `@doc` line. Nothing that makes machines can be exempt;
- the pinned gsm commit is not on gsm's `main`.

Test code is not scanned: test machines are not shipped. Documents are listed,
not run: `docs/guides/quorum.md` copies the quorum example's policy by hand. A machine whose rules
are closures cannot be exported as rules, so only the table checker runs on it
(`certified-tables`).

What it does not check: federation-level checks (morphisms, resolvers,
acyclicity, the monotone-cycle check), which gsm's `Build` does and no extracted
checker covers; and the runtime: the examples run against gsm at `GSM_COMMIT` in
this job, while bide's modules require the gsm version in their `go.mod`, so the
result applies to the examples as bide builds them once that version is at or
after `GSM_COMMIT`.

## Where the checkers come from

`pins.env` pins one gsm commit (`GSM_COMMIT`). Everything else comes from gsm's
`.github/oracle` at that commit, the same files gsm's own oracle job uses:
`pins.env` names the proof repository and commit, the Rocq image by digest and
the two checker binaries, and `SHA256SUMS` holds the expected hashes of the
extracted OCaml and both binaries. The job builds the checkers from that proof
commit in that image (`coq/extraction/ci-build.sh`), fails unless every hash
matches byte for byte, and fails if a checker the pins name is not hashed there.
A faster checker that gsm adopts (by its pins) reaches bide by moving
`GSM_COMMIT`; nothing here names a binary.

To move to a new gsm commit, change `GSM_COMMIT` to a commit on gsm's `main` (the
job fails otherwise). Its gsm checkout must contain `internal/cmd/gsmgate` (its
own module) and `gate.go`.
