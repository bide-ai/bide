# The durability guarantee

This is the precise statement of the core guarantee (guarantee 1 in the [README](../README.md#1--at-most-once-not-at-least-once-measured-not-claimed)).
README and marketing assert it; this document specifies it, including exactly where it stops.

## What the guarantee is

Every step that completed is durably recorded before the run depends on it, and on restart the
run resumes from that record instead of redoing completed work. A non-idempotent side effect is
never executed a second time.

The **unit of protection is the journaled step.** The moment a step's outcome is written to the
store, it is safe: resume replays it from the journal rather than re-running it.

## The three cases when a crash hits

1. **Crash before a step ran** → nothing recorded → resume runs it fresh. Correct.
2. **Crash after a step ran and its result was journaled** → resume reads the result, skips
   re-execution. Correct.
3. **Crash in the gap** (the side effect fired but its result was not journaled yet) → this is
   the dangerous window every other system re-runs into (the double-charge). Bide wrote an
   **attempt marker** before firing, so on resume it sees "this non-idempotent thing was
   attempted, outcome unknown" and **halts** (`ResumeHalt`) instead of guessing. It does not
   silently re-run, and it does not silently assume success.

   The gap between writing the marker and calling the effect is not dangerous when the process
   survives it: a driver cancelled there (a shutdown, a lost lease, a sibling's failure) or whose
   store fails there does not call the effect, and records that the attempt did not start. That
   attempt then no longer halts a resume, and the effect is attempted again under a new marker,
   claimed exclusively like the first. A process that dies in that gap records nothing, so its
   marker halts as in case 3.

The same holds when nothing crashed and a caller simply invokes the run again (a client retrying
after a lost response, a redelivered job, a sub-agent or session turn re-entered on resume):

4. **Re-invoking a finished run** → the run's completion marker is journaled → it returns the
   recorded final answer without asking the model for another turn. Correct. This matters because
   the protection is keyed by the tool call the model emitted: a fresh model turn could request the
   same side effect again under a new call id, which the journal would treat as new work. A crash
   after the final answer was journaled but before the marker was written is handled the same way:
   the resumed run finishes with the recorded answer and writes the marker, without a model call.

Case 3 is the whole moat. The precise phrasing is **at-most-once**: the side effect fires zero or
one times, never twice. It is **not** "exactly-once": an unresumable crash in that window can
leave it having fired once but unconfirmed, and the system stops for a human/policy decision
rather than pretending it knows.

## The boundary conditions (where the claim stops)

- **The store must survive the crash.** Durability is inherited from the journal's backend. If
  you use the in-memory store and the process dies, there is nothing to resume from: that is a
  dev/test store, not a durability claim. SQLite / Postgres / etc. is where the guarantee
  actually lives, and only as far as that storage's own durability (fsync, replication) holds.
- **The write to the store must itself be atomic/durable.** The guarantee reduces to "the
  journal did or did not record this step"; it relies on the store committing atomically. It
  does not defend against the storage layer lying about a commit.
- **Two drivers of one run.** Leases normally keep one process driving a run, but no lease can
  guarantee that: a holder stalled past its TTL (a long GC pause, a suspended VM, a partition) wakes
  still driving. The guarantee does not rely on the lease. Before a non-idempotent side effect, a
  driver writes the attempt marker as an exclusive claim, and only the driver whose claim the store
  kept runs it; the other halts (`ResumeHalt`). This relies on the store recording a step name at
  most once across processes (the SQLite and Postgres stores use a primary key), which is the atomic
  write above. Leases are deliberately not fenced (no token that the store checks on each write):
  the side effect happens outside the store, where no token could be checked, so only a claim
  written before the effect can stop a second one; see
  [known limitations](KNOWN-LIMITATIONS.md#durability-and-recovery).
- **The tool must declare its safety accurately.** `ReadOnly` re-runs freely, `Idempotent`
  retries, and only an unmarked non-idempotent write gets the attempt-marker/halt treatment.
  Mislabel a card-charge as idempotent and you have opted out of the protection.
  A call keeps the safety it fired under: the attempt marker is written only for a call that
  was not retry-safe when it fired, so a resume halts on a marker without a result whatever the
  tool is declared as by then (relabelled retry-safe, or no longer registered at all), and a
  `Step` attempted as a side effect halts even if the resuming code passes a retry-safe
  `StepSafety`. The marker needs no new field for this, so markers written by earlier versions
  are read the same way: every one of them means "not retry-safe, halt", unless the driver that
  wrote it recorded that its attempt never started.
- **It is at-most-once for the side effect, not "the agent always finishes."** A crash can still
  leave a run halted and needing intervention. The promise is *safety* (no double-fire, no lost
  completed work), not *liveness* (guaranteed completion without help).

## The one-sentence version

Not "it can't crash," and not even "it always recovers automatically." It is: **when it
crashes, you never lose completed work and you never double-execute a side effect, and in the
one genuinely ambiguous window it stops and tells you instead of guessing.**
