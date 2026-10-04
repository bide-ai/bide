package main

// Rules returns every rewrite class, in the order they run: each is a pass of its own over the
// code the passes before it left (see MigrateModule). The passes that change the type of a
// variable's definition run last (journal retypes stores, construct makes New return an error),
// so the passes before them still type-check the variables they rewrite around.
func Rules() []Rule {
	return []Rule{
		{
			Name:  "rename",
			Doc:   "package mcp -> mcptools; the scripted model -> package agenttest",
			Visit: visitRename,
		},
		{
			Name:  "tools",
			Doc:   "Func(name, desc, safety, fn) -> MustFunc(name, desc, fn, WithSafety(safety)); CompensatedFunc/SubAgent/RetrievalTool -> Must twins; SpecOf(t) and the old Tool methods -> t.Spec(); Spec methods for old implementers; plan Register* -> *Registry methods",
			Visit: visitTools,
		},
		{
			Name:  "run",
			Doc:   "the string entry points (Run, RunSaga, RunResult, Stream, Session.Send, RunTyped, ...) -> the Run API (Run(ctx, id, UserText(s), WithSaga()), *Result); transitional run names -> final; AgentStream/AgentEvent -> RunStream/RunEvent; audit.Record -> RecordStream",
			Visit: visitRun,
		},
		{
			Name:  "pause",
			Doc:   "pause aliases -> final types; ResolveHaltRef/ResolveHalt/ResolveStepHalt -> ResolveHalt(ctx, j, HaltRef, Outcome); Resume/Send -> AnswerInterrupt/Enqueue; journal verbs -> *Journal methods; context decorators on a run's context -> run options",
			Visit: visitPause,
		},
		{
			Name: "journal",
			Doc:  "the Durable interface -> *Journal; a store where a Durable was expected -> a Journal over it (NewJournal); store Do/History/Journal shims -> the journal's",
			Plan: planJournal, Planned: finishJournalPlan,
			Visit: visitJournal,
		},
		{
			Name:  "construct",
			Doc:   "New(model, store, tools...) and the builder methods -> New(model, j, WithTools(tools...), options...), which returns an error; Build -> New",
			Visit: visitConstruct,
		},
	}
}
