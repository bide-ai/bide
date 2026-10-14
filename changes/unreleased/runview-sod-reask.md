Section: Fixed

`bide view` and `bide approve` show a gate whose recorded tally passed without excluding the requester, under a policy that now excludes it, as waiting for approval again (`agent.PendingSoDReapproval`), with the requester's decision not counted, instead of as decided.
