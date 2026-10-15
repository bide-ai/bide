Section: Fixed

`bide check -diff` (and `planview.CheckDiff`) now reports an `assert` line that the new config keeps as it was but that no longer holds as a weakening: a line that held in the old config and is now violated (with the counterexample path), cannot be decided, or no longer reads makes `-fail-on-weaken` exit 1 and the page read SAFETY CHECK WEAKENED. Before, only a removed or loosened line was reported, so a new arm around a gate left `every path to charge passes approval >= 2 of 3` as written and false, and the diff said nothing. A line that did not hold before is not reported.
