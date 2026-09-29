# The durability guarantee

This is the precise statement of the core guarantee (pillar 1 in `POSITIONING.md`).
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

The same holds when nothing crashed and a caller simply invokes the run again (a client retrying
after a lost response, a redelivered job, a sub-agent or session turn re-entered on resume):

4. **Re-invoking a finished run** → the run's completion marker is journaled → it returns the
   recorded final answer without asking the model for another turn. Correct. This matters because
   the protection is keyed by the tool call the model emitted: a fresh model turn could request the
   same side effect again under a new call id, which the journal would treat as new work.

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
- **The tool must declare its safety accurately.** `ReadOnly` re-runs freely, `Idempotent`
  retries, and only an unmarked non-idempotent write gets the attempt-marker/halt treatment.
  Mislabel a card-charge as idempotent and you have opted out of the protection.
- **It is at-most-once for the side effect, not "the agent always finishes."** A crash can still
  leave a run halted and needing intervention. The promise is *safety* (no double-fire, no lost
  completed work), not *liveness* (guaranteed completion without help).

## The one-sentence version

Not "it can't crash," and not even "it always recovers automatically." It is: **when it
crashes, you never lose completed work and you never double-execute a side effect, and in the
one genuinely ambiguous window it stops and tells you instead of guessing.**
