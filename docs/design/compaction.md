# Journal compaction with proof continuity (design note)

Status: design, not implemented. This note pins the seam before the audit API ossifies, because
retrofitting proof continuity onto an append-only log is painful. It records the technique, the one
soundness invariant that must not be gotten wrong, and a staged plan. Nothing here ships yet.

## The problem

Two things grow without bound in a long-lived run, and each backs a guarantee compaction must keep:

- **The journal** backs deterministic replay (`Replay`) and `Durable.Do`'s at-most-once
  memoization (a step recorded under `(runID, name)` is never re-run). Compaction must preserve
  **functional continuity**: replay from the compacted point reproduces the same state, and no
  dropped step is ever re-executed.
- **The Merkle tree and its STHs** back the audit proofs (inclusion, consistency, absence). RFC
  6962 is deliberately append-only and never forgets, so truncating leaves breaks every consistency
  proof already issued. Compaction must preserve **proof continuity**: proofs issued before a
  snapshot still verify, or are cryptographically linked to the post-snapshot tree.

CRDT stores (Automerge, Yjs) faced only the first problem (garbage-collecting tombstones). Bide
faces both, so their GC technique is necessary but not sufficient: the audit tree is the added
constraint.

## Technique: sealed segments with a carry-forward checkpoint chain

Divide a run's journal into segments. To **seal** segment k:

1. Compute the STH over segment k: root `R_k`, size, signature. Retain it permanently (it is small).
2. Compute a **deterministic state snapshot** `S_k`. For governed runs this is the `gsm State.Digest`
   plus the `PolicyDigest`. For agent runs it is the resumable state: the conversation transcript
   plus any completed `Step` results that later steps depend on.
3. Emit a **seal record** `{segment: k, prevRoot: R_{k-1}, root: R_k, size, snapshotDigest: S_k}` as
   **leaf 0 of segment k+1**. This carry-forward makes the new segment's tree commit
   cryptographically to the previous segment's root and to the snapshot.
4. The raw records of segment k may now be dropped from hot storage.

**Proof continuity** then rests on a signed hash-chain of checkpoints `R_0 -> R_1 -> ... -> R_live`:
each STH is signed, and each segment commits to the previous root through its leaf-0 seal. A
verifier checks that chain instead of holding every leaf. Per-record inclusion for a *compacted*
record needs its raw bytes, which is a deliberate policy knob:

- **Prunable audit**: keep only the signed checkpoint chain and the snapshots. You can prove "the
  state at checkpoint k had root `R_k` and produced snapshot `S_k`," but you cannot re-prove an
  individual old tool call.
- **Archival audit**: also keep sealed leaves in cold storage, so old inclusion proofs still verify
  on demand against the retained `R_k`.

Both retain every signed STH and seal record forever; they differ only in whether raw leaves are
kept for deep inclusion proofs.

## The soundness invariant: at-most-once must survive sealing

This is the part that must not be gotten wrong, because getting it wrong reintroduces the
double-fire the durable layer exists to prevent.

`Durable.Do` is memoized by `(runID, name)`. If a sealed record is dropped and a later `Do`
references that name, it will not find the record and will re-execute the side effect. So:

- **Seal only a closed prefix**: a prefix no future `Do` will look up by name.
- **The snapshot must carry any completed-step results a later step could read**, so nothing
  dropped is ever needed again.

For linear agent runs this is natural: once a turn completes, its intermediate step names are not
revisited, and the snapshot is the transcript plus terminal state. But it is a real invariant the
`Seal` API must *enforce* (reject a seal boundary that a live step could reach back across), not
assume. This is the property to cover with tests first.

## Trusting the snapshot

The snapshot is a claim. It is made trustworthy two ways, both reusing existing machinery:

- **Anchored**: the seal record is a leaf under the next segment's signed STH, so the snapshot claim
  is committed in the tree like any other action.
- **Replay-verifiable**: runs are deterministic given policy and events, so a verifier holding the
  prior segment's leaves (archival mode) reproduces `S_k`'s `State.Digest` by replay. This is the
  same determinism-and-replay trust already used for per-run state digests (see AUDIT.md), applied
  at the segment boundary.

## New primitives (sketch)

Building on what exists (`SignedTreeHead`, `ProveRecord` / consistency proofs, `State.Digest`,
`PolicyDigest`, `Step[T]`, `Replay`):

- `Seal(ctx, store, runID, upToIndex) (SealRecord, error)`: STH over the prefix, compute the
  snapshot, emit the carry-forward leaf, and mark the prefix compactable. Enforces the closed-prefix
  invariant.
- `SealRecord{Segment, PrevRoot, Root, Size, SnapshotDigest, PolicyDigest}`: the checkpoint.
- `VerifySealChain(seals []SealRecord, sths []SignedTreeHead, pub) (bool, error)`: walk the signed
  checkpoint chain and verify each link (segment k+1 leaf 0 commits to `R_k`).
- `ReplayFrom(snapshot, liveSegment)`: replay without genesis.
- `Compact(runID, throughSegment)`: storage operation that drops sealed hot leaves, retaining the
  STH and seal record (and, in archival mode, moving raw leaves to cold storage).

## Staged plan

1. **Build `Seal` + `VerifySealChain` first.** The checkpoint chain is the load-bearing, hard-to-
   retrofit part, and it is what makes proofs survive a boundary.
2. **Gate the closed-prefix soundness rule with tests** before anything drops a record: prove a
   sealed-then-resumed run never re-fires a side effect.
3. **Defer storage GC and cold-archive** (`Compact`, cold storage tiers). These are pure operations
   once the chain and the soundness rule exist, and they are cheap to add when growth actually bites.

The one thing not to defer is the at-most-once soundness rule; the rest is additive.
