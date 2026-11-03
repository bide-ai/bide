Section: Changed

The cross-SDK crash benchmark was re-run on the latest stable releases (adk-go v2.7.0, trpc-agent-go v1.11.2, langchaingo v0.1.15, eino v0.9.22, LangGraph 1.2.19) and gave the same counts as before: bide holds at most once (`maxFired=1` over 209 schedules) and every other framework charges again. The README results card now names the exact version of each framework and the commit bide was measured at, and `assets/demo/chaos-card` holds the data and a script that rebuild it.
