"""Fairness checks for the LangGraph adapter, the counterpart of benchmarks/*fairness_test.go.

They show the adapter gives LangGraph its best documented behaviour: a clean run charges
once, resuming a finished run is a no-op, resume really continues from the checkpoint
(completed steps are not re-run), and the crash schedules match the Go harness.

    uv run pytest -q

Every check runs on SqliteSaver and on PostgresSaver; the Postgres ones skip when no
server is reachable (see lg_adapter.PG_ADMIN_DSN).
"""

import itertools

import pytest

from chaos import PCG, verify
from lg_adapter import DURABILITY, VARIANTS, WRITES, LangGraph, LangGraphPostgres, postgres_unavailable

CONFIGS = list(itertools.product(VARIANTS, DURABILITY))


@pytest.fixture(params=["sqlite", "postgres"])
def backend(request):
    if request.param == "postgres" and (why := postgres_unavailable()):
        pytest.skip(why)
    return request.param


@pytest.fixture
def system(request, backend):
    sys = (LangGraph if backend == "sqlite" else LangGraphPostgres)(*request.param)
    yield sys
    sys.close()


@pytest.mark.parametrize("system", CONFIGS, indirect=True, ids=lambda c: "/".join(c))
def test_clean_run_charges_once_and_resume_of_complete_run_is_noop(system):
    run = system.new_run()
    assert run.step(0) is False, "clean run should not crash"
    assert run.fired() == 1, f"clean run fired {run.fired()}, want 1"
    # Resume the already-complete thread: LangGraph must see it is done and not re-run charge.
    assert run.step(0) is False
    assert run.fired() == 1, "resuming a COMPLETE run re-fired the charge: the adapter restarts, not resumes"
    assert run.trace[1] == [], f"resuming a complete run wrote checkpoints: {run.trace[1]}"


@pytest.mark.parametrize("system", CONFIGS, indirect=True, ids=lambda c: "/".join(c))
def test_crash_before_charge_then_resume_charges_once(system):
    # Kill at every checkpoint write that comes before the side effect has run, then
    # resume: the charge must fire exactly once.
    crashed_before_charge = 0
    for k in range(1, system.writes() + 1):
        run = system.new_run()
        if not run.step_until(lambda events: len(events) == k and run.fired() == 0):
            continue  # the charge had already run by write k
        crashed_before_charge += 1
        while run.step(0):
            pass
        assert run.fired() == 1, f"crash at write {k} (before the charge) then resume fired {run.fired()}, want 1"
    if system.durability != "exit":
        assert crashed_before_charge >= 1


# In these configurations a checkpoint write falls between the start step's persisted
# result and the charge; in the others (durability="async", the functional API) the
# charge already runs before the next write. See README.
@pytest.mark.parametrize(
    "system",
    [(v, "sync") for v in ("node", "task", "cache")],
    indirect=True,
    ids=lambda c: "/".join(c),
)
def test_resume_continues_from_checkpoint_and_skips_completed_start(system):
    # Kill after the start step's result persisted and before the charge ran. Resume
    # must not re-run start (it continues from the checkpoint, it does not restart the
    # thread) and must charge exactly once.
    start_done = ("put_writes(branch:to:charge,steps)", "put(step=1)")
    run = system.new_run()
    assert run.step_until(lambda events: any(e in start_done for e in events[:-1]) and run.fired() == 0)
    assert (run.starts(), run.fired()) == (1, 0)
    while run.step(0):
        pass
    assert run.fired() == 1
    assert run.starts() == 1, "resume re-ran the completed start step: that is a restart, not a resume"


@pytest.mark.parametrize("system", CONFIGS, indirect=True, ids=lambda c: "/".join(c))
def test_crash_at_first_write_resumes_from_scratch_and_charges_once(system):
    run = system.new_run()
    assert run.step(1)
    assert run.fired() == 0 or system.durability == "exit"
    while run.step(0):
        pass
    assert run.fired() == (2 if system.durability == "exit" else 1)


@pytest.mark.parametrize("system", CONFIGS, indirect=True, ids=lambda c: "/".join(c))
def test_crash_right_after_charge_refires(system):
    # The finding, pinned down: kill at the first durable write after the side effect
    # ran. The charge's result never persisted, so resume re-executes it. This holds in
    # every variant and durability mode.
    run = system.new_run()
    assert run.step_until(lambda events: run.fired() >= 1)
    while run.step(0):
        pass
    assert run.fired() == 2


@pytest.mark.parametrize("system", CONFIGS, indirect=True, ids=lambda c: "/".join(c))
def test_writes_match_a_clean_run(system):
    # As benchmarks/writes_test.go: a crash at write Writes() happens, one past it does not.
    w = system.writes()
    assert system.new_run().step(w), f"Writes()={w} but a crash at write {w} does not happen"
    assert not system.new_run().step(w + 1), f"Writes()={w} but a crash at write {w + 1} still happens"


@pytest.mark.parametrize("system", [("node", "sync")], indirect=True, ids=lambda c: "/".join(c))
def test_no_completed_run_skips_the_charge(system):
    assert verify("lg", system, 50).missed == 0


def test_writes_table_covers_every_config():
    assert set(WRITES) == set(CONFIGS)


def test_pcg_matches_go_math_rand_v2():
    # Reference: rand.New(rand.NewPCG(uint64(s)+1, 0x9E3779B97F4A7C15)).IntN(bound)+1 in Go 1.26.
    want = {
        (5, 0): "5 4 5 5 4 2 5 4 5 2 2 5",
        (5, 1): "5 1 3 2 3 3 4 4 5 5 4 5",
        (6, 2): "5 1 5 4 4 1 5 6 2 1 3 5",
        (7, 0): "7 6 7 7 5 3 7 5 6 3 3 7",
        (7, 1): "7 1 4 3 4 4 5 5 7 7 6 7",
    }
    for (bound, s), seq in want.items():
        rng = PCG(s + 1, 0x9E3779B97F4A7C15)
        assert " ".join(str(rng.intn(bound) + 1) for _ in range(12)) == seq
