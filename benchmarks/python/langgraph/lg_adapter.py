"""chaos.System adapter for LangGraph with a SqliteSaver checkpointer and REAL crashes.

Each Step forks a child process (langgraph is already imported in the parent, so a
fork is cheap) that opens the run's SQLite checkpoint database and runs or resumes
the graph. The checkpointer is a SqliteSaver whose put and put_writes first report
to the parent over a pipe and block until the parent answers. At the crashAt-th
checkpoint write the parent sends SIGKILL instead of an answer, so the process dies
before that write reaches SQLite, with no cleanup, no exception handlers, no
atexit. This matches chaos.Run: "crashing at the crashAt-th write" means that write
never persisted and nothing after it ran.

The side effect ("charge") appends one line to a file outside the checkpoint store
and fsyncs it before returning, so the count survives the SIGKILL. Fired() counts
those lines.
"""

import operator
import os
import shutil
import signal
import sqlite3
import tempfile
import threading
import traceback
from typing import Annotated, TypedDict

from langgraph.cache.sqlite import SqliteCache
from langgraph.checkpoint.sqlite import SqliteSaver
from langgraph.func import entrypoint, task
from langgraph.graph import END, START, StateGraph
from langgraph.types import CachePolicy

THREAD = {"configurable": {"thread_id": "chaos"}}

# Each variant is one documented way to write the start -> charge workflow.
#   node:       StateGraph, the side effect runs directly in the "charge" node.
#   task:       StateGraph, the "charge" node runs the side effect inside a @task,
#               as the docs recommend for side effects.
#   functional: Functional API @entrypoint calling a start @task then a charge @task,
#               the docs' "Handling side effects" pattern.
#   cache:      node, plus a node CachePolicy backed by a durable SqliteCache.
VARIANTS = ("node", "task", "functional", "cache")
DURABILITY = ("sync", "async", "exit")


class State(TypedDict):
    steps: Annotated[list[str], operator.add]


class HookedSaver(SqliteSaver):
    """A SqliteSaver that hands control to the parent before every checkpoint write.

    hook(label, write) reports the write, waits for the parent, then performs it."""

    def __init__(self, conn, hook):
        super().__init__(conn)
        self._hook = hook

    def put(self, config, checkpoint, metadata, new_versions):
        step = metadata.get("step")
        return self._hook(
            f"put(step={step})", lambda: super(HookedSaver, self).put(config, checkpoint, metadata, new_versions)
        )

    def put_writes(self, config, writes, task_id, task_path=""):
        chans = ",".join(sorted({c for c, _ in writes}))
        return self._hook(
            f"put_writes({chans})", lambda: super(HookedSaver, self).put_writes(config, writes, task_id, task_path)
        )


class HookedCache(SqliteCache):
    """A SqliteCache whose writes are crash points too: every durable write counts."""

    def __init__(self, path, hook):
        super().__init__(path=path)
        self._hook = hook

    def set(self, mapping):
        return self._hook("cache_set", lambda: super(HookedCache, self).set(mapping))


def _append(path: str) -> None:
    # One durable line per call, fsynced before returning so it survives SIGKILL.
    with open(path, "a") as f:
        f.write("x\n")
        f.flush()
        os.fsync(f.fileno())


def build(variant: str, saver, dirpath: str, cache=None):
    # charges.log counts the non-idempotent side effect; starts.log counts runs of the
    # start step, which the fairness tests use to show resume skips completed work.
    charges = os.path.join(dirpath, "charges.log")
    starts = os.path.join(dirpath, "starts.log")

    if variant == "functional":

        @task
        def start_task() -> str:
            _append(starts)
            return "start"

        @task
        def charge_task() -> str:
            _append(charges)
            return "charge"

        @entrypoint(checkpointer=saver)
        def workflow(_: dict) -> list[str]:
            return [start_task().result(), charge_task().result()]

        return workflow

    @task
    def charge_task() -> str:
        _append(charges)
        return "charge"

    def start(_: State) -> dict:
        _append(starts)
        return {"steps": ["start"]}

    def charge(_: State) -> dict:
        if variant == "task":
            return {"steps": [charge_task().result()]}
        _append(charges)
        return {"steps": ["charge"]}

    g = StateGraph(State)
    g.add_node("start", start)
    if variant == "cache":
        g.add_node("charge", charge, cache_policy=CachePolicy())
    else:
        g.add_node("charge", charge)
    g.add_edge(START, "start")
    g.add_edge("start", "charge")
    g.add_edge("charge", END)
    return g.compile(checkpointer=saver, cache=cache)


def run_once(variant: str, durability: str, dirpath: str, hook) -> None:
    """Run the graph, or resume it the documented way if this thread has a checkpoint."""
    conn = sqlite3.connect(os.path.join(dirpath, "checkpoints.db"), check_same_thread=False)
    saver = HookedSaver(conn, hook)
    cache = None
    if variant == "cache":
        cache = HookedCache(os.path.join(dirpath, "cache.db"), hook)
    graph = build(variant, saver, dirpath, cache)
    if graph.get_state(THREAD).created_at is None:
        inp = {} if variant == "functional" else {"steps": []}
        graph.invoke(inp, THREAD, durability=durability)
    else:
        # Resume from the last checkpoint: invoke with None on the same thread_id.
        graph.invoke(None, THREAD, durability=durability)


def _child(variant, durability, dirpath, ev_w, ctl_r) -> None:
    # LangGraph issues writes from background threads, so two can be in flight at once.
    # Holding one lock across the hand-off AND the write makes every write that was
    # allowed commit before the next one is reported: a crash at write K then means
    # exactly "writes 1..K-1 persisted, K and later did not", the chaos.Run model.
    # SqliteSaver already serializes its writes on one connection lock, so this only
    # fixes the order in which the parent sees them.
    lock = threading.Lock()

    def hook(kind: str, write):
        with lock:
            os.write(ev_w, (kind + "\n").encode())
            if os.read(ctl_r, 1) != b"g":
                os._exit(3)
            return write()

    try:
        run_once(variant, durability, dirpath, hook)
    except BaseException:
        traceback.print_exc()
        os._exit(1)
    os._exit(0)


class LangGraphRun:
    def __init__(self, variant: str, durability: str, root: str | None = None):
        self.variant = variant
        self.durability = durability
        self.dir = tempfile.mkdtemp(prefix="run-", dir=root)
        self.trace: list[list[str]] = []  # the checkpoint writes each Step made

    def _count(self, name: str) -> int:
        try:
            with open(os.path.join(self.dir, name)) as f:
                return sum(1 for _ in f)
        except FileNotFoundError:
            return 0

    def fired(self) -> int:
        return self._count("charges.log")

    def starts(self) -> int:
        return self._count("starts.log")

    def step(self, crash_at: int) -> bool:
        return self.step_until(lambda events: crash_at > 0 and len(events) == crash_at)

    def step_until(self, crash) -> bool:
        """Run or resume once, SIGKILLing the child at the first checkpoint write for
        which crash(events so far, including this one) is true."""
        ev_r, ev_w = os.pipe()
        ctl_r, ctl_w = os.pipe()
        pid = os.fork()
        if pid == 0:
            os.close(ev_r)
            os.close(ctl_w)
            _child(self.variant, self.durability, self.dir, ev_w, ctl_r)
        os.close(ev_w)
        os.close(ctl_r)
        events: list[str] = []
        crashed = False
        with os.fdopen(ev_r) as ev:
            for line in ev:
                events.append(line.strip())
                if crash(events):
                    os.kill(pid, signal.SIGKILL)
                    crashed = True
                    break
                os.write(ctl_w, b"g")
        # Reap before closing the control pipe, so the child dies of SIGKILL while
        # still blocked on it rather than seeing EOF.
        _, status = os.waitpid(pid, 0)
        os.close(ctl_w)
        self.trace.append(events)
        if crashed:
            if not (os.WIFSIGNALED(status) and os.WTERMSIG(status) == signal.SIGKILL):
                raise RuntimeError(f"child not killed by SIGKILL: status={status}")
            return True
        if os.waitstatus_to_exitcode(status) != 0:
            raise RuntimeError(f"child failed: status={status} events={events}")
        return False

    def close(self) -> None:
        shutil.rmtree(self.dir, ignore_errors=True)


# The durable writes (checkpoint put + put_writes, plus cache sets for "cache") a clean
# run makes with langgraph 1.2.12 / langgraph-checkpoint-sqlite 3.1.1. Measured; checked
# by test_fairness.py::test_writes_match_a_clean_run. It is the sweep bound, as Writes()
# is in the Go harness.
WRITES = {
    ("node", "sync"): 7,
    ("node", "async"): 7,
    ("node", "exit"): 1,
    ("task", "sync"): 8,
    ("task", "async"): 8,
    ("task", "exit"): 1,
    ("functional", "sync"): 5,
    ("functional", "async"): 5,
    ("functional", "exit"): 1,
    ("cache", "sync"): 8,
    ("cache", "async"): 8,
    ("cache", "exit"): 2,
}


class LangGraph:
    """chaos.System for one (variant, durability) configuration."""

    def __init__(self, variant: str = "node", durability: str = "sync"):
        self.variant = variant
        self.durability = durability
        self.root = tempfile.mkdtemp(prefix="lgchaos-")

    def new_run(self) -> LangGraphRun:
        return LangGraphRun(self.variant, self.durability, self.root)

    def close(self) -> None:
        shutil.rmtree(self.root, ignore_errors=True)

    def writes(self) -> int:
        return WRITES[(self.variant, self.durability)]
