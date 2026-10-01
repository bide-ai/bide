----------------------------- MODULE ToolCallMC -----------------------------
(* Named assignments for the configurations: each .cfg picks its calls and what their *)
(* middleware may do from these with <-.                                               *)
EXTENDS ToolCall

\* The calls of the turn.
OneSide    == [c \in Calls |-> "side"]
SideDeleg  == (1 :> "side") @@ (2 :> "deleg")
TwoSide    == [c \in Calls |-> "side"]
\* A retry-safe tool that changes state (Idempotent, a Compensator), alone or beside a side effect.
OneIdem    == [c \in Calls |-> "idem"]
IdemSide   == (1 :> "idem") @@ (2 :> "side")

\* What a middleware may do (see LMw). Within the ToolMiddleware contract: call next once and
\* return what it returned or turn its success into an error (a result check), rename the call
\* (refused), end the call with ErrToolNotCalled without next, answer from a cache. A retry is
\* within it only for a retry-safe tool.
Contract    == {"next", "ret", "maperr", "deny", "cache", "renamed"}
ContractSafe == Contract \cup {"retry"}
\* Outside it: retry a side effect (the agent refuses the second call), leave next running,
\* return an error of its own without the sentinel, give up when the context is done.
Adversarial == Contract \cup {"retry", "async", "bare", "ctxerr"}

MWContract   == [c \in Calls |-> IF Kinds[c] = "side" THEN Contract ELSE ContractSafe]
MWAdv        == [c \in Calls |-> Adversarial]
MWAdvDirect  == [c \in Calls |-> Adversarial \cup {"direct"}]
MWPass       == [c \in Calls |-> {"next", "ret"}]
\* The side effect's middleware within the contract; the delegation's passes through.
MWSibling    == [c \in Calls |-> IF Kinds[c] = "side" THEN Contract ELSE {"next", "ret"}]
\* The same for liveness, without a cache answer (limits/cache-write-fails).
MWSiblingLive == [c \in Calls |-> IF Kinds[c] = "side" THEN Contract \ {"cache"} ELSE {"next", "ret"}]
\* A hedging wrapper: next in a goroutine, given up when the context is done.
MWHedge      == [c \in Calls |-> {"async", "ctxerr"}]
\* Liveness: within the contract and retries, without a cache answer (limits/cache-write-fails).
MWLive       == [c \in Calls |-> (Contract \ {"cache"}) \cup {"retry"}]
MWCache      == [c \in Calls |-> {"cache"}]
\* Regression and finding shapes.
MWAbandon    == [c \in Calls |-> {"async", "bare", "ctxerr"}]
MWDirect     == [c \in Calls |-> {"direct"}]
MWDirectNext == [c \in Calls |-> {"direct", "renamed", "ret"}]
MWRefLeak    == [c \in Calls |-> {"renamed", "async", "ret"}]
MWRetry      == [c \in Calls |-> {"next", "retry", "ret"}]
MWRetryArgs  == [c \in Calls |-> {"next", "retry", "ret"}]
MWMapErr     == [c \in Calls |-> {"next", "maperr"}]
MWLeakBare   == [c \in Calls |-> {"async", "bare"}]
\* Model 9's T3 to T5 and the rollback's re-run: a next left running past the chain's return,
\* with a cache answer (the retry-safe call), beside a step that fails the saga.
MWLeakCache  == [c \in Calls |-> IF Kinds[c] = "side" /\ c = 2 THEN {"next", "ret"} ELSE {"async", "cache"}]
MWIdemLeak   == [c \in Calls |-> IF Kinds[c] = "idem" THEN {"next", "async", "cache", "ret"} ELSE {"next", "ret"}]
MWIdemMapErr == [c \in Calls |-> IF Kinds[c] = "idem" THEN {"next", "maperr", "ret"} ELSE {"next", "ret"}]
=============================================================================
