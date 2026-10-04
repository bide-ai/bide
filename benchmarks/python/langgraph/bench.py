"""Run the chaos benchmark against LangGraph in every variant and durability mode.

    uv run python bench.py [--seeds 200]

Prints one row per configuration, in the Go harness's row format. The "langgraph" row is
the trpc.go shape (start -> charge, side effect in the node) under durability="sync",
LangGraph's most durable mode.
"""

import argparse
import importlib.metadata as md

from chaos import verify
from lg_adapter import DURABILITY, VARIANTS, WRITES, LangGraph


def main() -> None:
    ap = argparse.ArgumentParser()
    ap.add_argument("--seeds", type=int, default=200)
    args = ap.parse_args()
    print(
        f"langgraph=={md.version('langgraph')} "
        f"langgraph-checkpoint-sqlite=={md.version('langgraph-checkpoint-sqlite')} "
        f"langgraph-checkpoint=={md.version('langgraph-checkpoint')}"
    )
    print(f"  {run('langgraph', 'node', 'sync', args.seeds)}")
    print("all configurations (variant/durability):")
    for variant in VARIANTS:
        for durability in DURABILITY:
            rep = run(f"lg/{variant}/{durability}", variant, durability, args.seeds)
            print(f"  {rep}  writes={WRITES[(variant, durability)]}", flush=True)


def run(name: str, variant: str, durability: str, seeds: int):
    sys = LangGraph(variant, durability)
    try:
        return verify(name, sys, seeds)
    finally:
        sys.close()


if __name__ == "__main__":
    main()
