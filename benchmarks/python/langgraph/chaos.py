"""A line-for-line port of bide's chaos.Verify (chaos/chaos.go) and of the Go
math/rand/v2 PCG source it seeds, so the Python harness runs the same crash
schedules as the Go harness and prints the same row format.
"""

from dataclasses import dataclass
from typing import Protocol

M64 = (1 << 64) - 1


class PCG:
    """Go's math/rand/v2 PCG (128-bit LCG, DXSM output) with Rand.IntN on top."""

    def __init__(self, seed1: int, seed2: int):
        self.hi = seed1 & M64
        self.lo = seed2 & M64

    def _next(self) -> tuple[int, int]:
        mul = (2549297995355413924 << 64) | 4865540595714422341
        inc = (6364136223846793005 << 64) | 1442695040888963407
        state = (((self.hi << 64) | self.lo) * mul + inc) & ((1 << 128) - 1)
        self.hi, self.lo = state >> 64, state & M64
        return self.hi, self.lo

    def uint64(self) -> int:
        hi, lo = self._next()
        hi ^= hi >> 32
        hi = (hi * 0xDA942042E4DD58B5) & M64
        hi ^= hi >> 48
        hi = (hi * (lo | 1)) & M64
        return hi

    def intn(self, n: int) -> int:
        if n <= 0:
            raise ValueError("invalid argument to intn")
        if n & (n - 1) == 0:
            return self.uint64() & (n - 1)
        prod = self.uint64() * n
        hi, lo = prod >> 64, prod & M64
        if lo < n:
            thresh = (-n & M64) % n
            while lo < thresh:
                prod = self.uint64() * n
                hi, lo = prod >> 64, prod & M64
        return hi


class Run(Protocol):
    def step(self, crash_at: int) -> bool: ...
    def fired(self) -> int: ...


class System(Protocol):
    def new_run(self) -> Run: ...
    def writes(self) -> int: ...


@dataclass
class Report:
    name: str
    sweeps: int = 0
    schedules: int = 0
    max_fired: int = 0
    violations: int = 0
    missed: int = 0

    def ok(self) -> bool:
        return self.violations == 0 and self.max_fired <= 1 and self.missed == 0

    def __str__(self) -> str:
        verdict = "PASS ✓ (at-most-once held)"
        if not self.ok():
            verdict = f"FAIL ✗ ({self.violations} double-fires, worst={self.max_fired})"
            if self.missed > 0:
                verdict = (
                    f"FAIL ✗ ({self.violations} double-fires, worst={self.max_fired}, "
                    f"{self.missed} completed without firing)"
                )
        return (
            f"{self.name:<16} sweeps={self.sweeps:<3d} schedules={self.schedules:<5d} "
            f"maxFired={self.max_fired}  {verdict}"
        )

    def record(self, fired: int, terminal: bool) -> None:
        if terminal and fired == 0:
            self.missed += 1
        self.max_fired = max(self.max_fired, fired)
        if fired > 1:
            self.violations += 1


def verify(name: str, sys: System, seeds: int) -> Report:
    """Same algorithm as chaos.Verify: one crash-free run, an exhaustive sweep of a
    single crash at every write point 1..Writes()+2 (then resume to terminal), and
    `seeds` randomized multi-crash schedules of up to 64 attempts each."""
    rep = Report(name)
    bound = sys.writes() + 2

    clean = sys.new_run()
    clean.step(0)
    rep.record(clean.fired(), True)

    for crash_at in range(1, bound + 1):
        run = sys.new_run()
        crashed = run.step(crash_at)
        while crashed:
            crashed = run.step(0)
        rep.sweeps += 1
        rep.schedules += 1
        rep.record(run.fired(), True)

    for s in range(seeds):
        rng = PCG(s + 1, 0x9E3779B97F4A7C15)
        run = sys.new_run()
        terminal = False
        attempt = 0
        while attempt < 64 and not terminal:
            terminal = not run.step(rng.intn(bound) + 1)
            attempt += 1
        rep.schedules += 1
        rep.record(run.fired(), terminal)
    return rep
