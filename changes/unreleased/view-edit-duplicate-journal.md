Section: Fixed

`bide view` and `bide edit` now refuse two `-journal` exports of the same run, as a usage error (exit 2) naming both files and the run ID. Before, the later file on the command line silently replaced the earlier one, so a substituted or stale export of a run could be shown as that run with no sign. Exports of different runs are unchanged. Two exports of one run taken at different times used to resolve to the later file; to look at one of them, pass only that file.
