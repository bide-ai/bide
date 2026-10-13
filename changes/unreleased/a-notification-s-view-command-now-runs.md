Section: Fixed

A notification's `view.command` was `bide view -run <id>`, which fails because `bide view` needs a store. The webhook notifier now takes `notify.WithViewDB(path)` and the command is `bide view -db <path> -run <id>`; without it the command is the marked template `bide view -db <journal.db> -run <id>` rather than one that fails when pasted. The notifications guide and its Slack adapter say so.
