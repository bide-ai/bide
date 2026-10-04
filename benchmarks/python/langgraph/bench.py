"""Run the chaos benchmark against LangGraph in every variant and durability mode.

    uv run python bench.py [--seeds 200]

Prints one row per configuration, in the Go harness's row format. The "langgraph" row is
the trpc.go shape (start -> charge, side effect in the node) under durability="sync",
LangGraph's most durable mode, on SqliteSaver; "langgraph-pg" is the same on PostgresSaver
in a throwaway database (skipped when no Postgres is reachable, see lg_adapter.PG_ADMIN_DSN).
"""

import argparse
import importlib.metadata as md

from chaos import verify
from lg_adapter import DURABILITY, VARIANTS, WRITES, LangGraph, LangGraphPostgres, postgres_unavailable


def main() -> None:
    ap = argparse.ArgumentParser()
    ap.add_argument("--seeds", type=int, default=200)
    args = ap.parse_args()
    print(
        f"langgraph=={md.version('langgraph')} "
        f"langgraph-checkpoint-sqlite=={md.version('langgraph-checkpoint-sqlite')} "
        f"langgraph-checkpoint=={md.version('langgraph-checkpoint')} "
        f"langgraph-checkpoint-postgres=={md.version('langgraph-checkpoint-postgres')}"
    )
    no_pg = postgres_unavailable()
    print(f"  {run('langgraph', LangGraph, 'node', 'sync', args.seeds)}")
    if no_pg:
        print(f"  langgraph-pg     skipped: {no_pg}")
    else:
        print(f"  {run('langgraph-pg', LangGraphPostgres, 'node', 'sync', args.seeds)}")
    for backend, cls in (("sqlite", LangGraph), ("postgres", LangGraphPostgres)):
        if backend == "postgres" and no_pg:
            continue
        print(f"all configurations on {backend} (variant/durability):")
        for variant in VARIANTS:
            for durability in DURABILITY:
                rep = run(f"lg/{variant}/{durability}", cls, variant, durability, args.seeds)
                print(f"  {rep}  writes={WRITES[(variant, durability)]}", flush=True)


def run(name: str, cls, variant: str, durability: str, seeds: int):
    sys = cls(variant, durability)
    try:
        return verify(name, sys, seeds)
    finally:
        sys.close()


if __name__ == "__main__":
    main()
