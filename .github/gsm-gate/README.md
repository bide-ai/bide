# gsm machine gate

The `gsm machine gate` workflow (`.github/workflows/gsm-gate.yml`) checks every gsm
machine bide ships with the two checkers extracted from gsm's Coq proof
(normalization-confluence `coq/extraction`): `checker` decides the property
`Build` checks on a machine's step tables, `astchecker` on its rules. It is an
interim gate, in CI only, until an in-process gate lands in gsm.

## What it checks

`machines.txt` lists every program that makes gsm machines (today the
`examples/govern` programs), every machine each one makes, and its expected
verdict (`certified`, `certified-tables`, `rejected`, `synthesized`,
`not-built`; the file explains each). The workflow runs each program built with
`-tags gsmgate`, which records every machine the program makes: each `Build`
result, including each component a federation builds, and each synthesized or
compositional machine. gsm's `internal/cmd/gsmgate` then runs both checkers on
the records. The job fails if:

- a checker rejects a machine listed as accepted, or refuses its input;
- a checker disagrees with `Build` (verifies a machine `Build` rejects);
- a program makes a machine `machines.txt` does not list, or a listed machine is
  not made;
- a directory's non-test Go code creates a gsm registry or federation and is not
  a program in `machines.txt` (or `@exempt`), or a document shows one without an
  `@doc` line.

Test code is not scanned: test machines are not shipped. A machine whose rules
are closures cannot be exported as rules, so only the table checker runs on it
(`certified-tables`).

What it does not check: a federation's morphisms and its acyclicity (no extracted
checker covers them), and the runtime: the examples run against gsm at
`GSM_COMMIT` in this job, while bide's modules require the gsm version in their
`go.mod`. The claim holds for the machines bide ships once that version is at or
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

To move to a new gsm commit, change `GSM_COMMIT`. Its gsm checkout must contain
`internal/cmd/gsmgate` and `gate.go`.
