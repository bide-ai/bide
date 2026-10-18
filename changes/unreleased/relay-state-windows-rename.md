Section: Fixed

The relay's state file write retries on Windows when another process holds the file open: replacing `relay.state` fails with a sharing violation or access denied while a reader (an operator's `tail`, a virus scan) has it open, and the save now retries the rename for up to 300 ms before it fails. A save that still fails is no longer forgotten until the next event: the state stays pending and is written again after the flush delay (at least a second), or at `Close`. The test that polled the file for the flush no longer fails on the same transient open error.
