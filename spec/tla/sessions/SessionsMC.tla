----------------------------- MODULE SessionsMC -----------------------------
(* Named layouts of handles, processes and callers, which the configurations select with <-. *)
EXTENDS Sessions

CONSTANTS h1, h2, c1, c2, c3

\* Handles: two in two processes (two workers), or one handle shared by every caller.
H2 == {h1, h2}
H1 == {h1}
Proc2 == (h1 :> "p1" @@ h2 :> "p2")
Proc1 == (h1 :> "p1")
OnChat2 == (h1 :> "chat" @@ h2 :> "chat")
OnChat1 == (h1 :> "chat")
\* Two sessions whose ids differ by a suffix the old run-id scheme could spell: "chat" and
\* "chat/e" (#56).
OnTwo == (h1 :> "chat" @@ h2 :> "chat/e")

C2 == {c1, c2}
C3 == {c1, c2, c3}
NoKey == [c \in C3 |-> "none"]
NoRoot == [c \in C3 |-> "none"]

\* One message "x" delivered to two workers, and a second message "y" on the first worker.
TwoWorkersH == (c1 :> h1 @@ c2 :> h2 @@ c3 :> h1)
SendXXY     == (c1 :> "send" @@ c2 :> "send" @@ c3 :> "send")
MsgXXY      == (c1 :> "x" @@ c2 :> "x" @@ c3 :> "y")

\* Every caller on one handle: "x" twice (a redelivery, or a retry while the first runs), and a
\* SendOnce of "z".
SharedH     == (c1 :> h1 @@ c2 :> h1 @@ c3 :> h1)
SendXXOnceZ == (c1 :> "send" @@ c2 :> "send" @@ c3 :> "once")
MsgXXZ      == (c1 :> "x" @@ c2 :> "x" @@ c3 :> "z")
KeyZ        == (c1 :> "none" @@ c2 :> "none" @@ c3 :> "k2")

\* SendOnce: key k1 ("x") delivered to two workers, and key k2 ("y").
OnceAll  == (c1 :> "once" @@ c2 :> "once" @@ c3 :> "once")
KeyK1K1K2 == (c1 :> "k1" @@ c2 :> "k1" @@ c3 :> "k2")
OnceH    == (c1 :> h1 @@ c2 :> h2 @@ c3 :> h2)
SharedOnceH == (c1 :> h1 @@ c2 :> h1 @@ c3 :> h1)

\* Two messages on two workers, one each (SendOnce, distinct keys).
TwoMsgH   == (c1 :> h1 @@ c2 :> h2)
OnceTwo   == (c1 :> "once" @@ c2 :> "once")
MsgXY     == (c1 :> "x" @@ c2 :> "y")
KeyK1K2   == (c1 :> "k1" @@ c2 :> "k2")
SendTwo   == (c1 :> "send" @@ c2 :> "send")
NoKey2    == [c \in C2 |-> "none"]
NoRoot2   == [c \in C2 |-> "none"]

\* Run-id collisions (#86, #56): a root Run("chat/t0") beside session "chat"'s first Send, and
\* session "chat/e"'s first Send beside session "chat"'s SendOnce of key "t0".
RootTurnH   == (c1 :> h1 @@ c2 :> "none" @@ c3 :> h2)
RootTurnOp  == (c1 :> "send" @@ c2 :> "root" @@ c3 :> "once")
RootTurnMsg == (c1 :> "x" @@ c2 :> "w" @@ c3 :> "v")
RootTurnKey == (c1 :> "none" @@ c2 :> "none" @@ c3 :> "t0")
RootTurnRun == (c1 :> "none" @@ c2 :> "chat/t0" @@ c3 :> "none")

\* Two callers. A message delivered to two workers ("x" on h1 and h2), or sent twice on one handle.
XXWorkersH == (c1 :> h1 @@ c2 :> h2)
XXSharedH  == (c1 :> h1 @@ c2 :> h1)
SendSend   == (c1 :> "send" @@ c2 :> "send")
OnceOnce   == (c1 :> "once" @@ c2 :> "once")
MsgXX      == (c1 :> "x" @@ c2 :> "x")
KeyK1K1    == (c1 :> "k1" @@ c2 :> "k1")
\* Run-id collisions with two callers: session "chat"'s first Send and a root Run("chat/t0");
\* session "chat/e"'s first Send (on h2) and session "chat"'s SendOnce of key "t0" (on h1).
RootH2     == (c1 :> h1 @@ c2 :> "none")
SendRoot   == (c1 :> "send" @@ c2 :> "root")
MsgXW      == (c1 :> "x" @@ c2 :> "w")
RootRun2   == (c1 :> "none" @@ c2 :> "chat/t0")
SessPairH  == (c1 :> h2 @@ c2 :> h1)
SendOnceOp == (c1 :> "send" @@ c2 :> "once")
KeyT0      == (c1 :> "none" @@ c2 :> "t0")
\* One caller.
C1      == {c1}
OnlyH1  == (c1 :> h1)
OnlySend == (c1 :> "send")
OnlyOnce == (c1 :> "once")
OnlyX    == (c1 :> "x")
OnlyK1   == (c1 :> "k1")
OnlyNone == (c1 :> "none")
=============================================================================
