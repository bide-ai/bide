---------------------------- MODULE Identity ----------------------------
(***************************************************************************)
(* Model 15: approver identity bound to certificates                       *)
(* (docs/design/approver-identity.md, "Time: what makes a decision valid", *)
(* "Status rule", "Revocation", "Formal model impact"). It models the     *)
(* counting rule over signed decisions that carry a certificate, with one  *)
(* CA, a series of CRLs, a journal with signed heads, and a gate that     *)
(* anchors, counts, tallies and executes. Plain TLA+, like model 13.      *)
(*                                                                         *)
(* Time is a small integer clock. A certificate has a holder (the person   *)
(* whose key it certifies), an identifier (its SAN), a validity interval   *)
(* [nb, na] and, once the CA revokes it, an effective time E that may lie  *)
(* in the past. The CA publishes CRLs (number, thisUpdate, the revocations *)
(* it knows). The verifier holds an archive (rcvd, every CRL it received)  *)
(* that the live count, the pre-execution check and the offline tier read: *)
(* a listing in any received CRL revokes, and absence is read from any     *)
(* fresh CRL that passes the L(C) rule. An adversary can withhold a CRL from the verifier, deliver an older one,   *)
(* publish a head stamped out of order, write a decision with an earlier   *)
(* decided_at, and (when the CA's name constraint is not enforced) get a   *)
(* certificate for another person's identifier.                            *)
(*                                                                         *)
(* Each rule has a switch (the constants below). The passing                *)
(* configurations switch all on; each counterexample configuration         *)
(* switches one off and names the property that fails.                      *)
(*                                                                         *)
(* Abstractions (see spec/tla/README.md, model 15): one CA, no             *)
(* intermediates; one trust anchor, so no DistrustAfter; a stolen key and  *)
(* a genuine one are the same submission (the rules never read who held    *)
(* the key); RoutineGrace is 0; ClockTolerance is 0; no OCSP, delta CRLs,  *)
(* holds or expiredCertsOnCRL; no independent time source; W' is W (no     *)
(* cut by the certificate's life) and there is no MinW floor.              *)
(***************************************************************************)
EXTENDS Integers, FiniteSets

CONSTANTS
  Persons,      \* people (model values)
  Idents,       \* approver identifiers (model values)
  IdOwner,      \* identifier -> the person it belongs to (the bank's injectivity control)
  MaxT,         \* the clock runs 0..MaxT
  NaSet,        \* the notAfter values the CA may give a certificate
  IssueMax,     \* the latest time the CA issues a certificate
  MaxCert,      \* certificates the CA issues
  MaxDec,       \* decisions submitted
  MaxCRL,       \* CRLs the CA publishes
  CATier,       \* the leaf's issuing CA is itself certified by a root: a second tier with its own CRL series
  MaxRCRL,      \* CRLs the root publishes (the CA tier's series)
  RootSpacing,  \* the least time between two root CRLs
  MaxAgeCA,     \* live: oldest usable root CRL when a decision is counted
  ExecAgeCA,    \* oldest usable root CRL immediately before the call executes (ExecStatusAgeFor of the CA)
  MaxHeads,     \* signed heads
  K,            \* seats a tally needs
  MaxAge,       \* live: oldest usable status (now - thisUpdate) when a decision is counted
  ExecAge,      \* live: oldest usable status immediately before the call executes
  Period,       \* the CA's declared CRL period (CAPeriod; nextUpdate = thisUpdate + Period)
  MinSpacing,   \* the least time between two CRLs the CA actually publishes
  Skew,         \* decided_at <= A(d) + Skew
  Grace,        \* A(d) <= notAfter + Grace (anchorGrace)
  W,            \* the compromise-reporting window the final tier needs after A(d)
  \* Rule switches (TRUE: the rule is in force).
  NameCons,     \* the verifier enforces the CA's name constraint (a cert's identifier is its holder's)
  IdBind,       \* a counted decision's approver id equals the leaf's identifier
  Fresh,        \* live status must be no older than MaxAge
  NumRule,      \* the cache refuses a CRL numbered lower than the cached one
  NonDec,       \* head times must be non-decreasing in size; an inverted pair is contradictory
  GateAnchor,   \* before tallying, the gate anchors a decision no head covers
  ExecRecheck,  \* status is re-checked immediately before the call executes
  StickyTally,  \* a terminal tally is never recounted
  StaleSticky,  \* stale or missing status never removes a counted decision
  RevByClaim,   \* FALSE (the rule): E is compared with A(d); TRUE: with the signed decided_at
  DedupSeat,    \* a seat is an identifier: two decisions of one identifier are one seat
  Cov,          \* series coverage for the final tier: "none" | "time" | "number"
  CountLive,    \* a fresh CRL issued before A(d) may count a decision (provisional)
  LBoundCount,  \* the count reads a CRL beyond notAfter only if it is L(C), shown to follow its predecessor
  LBoundExec,   \* the pre-execution check applies the same rule to the status it reads
  DeadGuard,    \* a decision removed by a revocation is never counted again
  TallyRecheck, \* the tally forms only when no counted decision is listed in the archive
  CountCATier,  \* the count reads the CA tier's status (fresh within MaxAgeCA, not listing the CA with E <= A(d))
  ExecCATier,   \* the pre-execution check reads the CA tier's status (fresh within ExecAgeCA, no such listing)
  FinalCATier,  \* a final verdict also needs the CA tier: a root CRL a window past A(d), and no root CRL listing the CA
  LeafAgeFromCA,\* wrongly, the pre-execution check ages the leaf's status by ExecAgeCA instead of ExecAge
  \* The adversary and the CA's options.
  Prune,        \* the CA may drop a revoked certificate's entry after one CRL beyond its notAfter
  CanRevoke,    \* the CA may revoke a certificate
  CanMisIssue,  \* the CA may issue a certificate that breaks the name constraint
  CanWithhold,  \* a published CRL may be kept from the verifier
  CanRollback,  \* an older published CRL may be delivered after a newer one
  CanSkew,      \* a head may be stamped out of order
  CanBackdate,  \* a decision may carry a decided_at earlier than its true time
  CanRevokeCA,  \* the root may revoke the CA (the whole chain below it)
  CanCrash,     \* the gate may crash: the counted set is lost, the journal (heads, the tally) is not
  CanMisbind,   \* a decision may name an approver id other than its certificate's identifier
  \* A CA that splits its revocations over CRLs with an issuing distribution point (IDP).
  Partitioned,  \* the CA publishes two partitions of CRLs; a certificate's CDP names one, a CRL's IDP names one
  PartRule      \* TRUE (the rule): absence for a certificate is read only from a CRL whose IDP its CDP names;
                \* FALSE: from any CRL of the issuer (fail-open). Listings count from every CRL either way

ASSUME NaSet \subseteq 0..MaxT /\ IssueMax \in Nat
ASSUME MaxT \in Nat /\ MaxCert \in Nat /\ MaxDec \in Nat /\ MaxCRL \in Nat /\ MaxHeads \in Nat
ASSUME K \in Nat /\ MaxAge \in Nat /\ ExecAge \in Nat /\ Period \in Nat /\ MinSpacing \in Nat
ASSUME MinSpacing >= 1
ASSUME CATier \in BOOLEAN /\ MaxRCRL \in Nat /\ RootSpacing \in Nat /\ MaxAgeCA \in Nat /\ ExecAgeCA \in Nat
ASSUME RootSpacing >= 1
ASSUME Skew \in Nat /\ Grace \in Nat /\ W \in Nat
ASSUME Cov \in {"none", "time", "number"}
ASSUME Partitioned \in BOOLEAN /\ PartRule \in BOOLEAN
ASSUME Partitioned => Cov # "number"   \* a number sequence is per partition; the model indexes CRLs globally
ASSUME IdOwner \in [Idents -> Persons]

Time == 0..MaxT
NoE == MaxT + 1     \* "no revocation" / "not listed"
Certs == 1..MaxCert
Decs == 1..MaxDec
CRLs == 1..MaxCRL
RCRLs == 1..MaxRCRL

VARIABLES
  now,        \* the clock
  \* The CA.
  nc, hold, idn, nb, na, rev,   \* certificates: count, holder, identifier, notBefore, notAfter, E or NoE
  np, ctu, clst,                \* CRLs: count, thisUpdate, cert -> the E it lists or NoE
  \* The verifier.
  caRev,      \* the root's revocation of the CA: its effective time E, or NoE
  rnp, rtu, rrl,  \* the root's CRLs: count, thisUpdate, the E each lists for the CA (NoE if none)
  rrcvd,      \* the root CRLs the verifier received
  rcvd,       \* the archived series: numbers of the CRLs the verifier has received
  snap,       \* the latest CRL received (0: none); the number rule (NumRule) keeps the higher number
              \* when an older one arrives later. The count reads the archive, so only a probe reads it
  \* The journal and its heads.
  heads,      \* signed heads: [sz, tm (the signed time), rl (ghost: when it was really made)]
  \* Decisions (a decision's journal position is its index).
  nd, da, dc, dt,               \* count, approver id, certificate, signed decided_at
  \* The gate.
  waiting, cn, dead, tl, execd,
  \* Ghosts, written when a decision is counted or the call executes.
  cA,         \* A(d) at the count
  cRb,        \* a CRL the verifier received, or that is older than MaxAge, lists the chain with E <= A(d)
  cNb,        \* the same, against a head that precedes the decision's record
  cOk,        \* status was fresh, in window
  eb,         \* at execution, the leaf tier's status was stale or showed E <= A(d) of a counted decision
  ebC         \* the same for the CA tier

vars == <<now, nc, hold, idn, nb, na, rev, np, ctu, clst, caRev, rnp, rtu, rrl, rrcvd, rcvd, snap, heads, nd, da, dc, dt,
          waiting, cn, dead, tl, execd, cA, cRb, cNb, cOk, eb, ebC>>

\* vocabulary: begin
\* The journal records the model reads and writes: a cert-bound approval decision, a signed head
\* (the anchor), the terminal tally.
RecordKinds == {"approval", "anchor-head", "approval-tally"}
\* vocabulary: end

Min2(a, b) == IF a <= b THEN a ELSE b
MaxOf(S) == CHOOSE m \in S : \A x \in S : x <= m
MinOf(S) == CHOOSE m \in S : \A x \in S : m <= x

(***************************************************************************)
(* Heads, A(d), and the contradictory-head rule                            *)
(***************************************************************************)
Covers(d) == {h \in heads : h.sz >= d}
Covered(d) == d <= nd /\ Covers(d) # {}
SmallSz(d) == MinOf({h.sz : h \in Covers(d)})
Smallest(d) == {h \in Covers(d) : h.sz = SmallSz(d)}
\* A(d): the signed time of the smallest covering head (the earliest, if several of that size).
AOf(d) == MinOf({h.tm : h \in Smallest(d)})
\* A pair of heads out of order (a larger head stamped earlier than a smaller one, or two heads of
\* one size with two times) marks both members.
Implicated(h) ==
  \E g \in heads :
    \/ g.sz < h.sz /\ g.tm > h.tm
    \/ g.sz > h.sz /\ g.tm < h.tm
    \/ g.sz = h.sz /\ g.tm # h.tm
HeadOK(d) == ~NonDec \/ \A h \in Smallest(d) : ~Implicated(h)

(***************************************************************************)
(* Status                                                                  *)
(***************************************************************************)
\* Partitions: with Partitioned, certificate c's CRL distribution point is CertPart(c) and the CA's k-th CRL
\* carries the issuing distribution point CrlPart(k) and lists only the certificates of that partition (the
\* two alternate). TrueCrlCovers is what the CRL covers (RFC 5280 section 5.2.5, 6.3.3); CrlCovers is what the
\* verifier takes it to cover: with PartRule the same, without it every CRL of the issuer (fail-open).
CertPart(c) == IF Partitioned THEN c % 2 ELSE 0
CrlPart(k) == IF Partitioned THEN k % 2 ELSE 0
TrueCrlCovers(c, k) == CertPart(c) = CrlPart(k)
CrlCovers(c, k) == ~PartRule \/ TrueCrlCovers(c, k)
\* The received CRLs absence for c may be read from. Listings are read from rcvd, never from this.
RC(c) == {k \in rcvd : CrlCovers(c, k)}
\* The first received covering CRL beyond c's notAfter (0 if none).
LRcv(c) == IF \E k \in RC(c) : ctu[k] > na[c] THEN MinOf({k \in RC(c) : ctu[k] > na[c]}) ELSE 0
\* The E the CRLs of set S list for certificate c (NoE if none does).
KnownE(S, c) == IF S = {} THEN NoE ELSE MinOf({clst[k][c] : k \in S})
\* Published CRLs older than age: a fresh-enough cache cannot have missed their listing.
Old(age) == {k \in 1..np : ctu[k] <= now - age}
Seen(age) == rcvd \cup Old(age)

FreshSnap(age) == snap # 0 /\ now - ctu[snap] <= age
\* The CA tier: the root's CRLs list the CA (the chain below it) with the E the root gave. The E a set
\* of root CRLs lists (NoE if none), the root CRLs published longer ago than age (a fresh-enough
\* archive cannot have missed their listing), and a fresh one for d: received within age and, as for the
\* leaf, issued at or after A(d) (or live-counted).
KnownRE(S) == IF S = {} THEN NoE ELSE MinOf({rrl[k] : k \in S})
RSeen(age) == rrcvd \cup {k \in 1..rnp : rtu[k] <= now - age}
CAFresh(d, age) == \E k \in rrcvd : now - rtu[k] <= age /\ (rtu[k] >= AOf(d) \/ CountLive)
\* The series shows nothing was published between a CRL and its predecessor in the archive: with Cov
\* "number" the number follows; with Cov "time" it was issued within the declared Period (and the CA
\* publishes no more often than that). With Cov "none" nothing is shown. (The nextUpdate condition of
\* the Go rule guards against a certificateHold placed and released unseen; holds are not modelled.)
Follows(p, k) ==
  /\ Cov # "none"
  /\ (Cov = "number" => k = p + 1)
  /\ (Cov = "time" => ctu[k] <= ctu[p] + Period)
\* L(C): the first received CRL beyond notAfter, established when the archive has a predecessor it
\* follows. A CRL beyond notAfter is read for absence only if it is L(C) (RFC 5280 section 3.3: a CA
\* may have dropped the entry from any later one).
LOK(c, k) ==
  \/ ctu[k] <= na[c]
  \/ /\ k \in RC(c) /\ k = LRcv(c)
     /\ \E q \in RC(c) : q < k /\ (\A j \in RC(c) : j < k => j <= q) /\ Follows(q, k)
\* Usable for counting d: a cache, fresh (with Fresh), issued at or after A(d) (or live-counted).
UsableCRL(d, k, age) ==
  /\ k \in RC(dc[d]) /\ (~Fresh \/ now - ctu[k] <= age)
  /\ (ctu[k] >= AOf(d) \/ CountLive)
  /\ (~LBoundCount \/ LOK(dc[d], k))
StatusUsable(d) == (\E k \in rcvd : UsableCRL(d, k, MaxAge)) /\ (~CATier \/ ~CountCATier \/ CAFresh(d, MaxAgeCA))
\* Rule 5: the leaf has no E <= (A(d), or with RevByClaim the signed decided_at) in the status.
NotListed(d) ==
  /\ KnownE(rcvd, dc[d]) > IF RevByClaim THEN dt[d] ELSE AOf(d)
  /\ (~CATier \/ ~CountCATier \/ KnownRE(rrcvd) > AOf(d))

(***************************************************************************)
(* The counting rule (rules 1 to 5)                                        *)
(***************************************************************************)
ChainOK(d) ==
  LET c == dc[d] IN
  /\ c <= nc
  /\ (~IdBind \/ idn[c] = da[d])
  /\ (~NameCons \/ IdOwner[idn[c]] = hold[c])
  /\ nb[c] <= AOf(d)                        \* valid at A(d) (the leaf at min(A, na))
Window(d) ==
  LET c == dc[d] IN
  /\ nb[c] <= dt[d] /\ dt[d] <= na[c]       \* rule 2
  /\ dt[d] <= AOf(d) + Skew
  /\ AOf(d) <= na[c] + Grace                \* rule 3
Countable(d) ==
  /\ Covered(d) /\ HeadOK(d)
  /\ ChainOK(d) /\ Window(d)
  /\ StatusUsable(d) /\ NotListed(d)

\* A counted decision whose leaf the archive lists with E <= A(d).
Listed(d) == KnownE(rcvd, dc[d]) <= cA[d] \/ (CATier /\ KnownRE(rrcvd) <= cA[d])

Seats(S) == IF DedupSeat THEN Cardinality({da[d] : d \in S}) ELSE Cardinality(S)

(***************************************************************************)
(* The final tier (offline): a witness S for decision d                    *)
(***************************************************************************)
CovOK(d, S) ==
  LET a == AOf(d)
      F == {k \in RC(dc[d]) : ctu[k] <= a}
  IN \/ Cov = "none"
     \/ /\ F # {}
        /\ LET f == MaxOf(F) IN
           /\ ctu[f] + Period > a
           /\ f <= S
           /\ IF Cov = "number"
              THEN \A k \in f..S : k \in rcvd
              ELSE \A k1, k2 \in RC(dc[d]) :
                     (f <= k1 /\ k1 < k2 /\ k2 <= S /\ ~\E k3 \in RC(dc[d]) : k1 < k3 /\ k3 < k2)
                       => ctu[k2] <= ctu[k1] + Period
\* The CA tier of a final verdict: a received root CRL at least W after A(d), and no received root CRL
\* lists the CA with E <= A(d). The root never prunes, so a later CRL lists whatever an earlier one did
\* and a witness needs no coverage of the series between.
RootFinalOK(d) ==
  \/ ~CATier \/ ~FinalCATier
  \/ /\ \E kr \in rrcvd : AOf(d) + W <= rtu[kr]
     /\ \A k \in rrcvd : rrl[k] > AOf(d)
FinalWit(d, S) ==
  LET c == dc[d] a == AOf(d) IN
  /\ Covered(d) /\ HeadOK(d) /\ ChainOK(d) /\ Window(d)
  /\ S \in RC(c)
  /\ a + W <= ctu[S]
  /\ \A k \in rcvd : clst[k][c] > a                \* a listing anywhere in the series is a revocation
  /\ (ctu[S] > na[c] => S = LRcv(c))               \* absence beyond notAfter only from L(C)
  /\ CovOK(d, S)
  /\ RootFinalOK(d)

(***************************************************************************)
(* Initial state and actions                                               *)
(***************************************************************************)
Init ==
  /\ now = 0
  /\ nc = 0 /\ hold = [c \in Certs |-> CHOOSE p \in Persons : TRUE]
  /\ idn = [c \in Certs |-> CHOOSE i \in Idents : TRUE]
  /\ nb = [c \in Certs |-> 0] /\ na = [c \in Certs |-> 0] /\ rev = [c \in Certs |-> NoE]
  /\ np = 0 /\ ctu = [k \in CRLs |-> 0] /\ clst = [k \in CRLs |-> [c \in Certs |-> NoE]]
  /\ caRev = NoE /\ rnp = 0 /\ rtu = [k \in RCRLs |-> 0] /\ rrl = [k \in RCRLs |-> NoE] /\ rrcvd = {}
  /\ rcvd = {} /\ snap = 0
  /\ heads = {}
  /\ nd = 0 /\ da = [d \in Decs |-> CHOOSE i \in Idents : TRUE]
  /\ dc = [d \in Decs |-> 1] /\ dt = [d \in Decs |-> 0]
  /\ waiting = FALSE /\ cn = {} /\ dead = {} /\ tl = {} /\ execd = FALSE
  /\ cA = [d \in Decs |-> 0] /\ cRb = [d \in Decs |-> FALSE] /\ cNb = [d \in Decs |-> FALSE]
  /\ cOk = [d \in Decs |-> TRUE] /\ eb = FALSE /\ ebC = FALSE

Issue(p, i, e) ==
  /\ nc < MaxCert /\ now <= IssueMax /\ e \in NaSet /\ e >= now
  /\ nc' = nc + 1
  /\ hold' = [hold EXCEPT ![nc + 1] = p]
  /\ idn' = [idn EXCEPT ![nc + 1] = i]
  /\ nb' = [nb EXCEPT ![nc + 1] = now]
  /\ na' = [na EXCEPT ![nc + 1] = e]
  /\ UNCHANGED <<now, rev, np, ctu, clst, caRev, rnp, rtu, rrl, rrcvd, rcvd, snap, heads, nd, da, dc, dt, waiting, cn, dead, tl,
                 execd, cA, cRb, cNb, cOk, eb, ebC>>

\* The CA issues a certificate under its name constraint: the identifier is the holder's own.
IssueCert(p, i, e) == IdOwner[i] = p /\ Issue(p, i, e)
\* A certificate that breaks the constraint (a mis-issuance). Whether the verifier rejects it is
\* NameCons.
MisIssue(p, i, e) == CanMisIssue /\ IdOwner[i] # p /\ Issue(p, i, e)

\* The CA records a revocation with effective time E (which may be in the past).
Revoke(c, e) ==
  /\ CanRevoke /\ c <= nc /\ rev[c] = NoE /\ e \in 0..now
  /\ rev' = [rev EXCEPT ![c] = e]
  /\ UNCHANGED <<now, nc, hold, idn, nb, na, np, ctu, clst, caRev, rnp, rtu, rrl, rrcvd, rcvd, snap, heads, nd, da, dc, dt,
                 waiting, cn, dead, tl, execd, cA, cRb, cNb, cOk, eb, ebC>>

\* The entries a CRL published now carries. With pr, the CA drops the entries RFC 5280 section 3.3
\* lets it drop: a certificate whose notAfter an earlier CRL already follows.
Entries(pr) ==
  [c \in Certs |->
     IF c <= nc /\ rev[c] # NoE
        /\ (~Partitioned \/ CertPart(c) = CrlPart(np + 1))
        /\ ~(pr /\ now > na[c] /\ \E k \in 1..np : ctu[k] > na[c] /\ CrlPart(k) = CrlPart(np + 1))
     THEN rev[c] ELSE NoE]
CanPublish == np < MaxCRL /\ IF np = 0 THEN TRUE ELSE now - ctu[np] >= MinSpacing
Publish(pr) ==
  /\ CanPublish
  /\ np' = np + 1
  /\ ctu' = [ctu EXCEPT ![np + 1] = now]
  /\ clst' = [clst EXCEPT ![np + 1] = Entries(pr)]

\* A CRL is published and reaches the verifier: the cache takes it, the archive keeps it.
CRLPublish(pr) ==
  /\ (pr => Prune)
  /\ Publish(pr)
  /\ rcvd' = rcvd \cup {np + 1}
  /\ snap' = np + 1
  /\ UNCHANGED <<now, nc, hold, idn, nb, na, rev, caRev, rnp, rtu, rrl, rrcvd, heads, nd, da, dc, dt, waiting, cn, dead, tl,
                 execd, cA, cRb, cNb, cOk, eb, ebC>>

\* A CRL is published and the verifier never gets it (yet).
WithholdCRL(pr) ==
  /\ CanWithhold /\ (pr => Prune)
  /\ Publish(pr)
  /\ UNCHANGED <<now, nc, hold, idn, nb, na, rev, caRev, rnp, rtu, rrl, rrcvd, rcvd, snap, heads, nd, da, dc, dt, waiting, cn,
                 dead, tl, execd, cA, cRb, cNb, cOk, eb, ebC>>

\* An older published CRL is delivered after newer ones. With NumRule the cache keeps the higher
\* number; the archive keeps both.
RollbackCRL(k) ==
  /\ CanRollback /\ k \in 1..np /\ k # snap
  /\ rcvd' = rcvd \cup {k}
  /\ snap' = IF NumRule /\ snap > k THEN snap ELSE k
  /\ UNCHANGED <<now, nc, hold, idn, nb, na, rev, np, ctu, clst, caRev, rnp, rtu, rrl, rrcvd, heads, nd, da, dc, dt, waiting,
                 cn, dead, tl, execd, cA, cRb, cNb, cOk, eb, ebC>>

ClockTick ==
  /\ now < MaxT /\ now' = now + 1
  /\ UNCHANGED <<nc, hold, idn, nb, na, rev, np, ctu, clst, caRev, rnp, rtu, rrl, rrcvd, rcvd, snap, heads, nd, da, dc, dt,
                 waiting, cn, dead, tl, execd, cA, cRb, cNb, cOk, eb, ebC>>

\* A decision is signed with the key of certificate c, for approver a, claiming decided_at t.
\* Whether the key is its holder's or a thief's is not distinguished. Backdate: t is earlier than
\* the true time.
SubmitCert(c, a, t) ==
  /\ nd < MaxDec /\ c <= nc /\ (t = now \/ (CanBackdate /\ t < now))
  /\ (a = idn[c] \/ CanMisbind)
  /\ nd' = nd + 1
  /\ da' = [da EXCEPT ![nd + 1] = a]
  /\ dc' = [dc EXCEPT ![nd + 1] = c]
  /\ dt' = [dt EXCEPT ![nd + 1] = t]
  /\ UNCHANGED <<now, nc, hold, idn, nb, na, rev, np, ctu, clst, caRev, rnp, rtu, rrl, rrcvd, rcvd, snap, heads, waiting, cn,
                 dead, tl, execd, cA, cRb, cNb, cOk, eb, ebC>>

MakeHead(t) ==
  [sz |-> nd, tm |-> t, rl |-> now]

\* The application's own write path signs a head over the journal; it is blocked once the gate
\* waits (the application is waiting for the gate).
Anchor ==
  /\ ~waiting /\ nd > 0 /\ Cardinality(heads) < MaxHeads
  /\ ~\E h \in heads : h.sz = nd
  /\ heads' = heads \cup {MakeHead(now)}
  /\ UNCHANGED <<now, nc, hold, idn, nb, na, rev, np, ctu, clst, caRev, rnp, rtu, rrl, rrcvd, rcvd, snap, nd, da, dc, dt,
                 waiting, cn, dead, tl, execd, cA, cRb, cNb, cOk, eb, ebC>>

\* The gate's own step before tallying: it asks the store to sign a head over a journal no head
\* covers (AuditedStore.Reanchor).
GateAnchorStep ==
  /\ GateAnchor /\ waiting /\ nd > 0 /\ Cardinality(heads) < MaxHeads
  /\ ~Covered(nd)
  /\ heads' = heads \cup {MakeHead(now)}
  /\ UNCHANGED <<now, nc, hold, idn, nb, na, rev, np, ctu, clst, caRev, rnp, rtu, rrl, rrcvd, rcvd, snap, nd, da, dc, dt,
                 waiting, cn, dead, tl, execd, cA, cRb, cNb, cOk, eb, ebC>>

\* A head over the journal so far, stamped with a time that is not the real one.
SkewHead(t) ==
  /\ CanSkew /\ nd > 0 /\ Cardinality(heads) < MaxHeads
  /\ t \in Time /\ t # now
  /\ heads' = heads \cup {MakeHead(t)}
  /\ UNCHANGED <<now, nc, hold, idn, nb, na, rev, np, ctu, clst, caRev, rnp, rtu, rrl, rrcvd, rcvd, snap, nd, da, dc, dt,
                 waiting, cn, dead, tl, execd, cA, cRb, cNb, cOk, eb, ebC>>

GateOpen ==
  /\ ~waiting /\ waiting' = TRUE
  /\ UNCHANGED <<now, nc, hold, idn, nb, na, rev, np, ctu, clst, caRev, rnp, rtu, rrl, rrcvd, rcvd, snap, heads, nd, da, dc, dt,
                 cn, dead, tl, execd, cA, cRb, cNb, cOk, eb, ebC>>

\* The gate counts a decision.
Count(d) ==
  /\ waiting /\ tl = {} /\ d \in 1..nd /\ d \notin cn /\ (~DeadGuard \/ d \notin dead)
  /\ Countable(d)
  /\ cn' = cn \cup {d}
  /\ cA' = [cA EXCEPT ![d] = AOf(d)]
  /\ cRb' = [cRb EXCEPT ![d] = KnownE(Seen(MaxAge), dc[d]) <= AOf(d)
                              \/ (CATier /\ KnownRE(RSeen(MaxAgeCA)) <= AOf(d))]
  /\ cNb' = [cNb EXCEPT ![d] =
              \E h \in heads : h.sz < d /\ KnownE(Seen(MaxAge), dc[d]) <= h.tm]
  /\ cOk' = [cOk EXCEPT ![d] = (\E k \in rcvd : TrueCrlCovers(dc[d], k) /\ now - ctu[k] <= MaxAge /\ (ctu[k] >= AOf(d) \/ CountLive))
                                 /\ (~CATier \/ CAFresh(d, MaxAgeCA))]
  /\ UNCHANGED <<now, nc, hold, idn, nb, na, rev, np, ctu, clst, caRev, rnp, rtu, rrl, rrcvd, rcvd, snap, heads, nd, da, dc, dt,
                 waiting, dead, tl, execd, eb, ebC>>

\* A revocation with E <= A(d) in the archive removes a decision for good. A terminal tally is not
\* recounted (with StickyTally).
DropRevoked(d) ==
  /\ waiting /\ d \in cn /\ (tl = {} \/ ~StickyTally)
  /\ Listed(d)
  /\ dead' = dead \cup {d}
  /\ cn' = cn \ {d}
  /\ tl' = IF tl # {} THEN tl \ {d} ELSE tl
  /\ UNCHANGED <<now, nc, hold, idn, nb, na, rev, np, ctu, clst, caRev, rnp, rtu, rrl, rrcvd, rcvd, snap, heads, nd, da, dc, dt,
                 waiting, execd, cA, cRb, cNb, cOk, eb, ebC>>

\* Without StaleSticky: stale or missing status removes a counted decision.
DropStale(d) ==
  /\ ~StaleSticky /\ waiting /\ d \in cn /\ tl = {}
  /\ ~StatusUsable(d)
  /\ cn' = cn \ {d}
  /\ UNCHANGED <<now, nc, hold, idn, nb, na, rev, np, ctu, clst, caRev, rnp, rtu, rrl, rrcvd, rcvd, snap, heads, nd, da, dc, dt,
                 waiting, dead, tl, execd, cA, cRb, cNb, cOk, eb, ebC>>

Tally ==
  /\ waiting /\ tl = {} /\ Seats(cn) >= K
  /\ (~TallyRecheck \/ \A d \in cn : ~Listed(d))
  /\ tl' = cn
  /\ UNCHANGED <<now, nc, hold, idn, nb, na, rev, np, ctu, clst, caRev, rnp, rtu, rrl, rrcvd, rcvd, snap, heads, nd, da, dc, dt,
                 waiting, cn, dead, execd, cA, cRb, cNb, cOk, eb, ebC>>

\* The pre-execution check: a status no older than ExecAge, and no counted decision's chain with
\* E <= A(d) in the archive.
ExecOK ==
  /\ \A d \in tl : ~Listed(d) /\ \E k \in RC(dc[d]) : now - ctu[k] <= (IF LeafAgeFromCA THEN ExecAgeCA ELSE ExecAge) /\ (~LBoundExec \/ LOK(dc[d], k))
  /\ (~CATier \/ ~ExecCATier \/ \E k \in rrcvd : now - rtu[k] <= ExecAgeCA)

Exec ==
  /\ tl # {} /\ ~execd /\ (~ExecRecheck \/ ExecOK)
  /\ execd' = TRUE
  /\ eb' = (\E d \in tl : ~(\E k \in rcvd : TrueCrlCovers(dc[d], k) /\ now - ctu[k] <= ExecAge)
                        \/ KnownE(Seen(ExecAge), dc[d]) <= cA[d])
  /\ ebC' = (CATier /\ (~(\E k \in rrcvd : now - rtu[k] <= ExecAgeCA)
                        \/ \E d \in tl : KnownRE(RSeen(ExecAgeCA)) <= cA[d]))
  /\ UNCHANGED <<now, nc, hold, idn, nb, na, rev, np, ctu, clst, caRev, rnp, rtu, rrl, rrcvd, rcvd, snap, heads, nd, da, dc, dt,
                 waiting, cn, dead, tl, cA, cRb, cNb, cOk>>

\* The root revokes the CA with effective time E (which may lie in the past): every chain below it.
RevokeCA(e) ==
  /\ CATier /\ CanRevokeCA /\ caRev = NoE /\ e \in 0..now
  /\ caRev' = e
  /\ UNCHANGED <<now, nc, hold, idn, nb, na, rev, np, ctu, clst, rnp, rtu, rrl, rrcvd, rcvd, snap, heads, nd, da, dc, dt,
                 waiting, cn, dead, tl, execd, cA, cRb, cNb, cOk, eb, ebC>>

CanPublishRoot == CATier /\ rnp < MaxRCRL /\ IF rnp = 0 THEN TRUE ELSE now - rtu[rnp] >= RootSpacing
PublishRoot ==
  /\ CanPublishRoot
  /\ rnp' = rnp + 1
  /\ rtu' = [rtu EXCEPT ![rnp + 1] = now]
  /\ rrl' = [rrl EXCEPT ![rnp + 1] = caRev]

\* A root CRL is published and the verifier gets it, or (withheld) never does.
RootCRL ==
  /\ PublishRoot /\ rrcvd' = rrcvd \cup {rnp + 1}
  /\ UNCHANGED <<now, nc, hold, idn, nb, na, rev, np, ctu, clst, caRev, rcvd, snap, heads, nd, da, dc, dt,
                 waiting, cn, dead, tl, execd, cA, cRb, cNb, cOk, eb, ebC>>
WithholdRootCRL ==
  /\ CanWithhold /\ PublishRoot
  /\ UNCHANGED <<now, nc, hold, idn, nb, na, rev, np, ctu, clst, caRev, rrcvd, rcvd, snap, heads, nd, da, dc, dt,
                 waiting, cn, dead, tl, execd, cA, cRb, cNb, cOk, eb, ebC>>

\* The gate crashes and restarts: the counted set and the wait are lost; the journal (the heads and the
\* terminal tally) and the archive are not. The decisions are counted again.
Crash ==
  /\ CanCrash /\ (waiting \/ cn # {})
  /\ waiting' = FALSE /\ cn' = {}
  /\ UNCHANGED <<now, nc, hold, idn, nb, na, rev, np, ctu, clst, caRev, rnp, rtu, rrl, rrcvd, rcvd, snap, heads, nd, da, dc, dt,
                 dead, tl, execd, cA, cRb, cNb, cOk, eb, ebC>>

Next ==
  \/ ClockTick
  \/ \E p \in Persons, i \in Idents, e \in Time : IssueCert(p, i, e) \/ MisIssue(p, i, e)
  \/ \E c \in Certs, e \in Time : Revoke(c, e)
  \/ \E pr \in BOOLEAN : CRLPublish(pr) \/ WithholdCRL(pr)
  \/ \E k \in CRLs : RollbackCRL(k)
  \/ \E c \in Certs, a \in Idents, t \in Time : SubmitCert(c, a, t)
  \/ Anchor \/ GateAnchorStep
  \/ \E t \in Time : SkewHead(t)
  \/ GateOpen
  \/ \E d \in Decs : Count(d) \/ DropRevoked(d) \/ DropStale(d)
  \/ Tally
  \/ Exec
  \/ Crash
  \/ \E e \in Time : RevokeCA(e)
  \/ RootCRL \/ WithholdRootCRL

Spec == Init /\ [][Next]_vars

\* Fairness for the liveness check: the gate's anchor step and the application's write path.
SpecLive == Spec /\ WF_vars(GateAnchorStep) /\ WF_vars(Anchor)

(***************************************************************************)
(* Properties                                                              *)
(***************************************************************************)
TypeOK ==
  /\ now \in Time /\ nc \in 0..MaxCert /\ np \in 0..MaxCRL /\ nd \in 0..MaxDec
  /\ rcvd \subseteq CRLs /\ snap \in 0..MaxCRL
  /\ cn \subseteq Decs /\ dead \subseteq Decs /\ tl \subseteq Decs
  /\ \A c \in Certs : rev[c] \in 0..NoE /\ nb[c] \in Time /\ na[c] \in Time
  /\ \A h \in heads : h.sz \in 1..MaxDec /\ h.tm \in Time /\ h.rl \in Time
  /\ waiting \in BOOLEAN /\ execd \in BOOLEAN
  /\ rnp \in 0..MaxRCRL /\ rrcvd \subseteq RCRLs /\ caRev \in 0..NoE

Counted == cn \cup tl

\* A counted decision's leaf carries exactly the approver's identifier, was valid at A(d), and the
\* signed time was plausible.
IdentityBound ==
  \A d \in Counted :
    LET c == dc[d] IN
    /\ idn[c] = da[d]
    /\ nb[c] <= cA[d] /\ cA[d] <= na[c] + Grace
    /\ nb[c] <= dt[d] /\ dt[d] <= na[c] /\ dt[d] <= cA[d] + Skew

\* No CRL the verifier received, or published longer ago than the status may be old, lists a
\* counted decision's certificate with E <= A(d) at the moment it was counted.
RevocationSound == \A d \in Counted : ~cRb[d]

\* Unknown, stale or out-of-window status never counts.
FailClosed == \A d \in Counted : cOk[d]

\* A decision written after a head that already carries a time at or after the revocation never
\* counts, whatever its decided_at or a later head's time says.
NoBackdate == \A d \in Counted : ~cNb[d]

\* A passing tally has K distinct people behind it (given IdOwner is injective and the name
\* constraint holds).
SeatPerPerson ==
  tl # {} => Cardinality({hold[dc[d]] : d \in tl}) >= K

\* A passing tally has K distinct identifiers behind it.
SeatPerIdentifier ==
  tl # {} => Cardinality({da[d] : d \in tl}) >= K

\* Action properties.
\* A crash loses the counted set (the decisions are counted again).
Crashed == waiting /\ ~waiting'
TerminalStable == [][tl # {} => tl' = tl]_vars
StaleNeverUncounts ==
  [][\A d \in Decs : (d \in cn /\ d \notin dead' /\ ~Crashed) => d \in cn']_vars

\* The call never ran on stale status, or while a counted decision's chain showed E <= A(d) in
\* status the verifier had (or that was old enough to have).
NoExecAfterKnownRevocation == execd => ~eb /\ ~ebC

\* The same per tier: the leaf's issuer is read at ExecAge, the CA above it at ExecAgeCA (its own,
\* longer age: the root publishes rarely), and each must be fresh in its own series and not list its
\* certificate with E <= A(d).
NoExecAfterLeafTierRevocation == execd => ~eb
NoExecAfterCATierRevocation == execd => ~ebC

\* The final tier is sound against the CA's whole published series: nothing published up to the
\* witness's thisUpdate lists the chain with E <= A(d).
FinalNeedsCompleteSeries ==
  \A d \in 1..nd, S \in 1..np :
    FinalWit(d, S) =>
      /\ \A k \in 1..np : ctu[k] <= ctu[S] => clst[k][dc[d]] > AOf(d)
      \* and the root: no root CRL published up to a witness root CRL lists the CA
      /\ (CATier /\ FinalCATier => \E kr \in rrcvd : AOf(d) + W <= rtu[kr])
      /\ (CATier => \A j \in 1..rnp :
            (\E kr \in rrcvd : AOf(d) + W <= rtu[kr] /\ rtu[j] <= rtu[kr]) => rrl[j] > AOf(d))

\* Absence is evidence only from a CRL no later than L(C), the first published after notAfter.
AbsenceOnlyInWindow ==
  \A d \in 1..nd, S \in 1..np :
    FinalWit(d, S) =>
      \/ ctu[S] <= na[dc[d]]
      \/ \E L \in 1..np : TrueCrlCovers(dc[d], L) /\ ctu[L] > na[dc[d]] /\ ctu[S] <= ctu[L]
                          /\ \A j \in 1..np : (TrueCrlCovers(dc[d], j) /\ ctu[j] > na[dc[d]]) => ctu[L] <= ctu[j]

\* A tally forms only from decisions none of which the archive lists with E <= A(d) at that moment.
TallyClean ==
  [][(tl = {} /\ tl' # {}) =>
       \A d \in tl' : /\ KnownE(rcvd', dc[d]) > cA[d]
                     /\ (~CATier \/ KnownRE(rrcvd') > cA[d])]_vars

\* A decision a revocation removed is in neither the counted set nor the tally.
DeadStaysOut == dead \cap Counted = {}

\* Liveness, and only this: a decision written while the gate waits is eventually covered by a head,
\* so the gate's anchor step supplies the head a direct write lacks. It does not say the decision is
\* then counted: a covering head that contradicts another (NonDec) or status that never arrives
\* leaves the gate paused by design.
EventuallyCovered == \A d \in Decs : [](d <= nd => <>Covered(d))

\* Vacuity (check.sh): the call is reachable.
EffectNotReachable == ~execd

\* Reachability, expected violated (the configurations id-*-reach): the paths the properties
\* guard are not empty.
\* A decision is final.
FinalNotReachable == ~\E d \in 1..nd, S \in 1..np : FinalWit(d, S)
\* A revocation removes a counted decision.
DropNotReachable == dead = {}
\* A tally rests on two seats.
TwoSeatsNotReachable == ~(tl # {} /\ Cardinality(tl) >= 2)
\* A decision counts although a head carries a time that is not the real one.
SkewedCountNotReachable == ~(Counted # {} /\ \E h \in heads : h.tm # h.rl)
\* The gate crashed after the tally formed (the wait is lost, the tally is not).
CrashAfterTallyNotReachable == ~(tl # {} /\ ~waiting)
\* The call ran after a crash.
ExecAfterCrashNotReachable == ~(execd /\ ~waiting)
\* A decision counts although the verifier holds a CRL that does not cover its certificate (the other partition).
PartitionSkipNotReachable == ~(Counted # {} /\ \E d \in Counted, k \in rcvd : ~TrueCrlCovers(dc[d], k))
\* A withheld or rolled-back CRL leaves the cache behind the CA's series, and a decision counts.
BehindCountNotReachable == ~(Counted # {} /\ snap # np)
=============================================================================
