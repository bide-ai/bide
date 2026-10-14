Section: Fixed

`Agent.FlushNotifications` rechecks the events it deferred to a peer under one shared 2 s budget (or the caller's deadline if sooner), not 2 s per event, so a flush with no deadline and a hung trail no longer waits 2 s times the number of deferred events; the events it could not recheck are reported as they are.
