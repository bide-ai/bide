---------------------------- MODULE DelegationMC ----------------------------
(* Named trees for the configurations, which select one with <-. A tree is a tuple of runs,   *)
(* run 1 the root; each run is the tuple of its turn's calls.                                 *)
EXTENDS Delegation

\* A call. k: "eff" (a compensable side effect with an attempt marker), "fail" (a tool that
\* returns its own error), "deleg" (AttenuatingSubAgent over sub-run ch), "sub" (a tool that
\* starts the programmatic sub-run ch, 0 for none, with RunSaga if saga, from a goroutine that
\* outlives the call if late; w: the tool is itself a retry-safe compensable write; decl: the
\* agent WithSubRuns declares, "same", "none" or "other" (another store, or a panic); store: the
\* store the sub-run's agent journals to; long: a tool-use ID longer than encodeID's limit; forge:
\* the tool also tries another call's sub-run ID). ne and nsub: the AttenuateFunc's NotAfter
\* (Inf: the parent's) and subject.
Base == [k |-> "eff", ch |-> 0, w |-> FALSE, decl |-> "same", store |-> "same", saga |-> TRUE,
         long |-> FALSE, late |-> FALSE, forge |-> FALSE, ne |-> Inf, nsub |-> "self"]
Eff == Base
Fail == [Base EXCEPT !.k = "fail"]
Deleg(c) == [Base EXCEPT !.k = "deleg", !.ch = c]
DelegNe(c, x) == [Deleg(c) EXCEPT !.ne = x]
DelegOther(c) == [Deleg(c) EXCEPT !.nsub = "other"]
Sub(c) == [Base EXCEPT !.k = "sub", !.ch = c, !.forge = TRUE]
SubDecl(c, d) == [Sub(c) EXCEPT !.decl = d]
SubPlain(c) == [Sub(c) EXCEPT !.saga = FALSE]
SubW(c) == [Sub(c) EXCEPT !.w = TRUE]
SubLong(c) == [Sub(c) EXCEPT !.long = TRUE]
SubOther(c) == [Sub(c) EXCEPT !.store = "other"]
SubLate(c) == [Sub(c) EXCEPT !.late = TRUE]
RSW == SubW(0)

\* A saga delegates, then fails: mint, reuse on resume, BindRollback.
TDeleg == << <<Deleg(2), Fail>>, <<Eff>> >>
\* The same with a child grant that expires at tick 1.
TDelegNe == << <<DelegNe(2, 1), Fail>>, <<Eff>> >>
\* A delegation inside a delegation: the grandchild narrows the child's grant.
TNested == << <<Deleg(2), Fail>>, <<Deleg(3), Eff>>, <<Eff>> >>
\* Halt propagation (bug 8 of #117's final review): the child's turn holds an Unrecorded refusal
\* and a lost outcome; the root's next step must not start.
THalt == << <<Deleg(2), Eff>>, <<Deleg(3), Eff>>, <<Eff>> >>
\* Two delegations, then a failure: their grants may come from different bound grants (D1).
TTwo == << <<Deleg(2), Deleg(3), Fail>>, <<Eff>>, <<Eff>> >>
\* Programmatic sub-runs: a declared agent, an undeclared one, one on another store; a long ID.
TSubs == << <<Sub(2), SubDecl(3, "none"), SubDecl(4, "other"), Fail>>, <<Eff>>, <<Eff>>, <<Eff>> >>
TSubLong == << <<SubLong(2), Fail>>, <<Eff>> >>
TSubOther == << <<SubOther(2), Fail>>, <<Eff>> >>
TSubLate == << <<SubLate(2), Fail>>, <<Eff>> >>
\* A saga's call starts a plain sub-run, whose call starts another (B3: three levels).
TPlain == << <<SubPlain(2), Fail>>, <<SubPlain(3)>>, <<Eff>> >>
\* The rollback re-runs a retry-safe compensable call that never ran, and it starts a sub-run (B4).
TRerun == << <<Fail, SubW(2)>>, <<Eff>> >>
\* A delegated sub-run's own saga fails before its retry-safe write runs; the root's rollback
\* re-runs that write under the journaled grant (D2).
TDelegRerun == << <<DelegNe(2, 1)>>, <<Fail, RSW>> >>
\* A delegation inside a plain sub-run of a saga, whose sub-run ends with a model error (D3).
TPlainDeleg == << <<SubPlain(2), Fail>>, <<Deleg(3)>>, <<Eff>> >>
\* An AttenuateFunc that names another subject.
TDelegOther == << <<DelegOther(2), Fail>>, <<Eff>> >>
=============================================================================
